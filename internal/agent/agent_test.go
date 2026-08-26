package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestValidateSpecNormalizesCommandAndMakesDeepCopies(t *testing.T) {
	baseDirectory := t.TempDir()
	workingDirectory := filepath.Join(baseDirectory, "project")
	if err := os.Mkdir(workingDirectory, 0o755); err != nil {
		t.Fatal(err)
	}

	input := Spec{
		ID:      "  primary  ",
		Name:    "  Primary Agent  ",
		Command: []string{"runner", "  argument with spaces  ", ""},
		Cwd:     "project",
		Env:     map[string]string{"MODEL": "local", "EMPTY": ""},
	}
	wantCommand := append([]string(nil), input.Command...)
	wantEnvironment := map[string]string{"MODEL": "local", "EMPTY": ""}

	got, err := ValidateSpec(input, baseDirectory, BackendPTY)
	if err != nil {
		t.Fatalf("ValidateSpec: %v", err)
	}
	if got.ID != "primary" || got.Name != "Primary Agent" {
		t.Fatalf("identifiers were not normalized: %#v", got)
	}
	if !reflect.DeepEqual(got.Command, wantCommand) {
		t.Fatalf("command arguments changed: got %#v, want %#v", got.Command, wantCommand)
	}
	if !reflect.DeepEqual(got.Env, wantEnvironment) {
		t.Fatalf("environment changed: got %#v, want %#v", got.Env, wantEnvironment)
	}
	if got.Cwd != workingDirectory {
		t.Fatalf("relative cwd resolved to %q, want %q", got.Cwd, workingDirectory)
	}
	if got.Backend != BackendPTY || got.Adapter != AdapterGeneric {
		t.Fatalf("defaults not applied: backend=%q adapter=%q", got.Backend, got.Adapter)
	}

	input.Command[1] = "mutated input"
	input.Env["MODEL"] = "mutated input"
	if got.Command[1] != wantCommand[1] || got.Env["MODEL"] != "local" {
		t.Fatal("validated spec aliases mutable input storage")
	}
	got.Command[0] = "mutated output"
	got.Env["EMPTY"] = "mutated output"
	if input.Command[0] != "runner" || input.Env["EMPTY"] != "" {
		t.Fatal("input spec aliases mutable validated storage")
	}
}

func TestValidateSpecPreservesShellAndAcceptsAbsoluteCwd(t *testing.T) {
	baseDirectory := t.TempDir()
	script := "  printf '%s' 'kept verbatim'  "
	got, err := ValidateSpec(Spec{
		ID:      "shell",
		Name:    "Shell agent",
		Shell:   script,
		Cwd:     baseDirectory,
		Backend: BackendPTY,
		Adapter: AdapterGeneric,
	}, t.TempDir(), "ignored")
	if err != nil {
		t.Fatalf("ValidateSpec: %v", err)
	}
	if got.Shell != script {
		t.Fatalf("shell changed: got %q, want %q", got.Shell, script)
	}
	if got.Cwd != baseDirectory {
		t.Fatalf("absolute cwd changed: got %q, want %q", got.Cwd, baseDirectory)
	}
}

func TestValidateSpecAcceptsAndNormalizesEveryBackendSelector(t *testing.T) {
	for _, backend := range []string{BackendPTY, BackendTmux, BackendAuto} {
		t.Run("default "+backend, func(t *testing.T) {
			got, err := ValidateSpec(validCommandSpec(), t.TempDir(), "  "+backend+"  ")
			if err != nil {
				t.Fatalf("ValidateSpec default backend %q: %v", backend, err)
			}
			if got.Backend != backend {
				t.Fatalf("normalized default backend = %q, want %q", got.Backend, backend)
			}
		})

		t.Run("explicit "+backend, func(t *testing.T) {
			spec := validCommandSpec()
			spec.Backend = "  " + backend + "  "
			got, err := ValidateSpec(spec, t.TempDir(), BackendPTY)
			if err != nil {
				t.Fatalf("ValidateSpec explicit backend %q: %v", backend, err)
			}
			if got.Backend != backend {
				t.Fatalf("normalized explicit backend = %q, want %q", got.Backend, backend)
			}
		})
	}
}

func TestValidateSpecRejectsInvalidCoreFields(t *testing.T) {
	valid := validCommandSpec()
	tests := []struct {
		name   string
		mutate func(*Spec)
	}{
		{name: "blank id", mutate: func(spec *Spec) { spec.ID = " \t " }},
		{name: "blank name", mutate: func(spec *Spec) { spec.Name = "\n" }},
		{name: "neither command nor shell", mutate: func(spec *Spec) { spec.Command = nil }},
		{name: "command and shell", mutate: func(spec *Spec) { spec.Shell = "echo duplicate" }},
		{name: "blank command executable", mutate: func(spec *Spec) { spec.Command[0] = "  " }},
		{name: "unsupported backend", mutate: func(spec *Spec) { spec.Backend = "pipe" }},
		{name: "unsupported adapter", mutate: func(spec *Spec) { spec.Adapter = "claude" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := cloneSpec(valid)
			test.mutate(&spec)
			if _, err := ValidateSpec(spec, t.TempDir(), BackendPTY); err == nil {
				t.Fatal("ValidateSpec unexpectedly succeeded")
			}
		})
	}
}

