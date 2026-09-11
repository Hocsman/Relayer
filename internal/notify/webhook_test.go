package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func sampleNotification() Notification {
	return Notification{
		Title:     "Relayer Decision",
		Body:      "Agent claude requires confirmation for apt-get install",
		SessionID: "sess-claude-1",
		AgentName: "claude",
		Reason:    "confirmation required",
		EventID:   "evt-12345",
		Kind:      KindPendingDecision,
		Severity:  SeverityWarning,
		Details:   "apt-get install -y ripgrep",
		Timestamp: time.Date(2026, 9, 11, 18, 0, 0, 0, time.UTC),
	}
}

func sampleGuardrailNotification() Notification {
	return Notification{
		Title:     "Relayer Guardrail Alert",
		Body:      "Agent codex was blocked by security policy",
		SessionID: "sess-codex-2",
		AgentName: "codex",
		Reason:    "destructive_command_blocked",
		EventID:   "evt-67890",
		Kind:      KindGuardrailBlocked,
		Severity:  SeverityCritical,
		Details:   "rm -rf /",
		Timestamp: time.Date(2026, 9, 11, 18, 5, 0, 0, time.UTC),
	}
}

func TestBuildSlackPayload(t *testing.T) {
	n := sampleGuardrailNotification()
	data, err := buildSlackPayload(n)
	if err != nil {
		t.Fatalf("buildSlackPayload failed: %v", err)
	}

	var parsed struct {
		Text        string `json:"text"`
		Attachments []struct {
			Color  string `json:"color"`
			Text   string `json:"text"`
			Fields []struct {
				Title string `json:"title"`
				Value string `json:"value"`
			} `json:"fields"`
			Footer string `json:"footer"`
			Ts     int64  `json:"ts"`
		} `json:"attachments"`
	}

	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal Slack payload failed: %v\nJSON: %s", err, string(data))
	}

	if !strings.Contains(parsed.Text, "SECURITY GUARDRAIL") {
		t.Errorf("expected text to mention SECURITY GUARDRAIL, got %q", parsed.Text)
	}
	if len(parsed.Attachments) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(parsed.Attachments))
	}
	att := parsed.Attachments[0]
	if att.Color != "#ef4444" {
		t.Errorf("expected critical color #ef4444, got %q", att.Color)
	}
	if att.Text != n.Body {
		t.Errorf("expected body %q, got %q", n.Body, att.Text)
	}

	foundAgent, foundReason, foundDetails := false, false, false
	for _, f := range att.Fields {
		if f.Title == "Agent" && f.Value == "codex" {
			foundAgent = true
		}
		if f.Title == "Reason" && f.Value == "destructive_command_blocked" {
			foundReason = true
		}
		if f.Title == "Details" && f.Value == "rm -rf /" {
			foundDetails = true
		}
	}
	if !foundAgent || !foundReason || !foundDetails {
		t.Errorf("missing expected fields in Slack payload: %#v", att.Fields)
	}
}

