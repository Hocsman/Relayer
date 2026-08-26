package session

import (
	"context"
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/intercept"
)

func TestPTYManagerRejectsTmuxSelectorsBeforeLaunchingAProcess(t *testing.T) {
	events := make(chan Event, 1)
	manager, err := NewManager(context.Background(), events, []intercept.Pattern{{
		Name:        "confirmation",
		Description: "confirmation",
		Expression:  `(?i)continue`,
	}}, 1024)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer manager.Close()

	for _, backend := range []string{agent.BackendTmux, agent.BackendAuto} {
		t.Run(backend, func(t *testing.T) {
			_, startErr := manager.Start(agent.Spec{
				ID:      "must-not-start-" + backend,
				Name:    "Must not start " + backend,
				Command: []string{"this-executable-must-never-be-resolved"},
				Backend: backend,
			}, 80, 24)
			if startErr == nil || !strings.Contains(startErr.Error(), "gestionnaire PTY") {
				t.Fatalf("Start backend %q error = %v, want an explicit PTY-manager rejection", backend, startErr)
			}
		})
	}

	select {
	case event := <-events:
		t.Fatalf("rejected selectors emitted an event, so a process may have started: %#v", event)
	default:
	}
}
