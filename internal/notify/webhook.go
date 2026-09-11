package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// defaultWebhookTimeout is applied when no timeout is explicitly set in WebhookConfig.
const defaultWebhookTimeout = 5 * time.Second

// HTTPPoster abstracts HTTP POST execution for testing.
type HTTPPoster interface {
	Do(req *http.Request) (*http.Response, error)
}

// defaultHTTPClient is the standard HTTP client used for webhooks.
var defaultHTTPClient HTTPPoster = &http.Client{
	Timeout: defaultWebhookTimeout,
}

// buildSlackPayload formats a notification into an attractive Slack incoming webhook JSON payload.
func buildSlackPayload(n Notification) ([]byte, error) {
	color := "#3b82f6" // blue (info)
	icon := "ℹ️"
	if n.Severity == SeverityCritical {
		color = "#ef4444" // red
		icon = "🛡️"
	} else if n.Severity == SeverityWarning {
		color = "#f59e0b" // amber
		icon = "⏳"
	}

	headerText := fmt.Sprintf("%s *[%s]* %s", icon, strings.ToUpper(n.Severity), n.Title)
	if n.Kind == KindGuardrailBlocked {
		headerText = fmt.Sprintf("🛡️ *[SECURITY GUARDRAIL]* %s", n.Title)
	}

	type SlackField struct {
		Title string `json:"title"`
		Value string `json:"value"`
		Short bool   `json:"short"`
	}

	fields := make([]SlackField, 0, 5)
	if n.AgentName != "" {
		fields = append(fields, SlackField{Title: "Agent", Value: n.AgentName, Short: true})
	}
	if n.Severity != "" {
		fields = append(fields, SlackField{Title: "Severity", Value: strings.ToUpper(n.Severity), Short: true})
	}
	if n.Reason != "" {
		fields = append(fields, SlackField{Title: "Reason", Value: n.Reason, Short: true})
	}
	if n.SessionID != "" {
		fields = append(fields, SlackField{Title: "Session ID", Value: n.SessionID, Short: true})
	}
	if n.Details != "" {
		fields = append(fields, SlackField{Title: "Details", Value: n.Details, Short: false})
	}

	ts := n.Timestamp.Unix()
	if ts <= 0 {
		ts = time.Now().Unix()
	}

	type SlackAttachment struct {
		Color      string       `json:"color"`
		Title      string       `json:"title,omitempty"`
		Text       string       `json:"text"`
		Fields     []SlackField `json:"fields,omitempty"`
		Footer     string       `json:"footer"`
		FooterIcon string       `json:"footer_icon,omitempty"`
		Ts         int64        `json:"ts"`
	}

	payload := struct {
		Text        string            `json:"text"`
		Attachments []SlackAttachment `json:"attachments"`
	}{
		Text: headerText,
		Attachments: []SlackAttachment{
			{
				Color:  color,
				Text:   n.Body,
				Fields: fields,
				Footer: "Relayer Supervisor",
				Ts:     ts,
			},
		},
	}

	return json.Marshal(payload)
}

