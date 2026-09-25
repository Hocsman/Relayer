package server

import (
	"context"
	"io"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/config"
	"github.com/Hocsman/Relayer/internal/notify"
)

func TestSecuritySettingsSaveAndReloadWithoutLoss(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")

	if _, err := config.LoadOrCreate(configPath); err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	ctrl, err := NewController(configPath, io.Discard)
	if err != nil {
		t.Fatalf("NewController: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := ctrl.Start(ctx); err != nil {
		t.Fatalf("Start controller: %v", err)
	}
	defer func() {
		shutdownCtx, sCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer sCancel()
		_ = ctrl.Close(shutdownCtx)
	}()

	initial, err := ctrl.GetFullSettings()
	if err != nil {
		t.Fatalf("GetFullSettings initial: %v", err)
	}

	customWs := filepath.Join(tempDir, "custom-workspace")
	saveReq := SaveFullSettingsRequest{
		ExpectedRevision: initial.Revision,
		Security: &SecuritySettings{
			Profile:                     "developer-friendly",
			DefaultAction:               "ask",
			DryRun:                      true,
			BlockDestructive:            false,
			BlockExfiltration:           false,
			BlockSensitivePaths:         false,
			BlockOutsideWorkspace:       true,
			WorkspaceRoot:               customWs,
			RateLimitPerMinute:          42,
			MaxConsecutiveAutoDecisions: 7,
		},
	}

	saved, err := ctrl.SaveFullSettings("", saveReq)
	if err != nil {
		t.Fatalf("SaveFullSettings: %v", err)
	}

	// Verify saved view faithfully reflects non-default values
	if saved.Security.BlockDestructive != false {
		t.Errorf("saved BlockDestructive = true, want false")
	}
	if saved.Security.BlockExfiltration != false {
		t.Errorf("saved BlockExfiltration = true, want false")
	}
	if saved.Security.BlockSensitivePaths != false {
		t.Errorf("saved BlockSensitivePaths = true, want false")
	}
	if saved.Security.BlockOutsideWorkspace != true {
		t.Errorf("saved BlockOutsideWorkspace = false, want true")
	}
	if saved.Security.RateLimitPerMinute != 42 {
		t.Errorf("saved RateLimitPerMinute = %d, want 42", saved.Security.RateLimitPerMinute)
	}
	if saved.Security.MaxConsecutiveAutoDecisions != 7 {
		t.Errorf("saved MaxConsecutiveAutoDecisions = %d, want 7", saved.Security.MaxConsecutiveAutoDecisions)
	}
	if saved.Security.DryRun != true {
		t.Errorf("saved DryRun = false, want true")
	}

	// Verify reload from disk is also faithful (not returning hardcoded defaults)
	reloaded, err := ctrl.GetFullSettings()
	if err != nil {
		t.Fatalf("GetFullSettings after save: %v", err)
	}
	if reloaded.Security.BlockDestructive != false {
		t.Errorf("reloaded BlockDestructive = true, want false")
	}
	if reloaded.Security.BlockExfiltration != false {
		t.Errorf("reloaded BlockExfiltration = true, want false")
	}
	if reloaded.Security.BlockSensitivePaths != false {
		t.Errorf("reloaded BlockSensitivePaths = true, want false")
	}
	if reloaded.Security.BlockOutsideWorkspace != true {
		t.Errorf("reloaded BlockOutsideWorkspace = false, want true")
	}
	if reloaded.Security.RateLimitPerMinute != 42 {
		t.Errorf("reloaded RateLimitPerMinute = %d, want 42", reloaded.Security.RateLimitPerMinute)
	}
	if reloaded.Security.MaxConsecutiveAutoDecisions != 7 {
		t.Errorf("reloaded MaxConsecutiveAutoDecisions = %d, want 7", reloaded.Security.MaxConsecutiveAutoDecisions)
	}
}

func TestRestartRequiredAndSaveAndRestart(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")

	if _, err := config.LoadOrCreate(configPath); err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	ctrl, err := NewController(configPath, io.Discard)
	if err != nil {
		t.Fatalf("NewController: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := ctrl.Start(ctx); err != nil {
		t.Fatalf("Start controller: %v", err)
	}
	defer func() {
		shutdownCtx, sCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer sCancel()
		_ = ctrl.Close(shutdownCtx)
	}()

	initial, err := ctrl.GetFullSettings()
	if err != nil {
		t.Fatalf("GetFullSettings: %v", err)
	}

	// Initially, configuration is clean and matches running runtime
	if initial.RestartRequired {
		t.Errorf("initial RestartRequired = true, want false")
	}

	// Save modified security settings without restarting
	saved, err := ctrl.SaveFullSettings("", SaveFullSettingsRequest{
		ExpectedRevision: initial.Revision,
		Security: &SecuritySettings{
			DefaultAction: "ask",
			DryRun:        true,
		},
	})
	if err != nil {
		t.Fatalf("SaveFullSettings: %v", err)
	}

	// RestartRequired MUST now be true because disk revision has diverged from active engine revision
	if !saved.RestartRequired {
		t.Errorf("saved.RestartRequired = false, want true")
	}

	current, err := ctrl.GetFullSettings()
	if err != nil {
		t.Fatalf("GetFullSettings: %v", err)
	}
	if !current.RestartRequired {
		t.Errorf("current.RestartRequired = false, want true")
	}

	workerArgv := []string{"cmd.exe", "/c", "echo worker"}
	if runtime.GOOS != "windows" {
		workerArgv = []string{"sh", "-c", "sleep 10"}
	}

	// Provide at least 1 valid agent profile
	profilesInput := []AgentProfileInput{
		{
			ID:       "agent-1",
			Name:     "Agent One",
			PresetID: "custom",
			Backend:  "auto",
			Adapter:  "generic",
			Argv:     workerArgv,
		},
	}

	// Save and restart using the fresh revision token, for the run it restarts:
	// a request that names no run is refused.
	restartRes, err := ctrl.SaveAgentProfilesAndRestart(SaveAgentProfilesAndRestartRequest{
		ExpectedRunID:    ctrl.GetState().RunID,
		ExpectedRevision: current.Revision,
		Profiles:         profilesInput,
	})
	if err != nil {
		t.Fatalf("SaveAgentProfilesAndRestart: %v", err)
	}
	if restartRes.Outcome != "restarted" {
		t.Errorf("restart outcome = %q, want 'restarted'", restartRes.Outcome)
	}

	// After restart, RestartRequired MUST be false again
	afterRestart, err := ctrl.GetFullSettings()
	if err != nil {
		t.Fatalf("GetFullSettings after restart: %v", err)
	}
	if afterRestart.RestartRequired {
		t.Errorf("afterRestart.RestartRequired = true, want false")
	}
}

// TestNotificationsDisabledActuallyDisabled: a prompt that waits on a person is
// not broadcast as a notification while notifications are off. The prompt is a
// real session's, and the test first proves the prompt was taken in, and that
// the same run with notifications on does broadcast one: a prompt for a session
// the run does not have is dropped before anything could notify it, and the
// test passed without testing anything.
func TestNotificationsDisabledActuallyDisabled(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "off", true: "on"}[enabled], func(t *testing.T) {
			tempDir := t.TempDir()
			configPath := filepath.Join(tempDir, "config.yaml")

			loaded, err := config.LoadOrCreate(configPath)
			if err != nil {
				t.Fatalf("LoadOrCreate: %v", err)
			}
			_, _, err = config.UpdateFullConfiguration(configPath, loaded.Revision, config.FullConfigurationUpdate{
				Notifications: &notify.Config{Enabled: enabled},
			})
			if err != nil {
				t.Fatalf("UpdateFullConfiguration: %v", err)
			}

			ctrl, err := NewController(configPath, io.Discard)
			if err != nil {
				t.Fatalf("NewController: %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if err := ctrl.Start(ctx); err != nil {
				t.Fatalf("Start controller: %v", err)
			}
			defer func() {
				shutdownCtx, sCancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer sCancel()
				_ = ctrl.Close(shutdownCtx)
			}()

			ctrl.mu.RLock()
			notifEnabled := ctrl.notificationConfig.Enabled
			ctrl.mu.RUnlock()
			if notifEnabled != enabled {
				t.Fatalf("notificationConfig.Enabled = %v, want %v", notifEnabled, enabled)
			}

			var mu sync.Mutex
			receivedNotifications := 0
			unsubscribe := ctrl.Subscribe(func(event string, payload any) {
				if event == eventNotification {
					mu.Lock()
					receivedNotifications++
					mu.Unlock()
				}
			})
			defer unsubscribe()

			agents := ctrl.GetState().Agents
			if len(agents) == 0 {
				t.Fatal("the default configuration started no agent")
			}
			prompt := livePrompt(ctrl, agents[0].SessionID, "test-event-1", time.Now().UTC())
			if !promptOffered(ctrl, prompt.ID) {
				t.Fatal("the prompt was not taken in, so nothing could have notified it")
			}

			// Wait briefly to ensure no notification event was broadcast
			time.Sleep(50 * time.Millisecond)

			mu.Lock()
			count := receivedNotifications
			mu.Unlock()
			want := map[bool]int{false: 0, true: 1}[enabled]
			if count != want {
				t.Errorf("received %d notification event(s) with notifications %v, want %d", count, map[bool]string{false: "off", true: "on"}[enabled], want)
			}
		})
	}
}
