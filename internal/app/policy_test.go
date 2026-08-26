package app

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/policy"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/terminal"
	"github.com/Hocsman/Relayer/internal/tmuxbackend"
)

type encodedPolicyDecision struct {
	event      adapters.Event
	decision   adapters.Decision
	manualText string
}

type policyDecisionRecorder struct {
	mu    sync.Mutex
	calls []encodedPolicyDecision
	err   error
}

type policyDecisionAdapter struct{ recorder *policyDecisionRecorder }

func (*policyDecisionAdapter) ID() string { return "policy-test" }

func (*policyDecisionAdapter) Detect(*adapters.DetectionState, []byte) ([]adapters.Event, error) {
	return nil, nil
}

func (a *policyDecisionAdapter) EncodeDecision(
	event adapters.Event,
	decision adapters.Decision,
	manualInput string,
) ([]byte, error) {
	a.recorder.mu.Lock()
	a.recorder.calls = append(a.recorder.calls, encodedPolicyDecision{
		event: event.Clone(), decision: decision, manualText: manualInput,
	})
	err := a.recorder.err
	a.recorder.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return []byte("encoded-" + string(decision)), nil
}

func TestBackendRouterAppliesAutomaticDecisionThroughCanonicalAdapterEvent(t *testing.T) {
	backend := &routerEventBackend{routerFakeBackend: newRouterFakeBackend(agent.BackendPTY)}
	router, err := newBackendRouter(context.Background(), backend)
	if err != nil {
		t.Fatal(err)
	}
	recorder := &policyDecisionRecorder{}
	registry, err := adapters.NewRegistry(adapters.DefaultPatterns())
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(adapters.Descriptor{
		ID: "policy-test", Status: adapters.StatusStable, Implemented: true,
	}, func() (adapters.Adapter, error) {
		return &policyDecisionAdapter{recorder: recorder}, nil
	}); err != nil {
		t.Fatal(err)
	}
	router.adapters = registry
	t.Cleanup(func() { _ = router.Close(context.Background()) })
	if _, err := router.Start(context.Background(), agent.Spec{
		ID: "policy-agent", Name: "Policy agent", Command: []string{"runner"}, Backend: agent.BackendPTY,
	}, terminal.Size{}); err != nil {
		t.Fatal(err)
	}

	canonical := adapters.Event{
		ID: "evt-policy-1", SessionID: "policy-agent", AgentID: "policy-agent",
		Adapter: "policy-test", Type: adapters.EventConfirmation, Risk: adapters.RiskLow,
		Summary: "canonical summary", Match: "canonical match",
	}
	backend.pending = eventPointer(canonical)
	presented := canonical.Clone()
	presented.Adapter = adapters.GenericID
	presented.Summary = "untrusted presentation"
	presented.Match = "untrusted match"
	if err := router.ApplyDecision(
		context.Background(), "POLICY-AGENT", presented, adapters.DecisionAllow, "",
	); err != nil {
		t.Fatalf("ApplyDecision: %v", err)
	}

	recorder.mu.Lock()
	encodeCalls := append([]encodedPolicyDecision(nil), recorder.calls...)
	recorder.mu.Unlock()
	if len(encodeCalls) != 1 || encodeCalls[0].event.Summary != canonical.Summary ||
		encodeCalls[0].event.Match != canonical.Match || encodeCalls[0].decision != adapters.DecisionAllow ||
		encodeCalls[0].manualText != "" {
		t.Fatalf("encode calls = %#v", encodeCalls)
	}
	backend.mu.Lock()
	sends := append([]routerEventSendCall(nil), backend.eventSends...)
	backend.mu.Unlock()
	if len(sends) != 1 || sends[0].eventID != canonical.ID ||
		!reflect.DeepEqual(sends[0].data, []byte("encoded-allow")) {
		t.Fatalf("event sends = %#v", sends)
	}

	denied := canonical.Clone()
	denied.ID = "evt-policy-2"
	backend.pending = eventPointer(denied)
	if err := router.ApplyDecision(
		context.Background(), "policy-agent", denied, adapters.DecisionDeny, "",
	); err != nil {
		t.Fatalf("ApplyDecision deny: %v", err)
	}
	recorder.mu.Lock()
	encodeCalls = append([]encodedPolicyDecision(nil), recorder.calls...)
	recorder.mu.Unlock()
	backend.mu.Lock()
	sends = append([]routerEventSendCall(nil), backend.eventSends...)
	backend.mu.Unlock()
	if len(encodeCalls) != 2 || encodeCalls[1].decision != adapters.DecisionDeny ||
		encodeCalls[1].manualText != "" || len(sends) != 2 || sends[1].eventID != denied.ID ||
		!reflect.DeepEqual(sends[1].data, []byte("encoded-deny")) {
		t.Fatalf("deny encoding/sends = %#v / %#v", encodeCalls, sends)
	}
}

