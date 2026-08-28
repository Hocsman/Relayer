package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/intercept"
)

var defaultPromptPatterns = DefaultPatterns()

func loadPromptPatterns(path string) ([]intercept.Pattern, bool, error) {
	result, err := LoadOrCreate(path)
	return result.Patterns, result.Created, err
}

func TestLoadPromptPatternsCreatesAndReloadsDefaultConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.yaml")

	patterns, created, err := loadPromptPatterns(path)
	if err != nil {
		t.Fatalf("loadPromptPatterns returned an error: %v", err)
	}
	if !created {
		t.Fatal("loadPromptPatterns did not report creating the missing config")
	}
	if !reflect.DeepEqual(patterns, defaultPromptPatterns) {
		t.Fatalf("generated patterns = %#v, want %#v", patterns, defaultPromptPatterns)
	}

	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading generated config: %v", err)
	}
	if !strings.Contains(string(payload), "intercept_patterns:") {
		t.Fatalf("generated config has no intercept_patterns wrapper:\n%s", payload)
	}
	for _, required := range []string{"version: 1\n", "backend: pty\n", "agents: []\n"} {
		if !strings.Contains(string(payload), required) {
			t.Fatalf("generated config has no %q field:\n%s", strings.TrimSpace(required), payload)
		}
	}
	if !strings.HasPrefix(string(payload), "# Configuration de Relayer; agents: [] active les deux mocks.\n") {
		t.Fatalf("generated config has no explanatory header:\n%s", payload)
	}

	reloaded, createdAgain, err := loadPromptPatterns(path)
	if err != nil {
		t.Fatalf("reloading generated config: %v", err)
	}
	if createdAgain {
		t.Fatal("existing generated config was reported as newly created")
	}
	if !reflect.DeepEqual(reloaded, defaultPromptPatterns) {
		t.Fatalf("reloaded patterns = %#v, want %#v", reloaded, defaultPromptPatterns)
	}

	patterns[0].Description = "mutated by test"
	if defaultPromptPatterns[0].Description == "mutated by test" {
		t.Fatal("loaded patterns alias the built-in defaults")
	}
}

func TestLoadExistingNeverCreatesMissingConfiguration(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "nested", "config.yaml")

	result, err := LoadExisting(path)
	if err == nil {
		t.Fatalf("LoadExisting returned %#v for a missing file", result)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LoadExisting error = %v, want os.ErrNotExist", err)
	}
	if !errors.Is(err, ErrExistingConfigRead) {
		t.Fatalf("LoadExisting error = %v, want ErrExistingConfigRead", err)
	}
	var readError *ExistingConfigReadError
	if !errors.As(err, &readError) {
		t.Fatalf("LoadExisting error type = %T, want *ExistingConfigReadError", err)
	}
	if strings.Contains(err.Error(), path) || strings.Contains(err.Error(), "nested") {
		t.Fatalf("LoadExisting rendered the selected path: %q", err)
	}
	if cause := errors.Unwrap(err); cause == nil || strings.Contains(cause.Error(), path) || strings.Contains(cause.Error(), "nested") {
		t.Fatalf("LoadExisting retained an unsafe rendered cause: %v", cause)
	}
	if _, statErr := os.Lstat(filepath.Dir(path)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("LoadExisting created the parent directory: %v", statErr)
	}
}

func TestLoadExistingValidationPathFailureIsNotASelectedFileReadFailure(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	missingCWD := "missing-agent-working-directory"
	content := "version: 1\n" +
		"backend: pty\n" +
		"agents:\n" +
		"  - id: cwd-validation\n" +
		"    name: CWD validation\n" +
		"    command: [runner]\n" +
		"    cwd: " + missingCWD + "\n" +
		"intercept_patterns:\n" +
		"  - pattern: continue\n" +
		"    description: Continue\n"
	writeConfigTestFile(t, path, []byte(content))

	_, err := LoadExisting(path)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LoadExisting error = %v, want validation os.ErrNotExist", err)
	}
	if errors.Is(err, ErrExistingConfigRead) {
		t.Fatalf("validation failure was classified as selected-file read failure: %v", err)
	}
	var readError *ExistingConfigReadError
	if errors.As(err, &readError) {
		t.Fatalf("validation failure has read error type: %T", err)
	}
}

