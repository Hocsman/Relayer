package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/config"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/terminal"
	"github.com/Hocsman/Relayer/internal/tmuxbackend"
)

type appAuditSink struct {
	mu       sync.Mutex
	lines    [][]byte
	writes   int
	failFrom int
	closed   bool
}

func (s *appAuditSink) WriteLine(line []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes++
	if s.failFrom > 0 && s.writes >= s.failFrom {
		return errors.New("planned audit disk failure")
	}
	s.lines = append(s.lines, append([]byte(nil), line...))
	return nil
}

func (s *appAuditSink) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}

func (s *appAuditSink) entries(t *testing.T) []audit.Entry {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]audit.Entry, len(s.lines))
	for index, line := range s.lines {
		if err := json.Unmarshal(line, &result[index]); err != nil {
			t.Fatalf("decode audit line %d: %v", index, err)
		}
	}
	return result
}

type failSecondStartBackend struct {
	*routerFakeBackend
	failure error
}

func (b *failSecondStartBackend) Start(ctx context.Context, spec agent.Spec, size terminal.Size) (terminal.Info, error) {
	b.mu.Lock()
	call := len(b.starts) + 1
	b.mu.Unlock()
	if call == 2 {
		b.mu.Lock()
		b.starts = append(b.starts, routerStartCall{spec: spec, size: size})
		b.mu.Unlock()
		return terminal.Info{}, b.failure
	}
	return b.routerFakeBackend.Start(ctx, spec, size)
}

func TestRunAuditsLifecycleInOrderWithoutEnvironmentValues(t *testing.T) {
	secret := "audit-environment-secret-sentinel"
	commandSecret := "audit-command-secret-sentinel"
	shellSecret := "audit-shell-secret-sentinel"
	nameSecret := "audit-name-secret-sentinel"
	backendErrorSecret := "audit-backend-error-secret-sentinel"
	path := writeAuditAppConfig(t, secret)
	sink := &appAuditSink{}
	recorder := newAppAuditRecorder(t, sink)
	backend := &failSecondStartBackend{
		routerFakeBackend: newRouterFakeBackend(agent.BackendPTY),
		failure:           errors.New(backendErrorSecret),
	}
	err := run([]string{"--config", path}, io.Discard, backendDependencies{
		newAudit: func(audit.Config) (*audit.Recorder, error) { return recorder, nil },
		newPTY: func(context.Context, chan<- session.Event, *adapters.Registry, int) (terminal.Backend, error) {
			return backend, nil
		},
		newTmux: func(context.Context, chan<- session.Event, *adapters.Registry, int, tmuxbackend.Options) (terminal.Backend, error) {
			t.Fatal("tmux factory called")
			return nil, nil
		},
	})
	if err == nil || !errors.Is(err, backend.failure) {
		t.Fatalf("run error = %v", err)
	}

	entries := sink.entries(t)
	wantKinds := []audit.Kind{
		audit.KindRunStarted,
		audit.KindSessionStarted,
		audit.KindBackendError,
		audit.KindSupervisionFinished,
		audit.KindSessionCleanup,
		audit.KindRunFinished,
	}
	if len(entries) != len(wantKinds) {
		t.Fatalf("audit entries = %#v", entries)
	}
	for index, want := range wantKinds {
		if entries[index].Kind != want || entries[index].Sequence != uint64(index+1) {
			t.Fatalf("entry %d = %#v, want kind %s sequence %d", index, entries[index], want, index+1)
		}
	}
	if entries[1].SessionID != "first" || entries[1].Backend != agent.BackendPTY ||
		entries[3].SessionID != "first" || entries[3].Outcome != audit.OutcomeFinished ||
		entries[4].SessionID != "first" || entries[4].Outcome != audit.OutcomeSucceeded ||
		entries[4].Reason != "backend_cleanup_completed" ||
		entries[5].Outcome != audit.OutcomeFailed {
		t.Fatalf("lifecycle metadata = %#v", entries)
	}
	sink.mu.Lock()
	raw := string(joinAuditLines(sink.lines))
	closed := sink.closed
	sink.mu.Unlock()
	for _, forbidden := range []string{secret, commandSecret, shellSecret, nameSecret, backendErrorSecret} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("forbidden process/config value %q leaked into audit: %s", forbidden, raw)
		}
	}
	if !closed {
		t.Fatal("audit sink was not closed")
	}
	backend.mu.Lock()
	closeCalls := backend.closeCalls
	backend.mu.Unlock()
	if closeCalls != 1 {
		t.Fatalf("backend Close calls = %d, want one", closeCalls)
	}
}

