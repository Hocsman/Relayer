package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Hocsman/Relayer/internal/agent"
)

func TestReplaceAgentsConcurrentCASAllowsExactlyOnePublisher(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load default config: %v", err)
	}

	const contenders = 64
	type result struct {
		id  string
		err error
	}
	start := make(chan struct{})
	results := make(chan result, contenders)
	var workers sync.WaitGroup
	workers.Add(contenders)
	for index := 0; index < contenders; index++ {
		id := fmt.Sprintf("candidate-%02d", index)
		// Equal-sized documents make contenders reach the final CAS window at
		// roughly the same time without relying on sleeps or production hooks.
		argument := strings.Repeat("x", 128<<10) + id
		go func() {
			defer workers.Done()
			<-start
			_, _, replaceErr := ReplaceAgents(path, loaded.Revision, []agent.Spec{{
				ID:      id,
				Name:    id,
				Command: []string{"runner", argument},
				Backend: agent.BackendPTY,
			}})
			results <- result{id: id, err: replaceErr}
		}()
	}
	close(start)
	workers.Wait()
	close(results)

	successes := make([]string, 0, 1)
	for outcome := range results {
		if outcome.err == nil {
			successes = append(successes, outcome.id)
			continue
		}
		if !errors.Is(outcome.err, ErrRevisionMismatch) {
			t.Fatalf("concurrent ReplaceAgents(%s) error = %v, want revision mismatch", outcome.id, outcome.err)
		}
	}
	if len(successes) != 1 {
		t.Fatalf("successful CAS publishers = %v, want exactly one", successes)
	}

	final, err := Load(path)
	if err != nil {
		t.Fatalf("Load final config: %v", err)
	}
	if len(final.Agents) != 1 || final.Agents[0].ID != successes[0] {
		t.Fatalf("final agents = %#v, want winning candidate %q", final.Agents, successes[0])
	}
}

func TestReplaceAgentsReturnsDefensiveCopies(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load default config: %v", err)
	}
	input := []agent.Spec{{
		ID:      "agent-a",
		Name:    "Agent A",
		Command: []string{"runner", "literal argument"},
		Cwd:     directory,
		Env:     map[string]string{"MODE": "fixture"},
		Adapter: agent.AdapterGeneric,
		Backend: agent.BackendPTY,
	}}
	updated, _, err := ReplaceAgents(path, loaded.Revision, input)
	if err != nil {
		t.Fatalf("ReplaceAgents: %v", err)
	}

	input[0].Command[1] = "mutated input"
	input[0].Env["MODE"] = "mutated input"
	if updated.Agents[0].Command[1] != "literal argument" || updated.Agents[0].Env["MODE"] != "fixture" {
		t.Fatalf("updated result aliases input: %#v", updated.Agents[0])
	}

	updated.Agents[0].Command[1] = "mutated result"
	updated.Agents[0].Env["MODE"] = "mutated result"
	fresh, err := Load(path)
	if err != nil {
		t.Fatalf("Load fresh config: %v", err)
	}
	if fresh.Agents[0].Command[1] != "literal argument" || fresh.Agents[0].Env["MODE"] != "fixture" {
		t.Fatalf("persisted config aliases returned result: %#v", fresh.Agents[0])
	}
}

func TestReplaceAgentsRevisionMatchesUpdatedStateAfterExternalPostCommitWrite(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load default config: %v", err)
	}

	originalSync := syncConfigurationDirectory
	syncConfigurationDirectory = func(string) error {
		payload, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		external := strings.Replace(string(payload), "id: published", "id: external", 1)
		if external == string(payload) {
			return errors.New("fixture could not locate published agent")
		}
		return os.WriteFile(path, []byte(external), 0o600)
	}
	t.Cleanup(func() { syncConfigurationDirectory = originalSync })

	updated, revision, err := ReplaceAgents(path, loaded.Revision, []agent.Spec{{
		ID: "published", Name: "Published", Command: []string{"runner"}, Backend: agent.BackendPTY,
	}})
	if err != nil {
		t.Fatalf("ReplaceAgents: %v", err)
	}
	if len(updated.Agents) != 1 || updated.Agents[0].ID != "external" {
		t.Fatalf("updated state did not observe post-commit write: %#v", updated.Agents)
	}
	if revision != updated.Revision {
		t.Fatalf("returned revision %q does not describe updated revision %q", revision, updated.Revision)
	}
}
