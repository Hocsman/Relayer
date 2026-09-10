package notify

import (
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// Notification carries display-safe information about an event needing operator attention.
type Notification struct {
	Title     string
	Body      string
	SessionID string
	AgentName string
	Reason    string
	EventID   string
}

// Notifier delivers operator notifications and alerts.
type Notifier interface {
	Notify(Notification)
}

type noopNotifier struct{}

func (noopNotifier) Notify(Notification) {}

// NewNoop returns a notifier that discards all notifications.
func NewNoop() Notifier {
	return noopNotifier{}
}

// desktopSender is the platform-specific desktop notification implementation.
var desktopSender = showDesktopNotification

type compositeNotifier struct {
	config       Config
	bellOutput   io.Writer
	mu           sync.Mutex
	lastEventID  string
	lastSentTime time.Time
	throttle     time.Duration
}

// New constructs an active Notifier based on the given configuration.
func New(config Config, bellOutput io.Writer) Notifier {
	if !config.Enabled || (!config.Bell && !config.Desktop) {
		return noopNotifier{}
	}
	if bellOutput == nil {
		bellOutput = os.Stderr
	}
	return &compositeNotifier{
		config:     config,
		bellOutput: bellOutput,
		throttle:   1 * time.Second,
	}
}

func (c *compositeNotifier) Notify(n Notification) {
	c.mu.Lock()
	now := time.Now()
	// Deduplicate if identical EventID or if triggered within the throttle window
	if (n.EventID != "" && n.EventID == c.lastEventID) || (!c.lastSentTime.IsZero() && now.Sub(c.lastSentTime) < c.throttle) {
		c.mu.Unlock()
		return
	}
	c.lastEventID = n.EventID
	c.lastSentTime = now
	c.mu.Unlock()

	// 1. Terminal bell
	if c.config.Bell && c.bellOutput != nil {
		_, _ = c.bellOutput.Write([]byte{'\a'})
	}

	// 2. Desktop notification
	if c.config.Desktop {
		title := n.Title
		if strings.TrimSpace(title) == "" {
			title = "Relayer"
		}
		body := n.Body
		if strings.TrimSpace(body) == "" {
			if n.AgentName != "" && n.Reason != "" {
				body = n.AgentName + ": " + n.Reason
			} else if n.AgentName != "" {
				body = n.AgentName + " requires a human decision"
			} else {
				body = "An agent requires a human decision"
			}
		}

		go func() {
			desktopSender(title, body)
		}()
	}
}