func TestRunAuditInitialWriteFailurePreventsBackendConstruction(t *testing.T) {
	path := writeAuditAppConfig(t, "not-used")
	sink := &appAuditSink{failFrom: 1}
	recorder := newAppAuditRecorder(t, sink)
	constructed := 0
	err := run([]string{"--config", path}, io.Discard, backendDependencies{
		newAudit: func(audit.Config) (*audit.Recorder, error) { return recorder, nil },
		newPTY: func(context.Context, chan<- session.Event, *adapters.Registry, int) (terminal.Backend, error) {
			constructed++
			return newRouterFakeBackend(agent.BackendPTY), nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "writing the run start to the audit") {
		t.Fatalf("run error = %v", err)
	}
	if constructed != 0 {
		t.Fatalf("constructed %d backend(s) after initial audit write failure", constructed)
	}
	sink.mu.Lock()
	writes := sink.writes
	closed := sink.closed
	lines := len(sink.lines)
	sink.mu.Unlock()
	if writes != 1 || !closed || lines != 0 {
		t.Fatalf("audit sink after sticky failure = writes %d closed %t lines %d", writes, closed, lines)
	}
}

func TestRunRejectsNilAuditRecorderBeforeBackendConstruction(t *testing.T) {
	path := writeAuditAppConfig(t, "not-used")
	constructed := 0
	err := run([]string{"--config", path}, io.Discard, backendDependencies{
		newAudit: func(audit.Config) (*audit.Recorder, error) { return nil, nil },
		newPTY: func(context.Context, chan<- session.Event, *adapters.Registry, int) (terminal.Backend, error) {
			constructed++
			return newRouterFakeBackend(agent.BackendPTY), nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "audit journal") {
		t.Fatalf("run error = %v", err)
	}
	if constructed != 0 {
		t.Fatalf("constructed %d backend(s) after nil audit recorder", constructed)
	}
}

func TestRunAuditInitializationFailsBeforeBackendConstruction(t *testing.T) {
	path := writeAuditAppConfig(t, "not-used")
	constructed := 0
	err := run([]string{"--config", path}, io.Discard, backendDependencies{
		newAudit: func(audit.Config) (*audit.Recorder, error) {
			return nil, errors.New("audit directory unavailable")
		},
		newPTY: func(context.Context, chan<- session.Event, *adapters.Registry, int) (terminal.Backend, error) {
			constructed++
			return newRouterFakeBackend(agent.BackendPTY), nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "audit journal") {
		t.Fatalf("run error = %v", err)
	}
	if constructed != 0 {
		t.Fatalf("constructed %d backend(s) after audit preflight failure", constructed)
	}
}

func TestRunAuditWriteFailureRollsBackStartedSession(t *testing.T) {
	path := writeAuditAppConfig(t, "not-used")
	sink := &appAuditSink{failFrom: 2}
	recorder := newAppAuditRecorder(t, sink)
	backend := newRouterFakeBackend(agent.BackendPTY)
	err := run([]string{"--config", path}, io.Discard, backendDependencies{
		newAudit: func(audit.Config) (*audit.Recorder, error) { return recorder, nil },
		newPTY: func(context.Context, chan<- session.Event, *adapters.Registry, int) (terminal.Backend, error) {
			return backend, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "auditing the startup of agent") {
		t.Fatalf("run error = %v", err)
	}
	backend.mu.Lock()
	starts := len(backend.starts)
	closeCalls := backend.closeCalls
	backend.mu.Unlock()
	if starts != 1 || closeCalls != 1 {
		t.Fatalf("backend lifecycle = starts %d close %d", starts, closeCalls)
	}
}

func TestAuditCleanupResultNeverInventsPerSessionState(t *testing.T) {
	tests := []struct {
		name    string
		info    session.Info
		policy  config.SessionPolicy
		closed  bool
		known   bool
		outcome audit.Outcome
		reason  string
	}{
		{
			name: "PTY close completed", info: session.Info{Backend: agent.BackendPTY}, closed: true, known: true,
			outcome: audit.OutcomeSucceeded, reason: "backend_cleanup_completed",
		},
		{
			name: "tmux persistence requested", info: session.Info{Backend: agent.BackendTmux},
			policy: config.SessionPolicy{PersistOnExit: true}, closed: true, known: true,
			outcome: audit.OutcomeSkipped, reason: "persistence_requested",
		},
		{
			name: "aggregate backend failure", info: session.Info{Backend: agent.BackendTmux}, closed: false, known: true,
			outcome: audit.OutcomeUnknown, reason: "backend_cleanup_incomplete",
		},
		{
			name: "backend identity unknown", info: session.Info{Backend: "missing"}, closed: false, known: false,
			outcome: audit.OutcomeUnknown, reason: "backend_cleanup_incomplete",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			outcome, reason := auditCleanupResult(test.info, test.policy, test.closed, test.known)
			if outcome != test.outcome || reason != test.reason {
				t.Fatalf("cleanup result = %s/%s, want %s/%s", outcome, reason, test.outcome, test.reason)
			}
		})
	}
}

func newAppAuditRecorder(t *testing.T, sink audit.LineSink) *audit.Recorder {
	t.Helper()
	configuration := audit.DefaultConfig()
	identifier := 0
	recorder, err := audit.NewRecorder(
		configuration,
		sink,
		func() time.Time { return time.Date(2026, 8, 26, 20, 0, identifier, 0, time.FixedZone("test", 3600)) },
		func() (string, error) {
			identifier++
			return "audit-id-" + string(rune('a'+identifier-1)), nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return recorder
}

func writeAuditAppConfig(t *testing.T, secret string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := `version: 1
backend: pty
audit:
  enabled: true
  mode: detailed
  path: audit/audit.jsonl
  max_file_size_mb: 1
  max_files: 2
agents:
  - id: first
    name: audit-name-secret-sentinel
    command: [audit-command-secret-sentinel, argument]
    env:
      API_TOKEN: "` + secret + `"
  - id: second
    name: Second
    shell: "echo audit-shell-secret-sentinel"
intercept_patterns:
  - pattern: continue
    description: Continue
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func joinAuditLines(lines [][]byte) []byte {
	var result []byte
	for _, line := range lines {
		result = append(result, line...)
	}
	return result
}