func TestLoadExistingMatchesLoadWithoutMutatingExistingConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := []byte("- pattern: '(?i)read only check'\n  description: Read-only check\n")
	writeConfigTestFile(t, path, original)

	readOnly, err := LoadExisting(path)
	if err != nil {
		t.Fatalf("LoadExisting: %v", err)
	}
	regular, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if readOnly.Created || regular.Created || !reflect.DeepEqual(readOnly, regular) {
		t.Fatalf("read-only result = %#v, regular result = %#v", readOnly, regular)
	}
	assertConfigFileBytes(t, path, original)
}

func TestCreateDefaultConfigDoesNotOverwriteExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := []byte("# user formatting must survive\n- pattern: '(?i)custom gate'\n  description: Custom gate\n")
	writeConfigTestFile(t, path, original)

	created, err := createDefault(path)
	if err != nil {
		t.Fatalf("createDefaultConfig returned an error for an existing file: %v", err)
	}
	if created {
		t.Fatal("createDefaultConfig reported overwriting an existing file")
	}
	assertConfigFileBytes(t, path, original)

	patterns, loadCreated, err := loadPromptPatterns(path)
	if err != nil {
		t.Fatalf("loading preserved user config: %v", err)
	}
	if loadCreated {
		t.Fatal("loadPromptPatterns reported an existing config as created")
	}
	if len(patterns) != 1 || patterns[0].Expression != `(?i)custom gate` {
		t.Fatalf("loaded patterns = %#v", patterns)
	}
	assertConfigFileBytes(t, path, original)
}

func TestExclusiveConfigFallbackCreatesWithoutOverwriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	first := []byte("- pattern: first\n  description: First\n")
	created, err := createExclusively(path, first, errors.New("hard links unsupported"))
	if err != nil {
		t.Fatalf("createConfigExclusively returned an error: %v", err)
	}
	if !created {
		t.Fatal("exclusive fallback did not create a missing file")
	}
	assertConfigFileBytes(t, path, first)

	created, err = createExclusively(
		path,
		[]byte("- pattern: second\n  description: Second\n"),
		errors.New("hard links unsupported"),
	)
	if err != nil {
		t.Fatalf("second createConfigExclusively returned an error: %v", err)
	}
	if created {
		t.Fatal("exclusive fallback overwrote an existing file")
	}
	assertConfigFileBytes(t, path, first)
}

func TestLoadPromptPatternsAcceptsBothDocumentShapes(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{
			name: "direct list",
			content: "- pattern: '(?i)do the custom thing'\n" +
				"  description: Custom action\n",
		},
		{
			name: "intercept_patterns wrapper",
			// Keep the indentation-less sequence and blank line used by the README.
			content: "intercept_patterns:\n\n" +
				"- pattern: '(?i)do the custom thing'\n" +
				"  description: Custom action\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			writeConfigTestFile(t, path, []byte(test.content))

			patterns, created, err := loadPromptPatterns(path)
			if err != nil {
				t.Fatalf("loadPromptPatterns returned an error: %v", err)
			}
			if created {
				t.Fatal("existing config was reported as created")
			}
			want := []intercept.Pattern{{
				Name:        "config-1",
				Description: "Custom action",
				Expression:  `(?i)do the custom thing`,
			}}
			if !reflect.DeepEqual(patterns, want) {
				t.Fatalf("patterns = %#v, want %#v", patterns, want)
			}
		})
	}
}

