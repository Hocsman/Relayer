package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/agent"
)

func TestCheckAgentVersion(t *testing.T) {
	ctx := context.Background()
	// Every spec carries the vendor executable as its argv[0]: an agent the
	// probe may not run reports nothing at all, which the probe tests cover.
	claudeCommand := []string{"claude"}

	// 1. Simulated agent: ignored
	simSpec := agent.Spec{ID: "mock", Adapter: adapters.ClaudeID, Command: claudeCommand}
	info := CheckAgentVersion(ctx, simSpec, adapters.ClaudeID, true, func(ctx context.Context, s agent.Spec) (string, error) {
		return "3.0.0", nil
	})
	if info.Unverified || info.InstalledVersion != "" {
		t.Errorf("simulated agent should have empty info, got %+v", info)
	}

	// 2. Generic adapter: ignored
	genSpec := agent.Spec{ID: "gen", Adapter: adapters.GenericID, Command: []string{"runner"}}
	info = CheckAgentVersion(ctx, genSpec, adapters.GenericID, false, func(ctx context.Context, s agent.Spec) (string, error) {
		return "1.0.0", nil
	})
	if info.Unverified || info.InstalledVersion != "" {
		t.Errorf("generic adapter should have empty info, got %+v", info)
	}

	// 3. Claude Code with verified version 2.1.285
	claudeSpec := agent.Spec{ID: "claude", Adapter: adapters.ClaudeID, Command: claudeCommand}
	info = CheckAgentVersion(ctx, claudeSpec, adapters.ClaudeID, false, func(ctx context.Context, s agent.Spec) (string, error) {
		return "2.1.285", nil
	})
	if info.Unverified {
		t.Errorf("claude 2.1.285 should be verified, got unverified with reason: %s", info.Reason)
	}
	if info.InstalledVersion != "2.1.285" {
		t.Errorf("expected InstalledVersion '2.1.285', got %q", info.InstalledVersion)
	}

	// 4. Claude Code with unverified version 3.0.0
	info = CheckAgentVersion(ctx, claudeSpec, adapters.ClaudeID, false, func(ctx context.Context, s agent.Spec) (string, error) {
		return "3.0.0", nil
	})
	if !info.Unverified {
		t.Errorf("claude 3.0.0 should be unverified")
	}
	if !strings.Contains(info.Reason, "3.0.0") || !strings.Contains(info.Reason, "claude") {
		t.Errorf("unexpected reason: %s", info.Reason)
	}

	// 5. Claude Code with failed version inspection
	info = CheckAgentVersion(ctx, claudeSpec, adapters.ClaudeID, false, func(ctx context.Context, s agent.Spec) (string, error) {
		return "", errors.New("command not found")
	})
	if !info.Unverified {
		t.Errorf("failed version inspection should be marked unverified")
	}
	if !strings.Contains(info.Reason, "no version detected") {
		t.Errorf("expected reason to mention 'no version detected', got %s", info.Reason)
	}

	// 6. An inspector that refuses the agent is not a failed inspection: a
	// probe that must not run leaves nothing to warn about.
	info = CheckAgentVersion(ctx, claudeSpec, adapters.ClaudeID, false, func(ctx context.Context, s agent.Spec) (string, error) {
		return "", ErrVersionProbeNotApplicable
	})
	if info != (AgentVersionInfo{}) {
		t.Errorf("a skipped probe reported %+v, want no information", info)
	}
}