// buildDiscordPayload formats a notification into a Discord webhook JSON payload with embeds.
func buildDiscordPayload(n Notification) ([]byte, error) {
	color := 3899382 // blue (info)
	icon := "ℹ️"
	if n.Severity == SeverityCritical {
		color = 15682852 // red (#ef4444)
		icon = "🛡️"
	} else if n.Severity == SeverityWarning {
		color = 16103700 // amber (#f59e0b)
		icon = "⏳"
	}

	headerText := fmt.Sprintf("%s **[%s]** %s", icon, strings.ToUpper(n.Severity), n.Title)

	type DiscordField struct {
		Name   string `json:"name"`
		Value  string `json:"value"`
		Inline bool   `json:"inline"`
	}

	fields := make([]DiscordField, 0, 5)
	if n.AgentName != "" {
		fields = append(fields, DiscordField{Name: "Agent", Value: n.AgentName, Inline: true})
	}
	if n.Severity != "" {
		fields = append(fields, DiscordField{Name: "Severity", Value: strings.ToUpper(n.Severity), Inline: true})
	}
	if n.Reason != "" {
		fields = append(fields, DiscordField{Name: "Reason", Value: n.Reason, Inline: true})
	}
	if n.SessionID != "" {
		fields = append(fields, DiscordField{Name: "Session ID", Value: n.SessionID, Inline: true})
	}
	if n.Details != "" {
		fields = append(fields, DiscordField{Name: "Details", Value: n.Details, Inline: false})
	}

	ts := n.Timestamp.Format(time.RFC3339)
	if n.Timestamp.IsZero() {
		ts = time.Now().Format(time.RFC3339)
	}

	type DiscordEmbed struct {
		Title       string         `json:"title"`
		Description string         `json:"description"`
		Color       int            `json:"color"`
		Fields      []DiscordField `json:"fields,omitempty"`
		Timestamp   string         `json:"timestamp"`
		Footer      struct {
			Text string `json:"text"`
		} `json:"footer"`
	}

	embed := DiscordEmbed{
		Title:       n.Title,
		Description: n.Body,
		Color:       color,
		Fields:      fields,
		Timestamp:   ts,
	}
	embed.Footer.Text = "Relayer Supervisor"

	payload := struct {
		Username string         `json:"username"`
		Content  string         `json:"content"`
		Embeds   []DiscordEmbed `json:"embeds"`
	}{
		Username: "Relayer Supervisor",
		Content:  headerText,
		Embeds:   []DiscordEmbed{embed},
	}

	return json.Marshal(payload)
}

// buildGenericPayload formats a notification into standard JSON payload.
func buildGenericPayload(n Notification) ([]byte, error) {
	ts := n.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}

	payload := struct {
		Source    string    `json:"source"`
		Version   string    `json:"version"`
		Timestamp time.Time `json:"timestamp"`
		Kind      string    `json:"kind"`
		Severity  string    `json:"severity"`
		Title     string    `json:"title"`
		Body      string    `json:"body"`
		AgentName string    `json:"agent_name,omitempty"`
		SessionID string    `json:"session_id,omitempty"`
		EventID   string    `json:"event_id,omitempty"`
		Reason    string    `json:"reason,omitempty"`
		Details   string    `json:"details,omitempty"`
	}{
		Source:    "relayer",
		Version:   "1.0",
		Timestamp: ts,
		Kind:      n.Kind,
		Severity:  n.Severity,
		Title:     n.Title,
		Body:      n.Body,
		AgentName: n.AgentName,
		SessionID: n.SessionID,
		EventID:   n.EventID,
		Reason:    n.Reason,
		Details:   n.Details,
	}

	return json.Marshal(payload)
}

// sendWebhook dispatches an HTTP POST request to the specified webhook target.
func sendWebhook(ctx context.Context, client HTTPPoster, cfg WebhookConfig, n Notification) error {
	var body []byte
	var err error

	format := strings.ToLower(strings.TrimSpace(cfg.Format))
	switch WebhookFormat(format) {
	case FormatSlack:
		body, err = buildSlackPayload(n)
	case FormatDiscord:
		body, err = buildDiscordPayload(n)
	default:
		body, err = buildGenericPayload(n)
	}
	if err != nil {
		return fmt.Errorf("serialize webhook payload: %w", err)
	}

	timeout := defaultWebhookTimeout
	if cfg.Timeout != "" {
		if d, err := time.ParseDuration(cfg.Timeout); err == nil && d > 0 {
			timeout = d
		}
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, cfg.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create webhook request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Relayer-Supervisor/1.0")
	for k, v := range cfg.Headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("deliver webhook to %s: %w", cfg.URL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook endpoint %s returned HTTP %d", cfg.URL, resp.StatusCode)
	}

	return nil
}