func TestLoadPromptPatternsRejectsNonStrictOrMalformedYAML(t *testing.T) {
	tests := []struct {
		name        string
		content     string
		wantMessage string
	}{
		{
			name: "unknown wrapper field",
			content: "intercept_pattern:\n" +
				"  - pattern: 'ok'\n" +
				"    description: Typo\n",
		},
		{
			name: "unknown entry field",
			content: "- pattern: 'ok'\n" +
				"  description: Known fields only\n" +
				"  sensitive: true\n",
		},
		{
			name: "mapping pretending to be one entry",
			content: "pattern: 'ok'\n" +
				"description: Wrong root shape\n",
		},
		{
			name:    "scalar root",
			content: "just text\n",
		},
		{
			name: "wrong field type",
			content: "- pattern: ['not', 'a', 'scalar']\n" +
				"  description: Wrong type\n",
		},
		{
			name: "boolean pattern is not coerced",
			content: "- pattern: true\n" +
				"  description: Wrong scalar type\n",
		},
		{
			name: "numeric description is not coerced",
			content: "- pattern: 'valid'\n" +
				"  description: 123\n",
		},
		{
			name:    "malformed syntax",
			content: "intercept_patterns: [\n",
		},
		{
			name: "duplicate key",
			content: "- pattern: 'first'\n" +
				"  pattern: 'second'\n" +
				"  description: Duplicate\n",
		},
		{
			name: "multiple documents",
			content: "- pattern: 'first'\n" +
				"  description: First\n" +
				"---\n" +
				"- pattern: 'second'\n" +
				"  description: Second\n",
			wantMessage: "plusieurs documents YAML",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			original := []byte(test.content)
			writeConfigTestFile(t, path, original)

			patterns, created, err := loadPromptPatterns(path)
			if err == nil {
				t.Fatalf("loadPromptPatterns accepted invalid YAML and returned %#v", patterns)
			}
			if created {
				t.Fatal("invalid existing config was reported as created")
			}
			if test.wantMessage != "" && !strings.Contains(err.Error(), test.wantMessage) {
				t.Fatalf("error %q does not contain %q", err, test.wantMessage)
			}
			assertConfigFileBytes(t, path, original)
		})
	}
}

func TestLoadPromptPatternsRejectsMissingRequiredFields(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{name: "empty direct list", content: "[]\n"},
		{name: "empty wrapped list", content: "intercept_patterns: []\n"},
		{name: "missing wrapped list", content: "{}\n"},
		{name: "null wrapped list", content: "intercept_patterns:\n"},
		{name: "null entry", content: "- null\n"},
		{
			name:    "missing pattern",
			content: "- description: Missing pattern\n",
		},
		{
			name: "empty pattern",
			content: "- pattern: ''\n" +
				"  description: Empty pattern\n",
		},
		{
			name: "blank pattern",
			content: "- pattern: '   '\n" +
				"  description: Blank pattern\n",
		},
		{
			name:    "missing description",
			content: "- pattern: 'valid'\n",
		},
		{
			name: "empty description",
			content: "- pattern: 'valid'\n" +
				"  description: ''\n",
		},
		{
			name: "blank description",
			content: "- pattern: 'valid'\n" +
				"  description: '   '\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			original := []byte(test.content)
			writeConfigTestFile(t, path, original)

			patterns, created, err := loadPromptPatterns(path)
			if err == nil {
				t.Fatalf("loadPromptPatterns accepted an incomplete config and returned %#v", patterns)
			}
			if created {
				t.Fatal("incomplete existing config was reported as created")
			}
			assertConfigFileBytes(t, path, original)
		})
	}
}

func TestLoadPromptPatternsRejectsInvalidRegexWithoutReplacingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := []byte("- pattern: '('\n  description: Broken regex\n")
	writeConfigTestFile(t, path, original)

	patterns, created, err := loadPromptPatterns(path)
	if err == nil {
		t.Fatalf("loadPromptPatterns accepted an invalid regex and returned %#v", patterns)
	}
	if created {
		t.Fatal("invalid existing config was reported as created")
	}
	if !strings.Contains(err.Error(), "regex invalide") {
		t.Fatalf("error %q does not identify the invalid regex", err)
	}
	assertConfigFileBytes(t, path, original)
}

