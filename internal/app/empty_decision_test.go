package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/adapters"
)

// An empty manual decision is not an answer. The generic adapter encodes it as
// a bare carriage return, which the prompt reads as its default — frequently
// the permissive one — so a reflex keystroke could stand in for a decision and
// be recorded as a human answer.
func TestApplyDecisionRejectsAnEmptyManualAnswer(t *testing.T) {
	router := &backendRouter{}
	event := adapters.Event{
		ID: "occurrence-1", SessionID: "agent-a", AgentID: "agent-a",
		Adapter: adapters.GenericID, Type: adapters.EventConfirmation,
		Risk: adapters.RiskUnknown,
	}

	for _, empty := range []string{"", " ", "\t", "   \t "} {
		err := router.ApplyDecision(context.Background(), "agent-a", event, adapters.DecisionManual, empty)
		if !errors.Is(err, ErrEmptyManualDecision) {
			t.Fatalf("ApplyDecision(%q) = %v, want ErrEmptyManualDecision", empty, err)
		}
	}

	// The refusal must come before any backend lookup, so an empty answer can
	// never reach a session even when routing would have failed anyway.
	if err := router.ApplyDecision(context.Background(), "unknown", event, adapters.DecisionManual, ""); !errors.Is(err, ErrEmptyManualDecision) {
		t.Fatalf("empty answer was not refused first: %v", err)
	}

	// A real answer proceeds far enough to fail on something else, proving the
	// emptiness check is not swallowing valid input.
	err := router.ApplyDecision(context.Background(), "agent-a", event, adapters.DecisionManual, "y")
	if errors.Is(err, ErrEmptyManualDecision) {
		t.Fatalf("a typed answer was rejected as empty: %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "adaptateurs") {
		t.Fatalf("unexpected error for a typed answer: %v", err)
	}
}