func TestValidateSpecRejectsNULBytes(t *testing.T) {
	tests := []struct {
		name           string
		mutate         func(*Spec)
		baseDirectory  string
		defaultBackend string
	}{
		{name: "id", mutate: func(spec *Spec) { spec.ID = "agent\x00id" }},
		{name: "name", mutate: func(spec *Spec) { spec.Name = "agent\x00name" }},
		{name: "command executable", mutate: func(spec *Spec) { spec.Command[0] = "run\x00ner" }},
		{name: "command argument", mutate: func(spec *Spec) { spec.Command = append(spec.Command, "arg\x00value") }},
		{name: "shell", mutate: func(spec *Spec) { spec.Command = nil; spec.Shell = "echo\x00bad" }},
		{name: "cwd", mutate: func(spec *Spec) { spec.Cwd = "work\x00dir" }},
		{name: "environment name", mutate: func(spec *Spec) { spec.Env = map[string]string{"BAD\x00NAME": "value"} }},
		{name: "environment value", mutate: func(spec *Spec) { spec.Env = map[string]string{"TOKEN": "value\x00suffix"} }},
		{name: "backend", mutate: func(spec *Spec) { spec.Backend = "pty\x00" }},
		{name: "adapter", mutate: func(spec *Spec) { spec.Adapter = "generic\x00" }},
		{name: "base directory", baseDirectory: "base\x00dir"},
		{name: "default backend", defaultBackend: "pty\x00"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := validCommandSpec()
			if test.mutate != nil {
				test.mutate(&spec)
			}
			baseDirectory := test.baseDirectory
			if baseDirectory == "" {
				baseDirectory = t.TempDir()
			}
			defaultBackend := test.defaultBackend
			if defaultBackend == "" {
				defaultBackend = BackendPTY
			}
			if _, err := ValidateSpec(spec, baseDirectory, defaultBackend); err == nil || !strings.Contains(err.Error(), "NUL") {
				t.Fatalf("ValidateSpec error = %v, want a NUL error", err)
			}
		})
	}
}

func TestValidateSpecRejectsInvalidEnvironmentNames(t *testing.T) {
	for _, name := range []string{"", "1TOKEN", "BAD-NAME", "WITH=VALUE", "HAS SPACE"} {
		t.Run(name, func(t *testing.T) {
			spec := validCommandSpec()
			spec.Env = map[string]string{name: "value"}
			if _, err := ValidateSpec(spec, t.TempDir(), BackendPTY); err == nil {
				t.Fatalf("ValidateSpec accepted environment name %q", name)
			}
		})
	}

	for _, name := range []string{"PATH", "_PRIVATE", "MODEL_2"} {
		t.Run("valid "+name, func(t *testing.T) {
			spec := validCommandSpec()
			spec.Env = map[string]string{name: "value"}
			if _, err := ValidateSpec(spec, t.TempDir(), BackendPTY); err != nil {
				t.Fatalf("ValidateSpec rejected environment name %q: %v", name, err)
			}
		})
	}
}

func TestValidateSpecChecksWorkingDirectory(t *testing.T) {
	baseDirectory := t.TempDir()
	filePath := filepath.Join(baseDirectory, "regular-file")
	if err := os.WriteFile(filePath, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, cwd := range []string{"missing", filePath} {
		t.Run(cwd, func(t *testing.T) {
			spec := validCommandSpec()
			spec.Cwd = cwd
			if _, err := ValidateSpec(spec, baseDirectory, BackendPTY); err == nil {
				t.Fatalf("ValidateSpec accepted invalid cwd %q", cwd)
			}
		})
	}
}

func TestValidateAllRejectsCaseInsensitiveDuplicateIDs(t *testing.T) {
	specs := []Spec{
		validCommandSpec(),
		{ID: " AGENT ", Name: "Second", Shell: "echo second"},
	}
	if _, err := ValidateAll(specs, t.TempDir(), BackendPTY); err == nil || !strings.Contains(err.Error(), "duplicates") {
		t.Fatalf("ValidateAll error = %v, want duplicate id error", err)
	}
}

func TestValidateAllReturnsIndependentSpecsInOrder(t *testing.T) {
	input := []Spec{
		validCommandSpec(),
		{ID: "second", Name: "Second", Shell: "echo second", Env: map[string]string{"MODE": "test"}},
	}
	got, err := ValidateAll(input, t.TempDir(), BackendPTY)
	if err != nil {
		t.Fatalf("ValidateAll: %v", err)
	}
	if len(got) != 2 || got[0].ID != "agent" || got[1].ID != "second" {
		t.Fatalf("order or ids changed: %#v", got)
	}
	input[0].Command[0] = "mutated"
	input[1].Env["MODE"] = "mutated"
	if got[0].Command[0] != "runner" || got[1].Env["MODE"] != "test" {
		t.Fatal("ValidateAll output aliases input storage")
	}
}

func TestIsSensitiveEnvName(t *testing.T) {
	tests := map[string]bool{
		"PATH":                  false,
		"MODEL_NAME":            false,
		"DATABASE_PASSWORD":     true,
		"USER_PASS":             true,
		"GITHUB_TOKEN":          true,
		"OPENAI_API_KEY":        true,
		"AWS_SECRET_ACCESS_KEY": true,
		"ssh-private-key":       true,
		"Authorization":         true,
	}
	for name, want := range tests {
		t.Run(name, func(t *testing.T) {
			if got := IsSensitiveEnvName(name); got != want {
				t.Fatalf("IsSensitiveEnvName(%q) = %t, want %t", name, got, want)
			}
		})
	}
}

func validCommandSpec() Spec {
	return Spec{
		ID:      "agent",
		Name:    "Agent",
		Command: []string{"runner", "--flag"},
		Env:     map[string]string{"MODE": "test"},
	}
}