func TestBackendRouterUnsupportedAutomaticDecisionDoesNotWrite(t *testing.T) {
	backend := &routerEventBackend{routerFakeBackend: newRouterFakeBackend(agent.BackendPTY)}
	router, err := newBackendRouter(context.Background(), backend)
	if err != nil {
		t.Fatal(err)
	}
	router.adapters, err = adapters.NewRegistry(adapters.DefaultPatterns())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = router.Close(context.Background()) })
	if _, err := router.Start(context.Background(), agent.Spec{
		ID: "generic", Name: "Generic", Command: []string{"runner"}, Backend: agent.BackendPTY,
	}, terminal.Size{}); err != nil {
		t.Fatal(err)
	}
	event := adapters.Event{
		ID: "evt-generic", SessionID: "generic", AgentID: "generic",
		Adapter: adapters.GenericID, Type: adapters.EventConfirmation,
	}
	backend.pending = eventPointer(event)
	err = router.ApplyDecision(context.Background(), "generic", event, adapters.DecisionDeny, "")
	if !errors.Is(err, adapters.ErrDecisionUnsupported) {
		t.Fatalf("ApplyDecision error = %v", err)
	}
	backend.mu.Lock()
	sendCount := len(backend.eventSends)
	pending := backend.pending.Clone()
	backend.mu.Unlock()
	if sendCount != 0 || pending.ID != event.ID {
		t.Fatalf("unsupported decision wrote or mutated pending: sends=%d pending=%#v", sendCount, pending)
	}
}

func TestValidatePolicyAgentIDs(t *testing.T) {
	configuration := policy.Config{DefaultAction: policy.ActionAsk, Rules: []policy.Rule{{
		Name:   "reviewer-only",
		Match:  policy.Match{AgentIDs: []string{" REVIEWER "}},
		Action: policy.ActionAsk,
	}}}
	specs := []agent.Spec{{ID: "reviewer"}}
	if err := validatePolicyAgentIDs(configuration, specs); err != nil {
		t.Fatalf("valid agent reference: %v", err)
	}
	configuration.Rules[0].Match.AgentIDs[0] = "missing"
	if err := validatePolicyAgentIDs(configuration, specs); err == nil {
		t.Fatal("unknown policy agent was accepted")
	}
}

func TestPolicyPreflightFailsBeforeBackendSelectionOrConstruction(t *testing.T) {
	for _, test := range []struct {
		name     string
		policies string
	}{
		{
			name: "invalid regex",
			policies: `policies:
  default_action: ask
  rules:
    - name: invalid-regex
      match:
        text_regex: '['
      action: allow`,
		},
		{
			name: "unknown agent",
			policies: `policies:
  default_action: ask
  rules:
    - name: unknown-agent
      match:
        agent_ids: [missing-agent]
      action: deny`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			contents := `version: 1
backend: pty
` + test.policies + `
agents:
  - id: configured-agent
    name: Configured agent
    command: [runner]
intercept_patterns:
  - pattern: continue
    description: Confirmation
`
			if err := os.WriteFile(configPath, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			lookupCalls := 0
			factoryCalls := 0
			err := run([]string{"--config", configPath}, io.Discard, backendDependencies{
				lookup: func(string) (string, error) {
					lookupCalls++
					return "", errors.New("lookup must not run")
				},
				newPTY: func(context.Context, chan<- session.Event, *adapters.Registry, int) (terminal.Backend, error) {
					factoryCalls++
					return nil, errors.New("PTY factory must not run")
				},
				newTmux: func(context.Context, chan<- session.Event, *adapters.Registry, int, tmuxbackend.Options) (terminal.Backend, error) {
					factoryCalls++
					return nil, errors.New("tmux factory must not run")
				},
			})
			if err == nil {
				t.Fatal("invalid policy reached startup")
			}
			if lookupCalls != 0 || factoryCalls != 0 {
				t.Fatalf("preflight calls: lookup=%d factories=%d", lookupCalls, factoryCalls)
			}
		})
	}
}

func eventPointer(event adapters.Event) *adapters.Event {
	clone := event.Clone()
	return &clone
}
