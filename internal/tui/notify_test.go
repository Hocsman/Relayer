package tui

import (
	"sync"
	"testing"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/notify"
	"github.com/Hocsman/Relayer/internal/policy"
	"github.com/Hocsman/Relayer/internal/session"
)

type recordingNotifier struct {
	mu            sync.Mutex
	notifications []notify.Notification
}

func (r *recordingNotifier) Notify(n notify.Notification) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notifications = append(r.notifications, n)
}

func TestQueueHumanEventDispatchesNotification(t *testing.T) {
	backend := newPolicyTestBackend()
	events := make(chan session.Event, 1)
	defer close(events)

	evaluator, err := policy.New(policy.DefaultConfig())
	if err != nil {
		t.Fatalf("policy.New returned error: %v", err)
	}

	model, err := NewModelWithPolicy(
		backend,
		events,
		[]Pane{{ID: "agent-1", Name: "Agent Alpha", Command: "echo hi"}},
		80,
		24,
		nil,
		evaluator,
	)
	if err != nil {
		t.Fatalf("NewModelWithPolicy error: %v", err)
	}

	notifier := &recordingNotifier{}
	model.SetNotifier(notifier)

	prompt := adapters.Event{
		ID:        "evt-ask-1",
		SessionID: "agent-1",
		Type:      adapters.EventConfirmation,
		Match:     "Overwrite file? [y/n]",
	}
	backend.setPending(prompt)

	model.Update(session.AdapterEvent{Event: prompt})

	notifier.mu.Lock()
	defer notifier.mu.Unlock()
	if len(notifier.notifications) != 1 {
		t.Fatalf("got %d notifications, want 1", len(notifier.notifications))
	}
	n := notifier.notifications[0]
	if n.AgentName != "Agent Alpha" {
		t.Errorf("got AgentName %q, want %q", n.AgentName, "Agent Alpha")
	}
	if n.EventID != "evt-ask-1" {
		t.Errorf("got EventID %q, want %q", n.EventID, "evt-ask-1")
	}
	if n.Reason != "confirmation required" {
		t.Errorf("got Reason %q, want %q", n.Reason, "confirmation required")
	}
}
