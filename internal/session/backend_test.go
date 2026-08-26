package session

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/intercept"
)

func TestNewManagerWithRegistryRejectsNilAndExplicitUnknownAdapters(t *testing.T) {
	if _, err := NewManagerWithRegistry(context.Background(), make(chan Event, 1), nil, 1024); err == nil {
		t.Fatal("NewManagerWithRegistry accepted a nil registry")
	}
	registry, err := adapters.NewRegistry(nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	manager, err := NewManagerWithRegistry(context.Background(), make(chan Event, 8), registry, 1024)
	if err != nil {
		t.Fatalf("NewManagerWithRegistry: %v", err)
	}
	defer manager.Close()
	for _, test := range []struct {
		adapter string
		want    error
	}{
		{adapter: "missing", want: adapters.ErrUnknownAdapter},
		{adapter: "claude", want: adapters.ErrAdapterUnavailable},
	} {
		_, startErr := manager.Start(agent.Spec{
			ID:      "adapter-" + test.adapter,
			Name:    "adapter " + test.adapter,
			Command: []string{"this-executable-must-never-be-started"},
			Adapter: test.adapter,
		}, 80, 24)
		if !errors.Is(startErr, test.want) {
			t.Fatalf("Start adapter %q error = %v, want %v", test.adapter, startErr, test.want)
		}
	}
}

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
