package server

import (
	"context"
	"io"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/config"
	"github.com/Hocsman/Relayer/internal/notify"
	"github.com/Hocsman/Relayer/internal/session"
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
			ID:      "agent-1",
			Name:    "Agent One",
			Backend: "auto",
			Adapter: "generic",
			Argv:    workerArgv,
		},
	}

	// Save and restart using the fresh revision token
	restartRes, err := ctrl.SaveAgentProfilesAndRestart(SaveAgentProfilesAndRestartRequest{
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

func TestNotificationsDisabledActuallyDisabled(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")

	loaded, err := config.LoadOrCreate(configPath)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	// Explicitly disable notifications in config file
	_, _, err = config.UpdateFullConfiguration(configPath, loaded.Revision, config.FullConfigurationUpdate{
		Notifications: &notify.Config{
			Enabled: false,
		},
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

	// Verify notificationConfig is disabled
	ctrl.mu.RLock()
	notifEnabled := ctrl.notificationConfig.Enabled
	ctrl.mu.RUnlock()
	if notifEnabled {
		t.Fatalf("notificationConfig.Enabled = true, want false")
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

	// Dispatch an actionable adapter event requiring arbitration
	arbitrationEv := adapters.Event{
		ID:        "test-event-1",
		SessionID: "agent-1",
		AgentID:   "agent-1",
		Type:      adapters.EventConfirmation,
		Summary:   "Do you confirm this action?",
	}

	ctrl.handleEvent(ctx, ctrl.runtime, session.AdapterEvent{Event: arbitrationEv})

	// Wait briefly to ensure no notification event was broadcast
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	count := receivedNotifications
	mu.Unlock()

	if count != 0 {
		t.Errorf("received %d notification event(s) when notifications were disabled, want 0", count)
	}
}