func TestBuildDiscordPayload(t *testing.T) {
	n := sampleNotification()
	data, err := buildDiscordPayload(n)
	if err != nil {
		t.Fatalf("buildDiscordPayload failed: %v", err)
	}

	var parsed struct {
		Username string `json:"username"`
		Content  string `json:"content"`
		Embeds   []struct {
			Title       string `json:"title"`
			Description string `json:"description"`
			Color       int    `json:"color"`
			Fields      []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"fields"`
			Timestamp string `json:"timestamp"`
		} `json:"embeds"`
	}

	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal Discord payload failed: %v\nJSON: %s", err, string(data))
	}

	if parsed.Username != "Relayer Supervisor" {
		t.Errorf("expected username 'Relayer Supervisor', got %q", parsed.Username)
	}
	if len(parsed.Embeds) != 1 {
		t.Fatalf("expected 1 embed, got %d", len(parsed.Embeds))
	}
	emb := parsed.Embeds[0]
	if emb.Color != 16103700 { // amber
		t.Errorf("expected warning color 16103700, got %d", emb.Color)
	}
	if emb.Title != n.Title {
		t.Errorf("expected embed title %q, got %q", n.Title, emb.Title)
	}
}

func TestBuildGenericPayload(t *testing.T) {
	n := sampleGuardrailNotification()
	data, err := buildGenericPayload(n)
	if err != nil {
		t.Fatalf("buildGenericPayload failed: %v", err)
	}

	var parsed struct {
		Source    string    `json:"source"`
		Version   string    `json:"version"`
		Timestamp time.Time `json:"timestamp"`
		Kind      string    `json:"kind"`
		Severity  string    `json:"severity"`
		Title     string    `json:"title"`
		Body      string    `json:"body"`
		AgentName string    `json:"agent_name"`
		SessionID string    `json:"session_id"`
		EventID   string    `json:"event_id"`
		Reason    string    `json:"reason"`
		Details   string    `json:"details"`
	}

	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal generic payload failed: %v", err)
	}

	if parsed.Source != "relayer" || parsed.Kind != KindGuardrailBlocked || parsed.Severity != SeverityCritical {
		t.Errorf("unexpected generic payload fields: %#v", parsed)
	}
	if parsed.Details != "rm -rf /" {
		t.Errorf("expected details 'rm -rf /', got %q", parsed.Details)
	}
}

func TestSendWebhookDelivery(t *testing.T) {
	var receivedMethod, receivedAuth, receivedContentType string
	var receivedBody []byte

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedMethod = r.Method
		receivedAuth = r.Header.Get("Authorization")
		receivedContentType = r.Header.Get("Content-Type")
		receivedBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	cfg := WebhookConfig{
		Name:    "test-server",
		URL:     server.URL,
		Format:  "generic",
		Timeout: "2s",
		Headers: map[string]string{
			"Authorization": "Bearer test-secret-token",
		},
	}

	n := sampleNotification()
	err := sendWebhook(context.Background(), http.DefaultClient, cfg, n)
	if err != nil {
		t.Fatalf("sendWebhook failed: %v", err)
	}

	if receivedMethod != http.MethodPost {
		t.Errorf("expected POST method, got %q", receivedMethod)
	}
	if receivedAuth != "Bearer test-secret-token" {
		t.Errorf("expected Authorization header, got %q", receivedAuth)
	}
	if receivedContentType != "application/json" {
		t.Errorf("expected application/json Content-Type, got %q", receivedContentType)
	}
	if !strings.Contains(string(receivedBody), "sess-claude-1") {
		t.Errorf("expected body to contain session ID, got %s", string(receivedBody))
	}
}

func TestSendWebhookErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal failure"}`))
	}))
	defer server.Close()

	cfg := WebhookConfig{
		URL:    server.URL,
		Format: "slack",
	}

	err := sendWebhook(context.Background(), http.DefaultClient, cfg, sampleNotification())
	if err == nil {
		t.Fatal("expected error on HTTP 500, got nil")
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("expected error message to mention HTTP 500, got %v", err)
	}
}

func TestValidateConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{
			name:    "valid default",
			cfg:     DefaultConfig(),
			wantErr: false,
		},
		{
			name: "valid webhooks",
			cfg: Config{
				Enabled:     true,
				MinSeverity: "warning",
				Webhooks: []WebhookConfig{
					{
						URL:         "https://hooks.slack.com/services/XXX",
						Format:      "slack",
						MinSeverity: "warning",
						Timeout:     "3s",
					},
					{
						URL:    "http://localhost:8080/webhook",
						Format: "generic",
					},
				},
			},
			wantErr: false,
		},
		{
			name: "invalid global min_severity",
			cfg: Config{
				Enabled:     true,
				MinSeverity: "super-critical",
			},
			wantErr: true,
		},
		{
			name: "webhook missing url",
			cfg: Config{
				Enabled: true,
				Webhooks: []WebhookConfig{
					{Format: "slack"},
				},
			},
			wantErr: true,
		},
		{
			name: "webhook invalid url scheme",
			cfg: Config{
				Enabled: true,
				Webhooks: []WebhookConfig{
					{URL: "ftp://files.internal/webhook"},
				},
			},
			wantErr: true,
		},
		{
			name: "webhook unsupported format",
			cfg: Config{
				Enabled: true,
				Webhooks: []WebhookConfig{
					{URL: "https://example.com", Format: "teams"},
				},
			},
			wantErr: true,
		},
		{
			name: "webhook invalid timeout",
			cfg: Config{
				Enabled: true,
				Webhooks: []WebhookConfig{
					{URL: "https://example.com", Timeout: "invalid-time"},
				},
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.cfg)
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
