package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/agent"
)

// The agent update path must never materialize a configuration.
//
// Load creates a default file when the path is absent. That is correct for
// first-run bootstrap and wrong everywhere else: used to validate the temporary
// file of an in-flight save, it would write a full default configuration there
// and the save would then rename it over the user's real one, replacing their
// agents, policies and audit settings while reporting success.
func TestAgentUpdateEntryPointsNeverCreateAConfiguration(t *testing.T) {
	specs := []agent.Spec{{
		ID: "a", Name: "A", Command: []string{"runner"},
		Adapter: agent.AdapterGeneric, Backend: agent.BackendPTY,
	}}

	for _, test := range []struct {
		name string
		call func(path string) error
	}{
		{name: "ReplaceAgents", call: func(path string) error {
			_, _, err := ReplaceAgents(path, "any-revision", specs)
			return err
		}},
		{name: "FileRevision", call: func(path string) error {
			_, err := FileRevision(path)
			return err
		}},
		{name: "CaptureFileSnapshot", call: func(path string) error {
			_, err := CaptureFileSnapshot(path)
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "config.yaml")

			if err := test.call(path); err == nil {
				t.Fatal("absent configuration accepted")
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a configuration was created at the target path: %v", err)
			}

			// The inter-process update lock is a deliberate artifact; nothing
			// else may be left behind, and in particular no rendered
			// configuration under a temporary name.
			entries, err := os.ReadDir(directory)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasSuffix(entry.Name(), ".lock") {
					continue
				}
				t.Fatalf("update path left %q behind", entry.Name())
			}
		})
	}
}
