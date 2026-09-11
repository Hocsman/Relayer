package notify

import (
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityCritical = "critical"
)

const (
	KindPendingDecision  = "pending_decision"
	KindGuardrailBlocked = "guardrail_blocked"
	KindSessionState     = "session_state"
)

// Notification carries display-safe information about an event needing operator attention.
type Notification struct {
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	SessionID string    `json:"session_id,omitempty"`
	AgentName string    `json:"agent_name,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	EventID   string    `json:"event_id,omitempty"`
	Kind      string    `json:"kind,omitempty"`
	Severity  string    `json:"severity,omitempty"`
	Details   string    `json:"details,omitempty"`
	Timestamp time.Time `json:"timestamp"`
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
	httpClient   HTTPPoster
	mu           sync.Mutex
	lastEventID  string
	lastSentTime time.Time
	throttle     time.Duration
}

// New constructs an active Notifier based on the given configuration.
func New(config Config, bellOutput io.Writer) Notifier {
	if !config.Enabled || (!config.Bell && !config.Desktop && len(config.Webhooks) == 0) {
		return noopNotifier{}
	}
	if bellOutput == nil {
		bellOutput = os.Stderr
	}
	return &compositeNotifier{
		config:     config,
		bellOutput: bellOutput,
		httpClient: defaultHTTPClient,
		throttle:   1 * time.Second,
	}
}

func severityRank(s string) int {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case SeverityCritical:
		return 3
	case SeverityWarning:
		return 2
	case SeverityInfo:
		fallthrough
	default:
		return 1
	}
}

func severityMeetsThreshold(actual, minimum string) bool {
	if strings.TrimSpace(minimum) == "" {
		return true
	}
	return severityRank(actual) >= severityRank(minimum)
}

func (c *compositeNotifier) Notify(n Notification) {
	if n.Severity == "" {
		n.Severity = SeverityInfo
	}
	if n.Timestamp.IsZero() {
		n.Timestamp = time.Now().UTC()
	}
	if strings.TrimSpace(n.Title) == "" {
		n.Title = "Relayer"
	}
	if strings.TrimSpace(n.Body) == "" {
		if n.AgentName != "" && n.Reason != "" {
			n.Body = n.AgentName + ": " + n.Reason
		} else if n.AgentName != "" {
			n.Body = n.AgentName + " requires a human decision"
		} else {
			n.Body = "An agent requires a human decision"
		}
	}

	c.mu.Lock()
	now := time.Now()
	// Deduplicate if identical EventID.
	// Throttle non-critical events, but critical guardrails always pass through.
	isDuplicate := n.EventID != "" && n.EventID == c.lastEventID
	isThrottled := n.Severity != SeverityCritical && !c.lastSentTime.IsZero() && now.Sub(c.lastSentTime) < c.throttle
	if isDuplicate || isThrottled {
		c.mu.Unlock()
		return
	}
	c.lastEventID = n.EventID
	c.lastSentTime = now
	c.mu.Unlock()

	// Check if notification meets global threshold
	if !severityMeetsThreshold(n.Severity, c.config.MinSeverity) {
		return
	}

	// 1. Terminal bell
	if c.config.Bell && c.bellOutput != nil {
		_, _ = c.bellOutput.Write([]byte{'\a'})
	}

	// 2. Desktop notification
	if c.config.Desktop {
		title := n.Title
		if n.Severity == SeverityCritical && !strings.Contains(title, "🛡️") && !strings.Contains(title, "CRITICAL") {
			title = "🛡️ [CRITICAL] " + title
		}
		body := n.Body
		go func(t, b string) {
			desktopSender(t, b)
		}(title, body)
	}

	// 3. Remote Webhooks
	for _, w := range c.config.Webhooks {
		targetWebhook := w
		// Check webhook-specific minimum severity (default: warning)
		minSev := targetWebhook.MinSeverity
		if minSev == "" {
			minSev = SeverityWarning
		}
		if !severityMeetsThreshold(n.Severity, minSev) {
			continue
		}

		go func(webhook WebhookConfig, payload Notification) {
			_ = sendWebhook(context.Background(), c.httpClient, webhook, payload)
		}(targetWebhook, n)
	}
}
