package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// yamlBlocks returns the fenced YAML examples of a Markdown document.
func yamlBlocks(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	fence := regexp.MustCompile("(?s)```yaml\n(.*?)```")
	var blocks []string
	for _, match := range fence.FindAllStringSubmatch(text, -1) {
		blocks = append(blocks, match[1])
	}
	return blocks
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test directory")
		}
		dir = parent
	}
}

var topLevelKey = regexp.MustCompile(`(?m)^([a-z_]+):`)

// TestConfigurationReferenceExamplesLoad loads every YAML example in
// docs/configuration.md with the strict loader. The reference used keys the
// loader rejects — os_notifications, terminal_bell, type, workspace_only,
// interval — so a user who copied an example got a configuration that would
// not load. A fragment is completed with the required top-level fields it does
// not set itself.
func TestConfigurationReferenceExamplesLoad(t *testing.T) {
	checkExamplesLoad(t, filepath.Join("docs", "configuration.md"), 5)
}

// TestEveryDocumentedConfigurationLoads extends the check to every other page
// with a configuration example. v0.8.6 fixed the reference but not README's
// "every top-level section" example, which used the same rejected keys.
func TestEveryDocumentedConfigurationLoads(t *testing.T) {
	for _, page := range []struct {
		path    string
		minimum int
	}{
		{"README.md", 1},
		{filepath.Join("docs", "recording.md"), 1},
		{filepath.Join("docs", "audit.md"), 0},
		{filepath.Join("docs", "observability.md"), 0},
		{filepath.Join("docs", "troubleshooting.md"), 0},
	} {
		t.Run(page.path, func(t *testing.T) { checkExamplesLoad(t, page.path, page.minimum) })
	}
}

// checkExamplesLoad loads the top-level YAML examples of one page and fails
// unless at least minimum of them were checked.
func checkExamplesLoad(t *testing.T, page string, minimum int) {
	t.Helper()
	blocks := yamlBlocks(t, filepath.Join(repositoryRoot(t), page))
	if len(blocks) == 0 {
		t.Fatalf("%s has no YAML example", page)
	}
	skeleton := map[string]string{
		"version":            "version: 1\n",
		"backend":            "backend: pty\n",
		"agents":             "agents: []\n",
		"intercept_patterns": "intercept_patterns:\n  - pattern: '(?i)continue'\n    description: continue prompt\n",
	}
	topLevel := map[string]bool{
		"version": true, "backend": true, "sessions": true, "policies": true, "audit": true, "agents": true,
		"intercept_patterns": true, "notifications": true, "telemetry": true, "recording": true,
	}
	checked := 0
	for index, block := range blocks {
		// A fragment of a nested structure — one agent's fields, one list
		// item — is not a document; only examples that start at the top
		// level can be loaded.
		if first := topLevelKey.FindStringSubmatch(strings.TrimSpace(block)); first == nil || !topLevel[first[1]] || !strings.HasPrefix(strings.TrimSpace(block), first[0]) {
			t.Logf("example %d is a nested fragment; not loaded", index+1)
			continue
		}
		checked++
		present := map[string]bool{}
		for _, match := range topLevelKey.FindAllStringSubmatch(block, -1) {
			present[match[1]] = true
		}
		document := block
		for _, key := range []string{"version", "backend", "agents", "intercept_patterns"} {
			if !present[key] {
				document = skeleton[key] + document
			}
		}

		dir := t.TempDir()
		// The agents example names a relative working directory.
		if err := os.MkdirAll(filepath.Join(dir, "workspace"), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		path := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := LoadExisting(path); err != nil {
			first := strings.SplitN(strings.TrimSpace(block), "\n", 2)[0]
			t.Errorf("example %d (starting %q) does not load: %v", index+1, first, err)
		}
	}
	if checked < minimum {
		t.Fatalf("only %d top-level examples of %s were checked, want at least %d; the page lost its examples or the filter is wrong", checked, page, minimum)
	}
}
