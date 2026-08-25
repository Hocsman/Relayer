// Package agent defines and validates the process specifications used by
// Relayer. It intentionally contains no process-launching logic.
package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	// BackendPTY is the interactive pseudo-terminal backend supported by the
	// current Relayer runtime.
	BackendPTY = "pty"

	// AdapterGeneric selects the built-in prompt adapter that works with any
	// command-line program.
	AdapterGeneric = "generic"
)

var environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Spec describes one agent process. Command contains an executable followed by
// its arguments, while Shell contains a script interpreted by the platform
// shell. Exactly one of Command and Shell must be configured.
type Spec struct {
	ID      string
	Name    string
	Command []string
	Shell   string
	Cwd     string
	Env     map[string]string
	Adapter string
	Backend string
}

// ValidateSpec validates and normalizes spec without retaining any of its
// mutable storage. Relative working directories are resolved from baseDir.
func ValidateSpec(spec Spec, baseDir, defaultBackend string) (Spec, error) {
	if err := rejectNUL("base directory", baseDir); err != nil {
		return Spec{}, err
	}
	if err := rejectNUL("default backend", defaultBackend); err != nil {
		return Spec{}, err
	}

	normalized := cloneSpec(spec)
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "id", value: normalized.ID},
		{name: "name", value: normalized.Name},
		{name: "shell", value: normalized.Shell},
		{name: "working directory", value: normalized.Cwd},
		{name: "adapter", value: normalized.Adapter},
		{name: "backend", value: normalized.Backend},
	} {
		if err := rejectNUL(field.name, field.value); err != nil {
			return Spec{}, err
		}
	}

	normalized.ID = strings.TrimSpace(normalized.ID)
	if normalized.ID == "" {
		return Spec{}, errors.New("agent id must not be blank")
	}
	normalized.Name = strings.TrimSpace(normalized.Name)
	if normalized.Name == "" {
		return Spec{}, errors.New("agent name must not be blank")
	}

	hasCommand := len(normalized.Command) > 0
	hasShell := strings.TrimSpace(normalized.Shell) != ""
	if hasCommand == hasShell {
		return Spec{}, errors.New("exactly one of command or shell must be configured")
	}
	for index, argument := range normalized.Command {
		if err := rejectNUL(fmt.Sprintf("command argument %d", index), argument); err != nil {
			return Spec{}, err
		}
	}
	if hasCommand && strings.TrimSpace(normalized.Command[0]) == "" {
		return Spec{}, errors.New("command executable must not be blank")
	}
	if !hasCommand {
		normalized.Command = nil
	}
	if !hasShell {
		normalized.Shell = ""
	}

	for name, value := range normalized.Env {
		if err := rejectNUL("environment variable name", name); err != nil {
			return Spec{}, err
		}
		if !environmentNamePattern.MatchString(name) {
			return Spec{}, fmt.Errorf("invalid environment variable name %q", name)
		}
		if err := rejectNUL(fmt.Sprintf("environment variable %s", name), value); err != nil {
			return Spec{}, err
		}
	}

	backend := strings.TrimSpace(normalized.Backend)
	if backend == "" {
		backend = strings.TrimSpace(defaultBackend)
	}
	if backend != BackendPTY {
		return Spec{}, fmt.Errorf("unsupported backend %q: only %q is available", backend, BackendPTY)
	}
	normalized.Backend = backend

	adapter := strings.TrimSpace(normalized.Adapter)
	if adapter == "" {
		adapter = AdapterGeneric
	}
	if adapter != AdapterGeneric {
		return Spec{}, fmt.Errorf("unsupported adapter %q: only %q is available", adapter, AdapterGeneric)
	}
	normalized.Adapter = adapter

	workingDirectory := normalized.Cwd
	if strings.TrimSpace(workingDirectory) == "" {
		normalized.Cwd = ""
		return normalized, nil
	}
	if !filepath.IsAbs(workingDirectory) {
		absoluteBase, err := filepath.Abs(baseDir)
		if err != nil {
			return Spec{}, fmt.Errorf("resolve base directory %q: %w", baseDir, err)
		}
		workingDirectory = filepath.Join(absoluteBase, workingDirectory)
	} else {
		workingDirectory = filepath.Clean(workingDirectory)
	}
	info, err := os.Stat(workingDirectory)
	if err != nil {
		return Spec{}, fmt.Errorf("inspect working directory %q: %w", workingDirectory, err)
	}
	if !info.IsDir() {
		return Spec{}, fmt.Errorf("working directory %q is not a directory", workingDirectory)
	}
	normalized.Cwd = workingDirectory
	return normalized, nil
}

// ValidateAll validates every specification and rejects identifiers that only
// differ by case. The returned slice and every mutable field are independent
// from the input.
func ValidateAll(specs []Spec, baseDir, defaultBackend string) ([]Spec, error) {
	validated := make([]Spec, 0, len(specs))
	for index, spec := range specs {
		normalized, err := ValidateSpec(spec, baseDir, defaultBackend)
		if err != nil {
			return nil, fmt.Errorf("agent %d: %w", index+1, err)
		}
		for previous, existing := range validated {
			if strings.EqualFold(existing.ID, normalized.ID) {
				return nil, fmt.Errorf("agent %d: id %q duplicates agent %d", index+1, normalized.ID, previous+1)
			}
		}
		validated = append(validated, normalized)
	}
	return validated, nil
}

// IsSensitiveEnvName reports whether an environment variable name commonly
// carries credentials. It is deliberately conservative because callers use
// this classification to prevent accidental display of values.
func IsSensitiveEnvName(name string) bool {
	compact := strings.NewReplacer("_", "", "-", "", ".", "").Replace(strings.ToUpper(strings.TrimSpace(name)))
	for _, marker := range []string{
		"PASSWORD",
		"PASSWD",
		"PASSPHRASE",
		"PASS",
		"SECRET",
		"TOKEN",
		"CREDENTIAL",
		"APIKEY",
		"PRIVATEKEY",
		"ACCESSKEY",
		"AUTHTOKEN",
		"AUTHORIZATION",
	} {
		if strings.Contains(compact, marker) {
			return true
		}
	}
	return false
}

func cloneSpec(spec Spec) Spec {
	cloned := spec
	if spec.Command != nil {
		cloned.Command = append([]string(nil), spec.Command...)
	}
	if spec.Env != nil {
		cloned.Env = make(map[string]string, len(spec.Env))
		for name, value := range spec.Env {
			cloned.Env[name] = value
		}
	}
	return cloned
}

func rejectNUL(field, value string) error {
	if strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("%s contains a NUL byte", field)
	}
	return nil
}
