package notify

import (
	"bytes"
	"sync"
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if !cfg.Enabled || !cfg.Bell || !cfg.Desktop {
		t.Fatalf("unexpected DefaultConfig values: %#v", cfg)
	}
}

func TestDisabledConfig(t *testing.T) {
	cfg := DisabledConfig()
	if cfg.Enabled || cfg.Bell || cfg.Desktop {
		t.Fatalf("unexpected DisabledConfig values: %#v", cfg)
	}
}

func TestNewDisabledReturnsNoop(t *testing.T) {
	var buf bytes.Buffer
	notifier := New(Config{Enabled: false}, &buf)
	if _, ok := notifier.(noopNotifier); !ok {
		t.Fatalf("expected noopNotifier, got %T", notifier)
	}

	notifier.Notify(Notification{Title: "Test", Body: "Body"})
	if buf.Len() != 0 {
		t.Fatalf("expected 0 bytes written, got %d", buf.Len())
	}
}

func TestBellNotification(t *testing.T) {
	var buf bytes.Buffer
	notifier := New(Config{Enabled: true, Bell: true, Desktop: false}, &buf)

	notifier.Notify(Notification{
		Title:     "Relayer",
		AgentName: "Agent-1",
		Reason:    "confirmation required",
		EventID:   "event-1",
	})

	if buf.String() != "\a" {
		t.Fatalf("expected \\a in buffer, got %q", buf.String())
	}
}

func TestDesktopNotificationDispatched(t *testing.T) {
	var mu sync.Mutex
	var gotTitle, gotBody string
	called := make(chan struct{}, 1)

	origDesktop := desktopSender
	defer func() { desktopSender = origDesktop }()

	desktopSender = func(title, body string) {
		mu.Lock()
		gotTitle = title
		gotBody = body
		mu.Unlock()
		select {
		case called <- struct{}{}:
		default:
		}
	}

	notifier := New(Config{Enabled: true, Bell: false, Desktop: true}, nil)
	notifier.Notify(Notification{
		Title:     "Relayer Alert",
		AgentName: "Agent-Code",
		Reason:    "sensitive input required",
		EventID:   "evt-100",
	})

	select {
	case <-called:
	case <-time.After(1 * time.Second):
		t.Fatal("desktop notification was not dispatched")
	}

	mu.Lock()
	defer mu.Unlock()
	if gotTitle != "Relayer Alert" {
		t.Errorf("title = %q, want %q", gotTitle, "Relayer Alert")
	}
	if gotBody != "Agent-Code: sensitive input required" {
		t.Errorf("body = %q, want %q", gotBody, "Agent-Code: sensitive input required")
	}
}

func TestDeduplicationAndThrottling(t *testing.T) {
	var buf bytes.Buffer
	notifier := New(Config{Enabled: true, Bell: true, Desktop: false}, &buf)

	// First notification
	notifier.Notify(Notification{EventID: "dup-1"})
	if buf.Len() != 1 {
		t.Fatalf("expected 1 byte, got %d", buf.Len())
	}

	// Immediately sending identical event ID
	notifier.Notify(Notification{EventID: "dup-1"})
	if buf.Len() != 1 {
		t.Fatalf("expected deduplication to prevent second bell, got %d bytes", buf.Len())
	}

	// Immediately sending another event ID within throttle window
	notifier.Notify(Notification{EventID: "dup-2"})
	if buf.Len() != 1 {
		t.Fatalf("expected throttle to prevent rapid bell, got %d bytes", buf.Len())
	}
}