func TestLoadPromptPatternsMarksPasswordPatternSensitive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeConfigTestFile(t, path, []byte(
		"intercept_patterns:\n"+
			"  - pattern: '(?i)enter passphrase:'\n"+
			"    description: Credential required\n",
	))

	patterns, _, err := loadPromptPatterns(path)
	if err != nil {
		t.Fatalf("loadPromptPatterns returned an error: %v", err)
	}
	if len(patterns) != 1 {
		t.Fatalf("got %d patterns, want 1", len(patterns))
	}
	if patterns[0].Name != "password" {
		t.Fatalf("password pattern name = %q", patterns[0].Name)
	}
	if !patterns[0].Sensitive {
		t.Fatal("password/passphrase pattern was not marked sensitive")
	}
}

func TestLoadedConfigPatternReachesInterceptor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeConfigTestFile(t, path, []byte(
		"- pattern: '(?i)magic approval\\?$'\n"+
			"  description: Custom gate\n",
	))

	patterns, _, err := loadPromptPatterns(path)
	if err != nil {
		t.Fatalf("loadPromptPatterns returned an error: %v", err)
	}
	if len(patterns) != 1 {
		t.Fatalf("got %d patterns, want 1", len(patterns))
	}

	var detected *intercept.Detection
	engine, err := intercept.New(
		patterns,
		128,
		intercept.Hooks{OnPrompt: func(prompt intercept.Detection) {
			promptCopy := prompt
			detected = &promptCopy
		}},
	)
	if err != nil {
		t.Fatalf("intercept.New returned an error: %v", err)
	}
	engine.Consume([]byte("MAGIC "))
	engine.Consume([]byte("APPROVAL?"))

	if detected == nil {
		t.Fatal("loaded custom pattern did not reach the interceptor")
	}
	if detected.Pattern != "config-1" {
		t.Fatalf("unexpected prompt identity: %#v", *detected)
	}
	if detected.Description != "Custom gate" || detected.Match != "MAGIC APPROVAL?" {
		t.Fatalf("unexpected prompt payload: %#v", *detected)
	}
	if detected.Sensitive {
		t.Fatalf("non-sensitive custom pattern was marked sensitive: %#v", *detected)
	}
}

func TestInterceptorMasksSensitiveMatchEvenWhenRegexIsObfuscated(t *testing.T) {
	patterns, err := validate([]ConfigPattern{{
		Pattern:     `(?i)p[a]ssword:`,
		Description: "Authentication gate",
	}})
	if err != nil {
		t.Fatalf("validateConfigPatterns returned an error: %v", err)
	}
	if patterns[0].Sensitive {
		t.Fatal("test precondition failed: obfuscated expression was inferred as sensitive")
	}

	var detected intercept.Detection
	engine, err := intercept.New(
		patterns,
		128,
		intercept.Hooks{OnPrompt: func(prompt intercept.Detection) {
			detected = prompt
		}},
	)
	if err != nil {
		t.Fatalf("intercept.New returned an error: %v", err)
	}
	engine.Consume([]byte("Password:"))
	if !detected.Sensitive {
		t.Fatal("a password prompt match was not marked sensitive")
	}
}

func TestInterceptorMasksCommonCredentialPrompts(t *testing.T) {
	for _, prompt := range []string{"API key:", "Access token:", "PIN:", "OTP:"} {
		t.Run(prompt, func(t *testing.T) {
			if !intercept.IsSensitiveText(prompt) {
				t.Fatalf("credential prompt %q was not classified as sensitive", prompt)
			}
		})
	}
}

func writeConfigTestFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
}

func assertConfigFileBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading test config: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("config file was modified:\ngot:  %q\nwant: %q", got, want)
	}
}
