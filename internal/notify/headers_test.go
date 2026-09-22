package notify

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMergeWebhookHeadersKeepsCredentialsTheEditorNeverSaw(t *testing.T) {
	existing := []WebhookConfig{
		{Name: "team", URL: "https://hooks.example/a", Headers: map[string]string{"Authorization": "Bearer s3cret"}},
		{Name: "ops", URL: "https://hooks.example/b", Headers: map[string]string{"X-Token": "t0ken"}},
		{Name: "dup-1", URL: "https://hooks.example/dup", Headers: map[string]string{"X": "1"}},
		{Name: "dup-2", URL: "https://hooks.example/dup", Headers: map[string]string{"X": "2"}},
	}
	edited := []WebhookConfig{
		{Name: "team", URL: "https://hooks.example/a"},                                          // unchanged: keep
		{Name: "renamed", URL: "https://hooks.example/b"},                                       // renamed, same URL: keep
		{Name: "moved", URL: "https://hooks.example/new"},                                       // new URL: the old credential stays behind
		{Name: "dup-2", URL: "https://hooks.example/dup"},                                       // ambiguous URL, matched by name
		{Name: "dup-3", URL: "https://hooks.example/dup"},                                       // ambiguous URL, no name match: nothing
		{Name: "own", URL: "https://hooks.example/a", Headers: map[string]string{"Own": "yes"}}, // supplied its own: untouched
	}
	merged := MergeWebhookHeaders(existing, edited)

	want := []map[string]string{
		{"Authorization": "Bearer s3cret"},
		{"X-Token": "t0ken"},
		nil,
		{"X": "2"},
		nil,
		{"Own": "yes"},
	}
	for index, headers := range want {
		if !reflect.DeepEqual(merged[index].Headers, headers) {
			t.Errorf("webhook %q headers = %v, want %v", merged[index].Name, merged[index].Headers, headers)
		}
	}

	merged[0].Headers["Authorization"] = "changed"
	if existing[0].Headers["Authorization"] != "Bearer s3cret" {
		t.Fatal("editing a merged webhook's headers edited the source configuration")
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestFailedWebhookIsReportedWithoutItsURL: a failure used to leave no trace,
// and the errors it produced quoted the URL — which for Slack and Discord is the
// credential itself.
func TestFailedWebhookIsReportedWithoutItsURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	secretURL := server.URL + "/services/T000/B000/SECRET-WEBHOOK-TOKEN"
	var diagnostics lockedBuffer
	notifier := NewWithDiagnostics(Config{
		Enabled:     true,
		MinSeverity: SeverityInfo,
		Webhooks:    []WebhookConfig{{Name: "team-slack", URL: secretURL, MinSeverity: SeverityInfo, Timeout: "2s"}},
	}, &bytes.Buffer{}, &diagnostics)
	notifier.Notify(Notification{Title: "t", EventID: "e1", Severity: SeverityWarning, Timestamp: time.Now()})

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(diagnostics.String(), "team-slack") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	line := diagnostics.String()
	if !strings.Contains(line, `webhook "team-slack" was not delivered`) || !strings.Contains(line, "HTTP 503") {
		t.Fatalf("diagnostics = %q, want the webhook's name and the status", line)
	}
	if strings.Contains(line, "SECRET-WEBHOOK-TOKEN") || strings.Contains(line, server.URL) {
		t.Fatalf("diagnostics carry the webhook URL: %q", line)
	}
}

// TestMergeWebhookHeadersNeverLendsACredentialToASibling: two webhooks share a
// URL and only one has a credential. The URL is ambiguous, so the one without
// keeps having none; counting only webhooks with headers made the URL look
// unambiguous and copied the credential onto the sibling on every save.
func TestMergeWebhookHeadersNeverLendsACredentialToASibling(t *testing.T) {
	existing := []WebhookConfig{
		{Name: "authed", URL: "https://hooks.example/u", Headers: map[string]string{"Authorization": "Bearer A"}},
		{Name: "plain", URL: "https://hooks.example/u"},
	}
	edited := []WebhookConfig{
		{Name: "authed", URL: "https://hooks.example/u"},
		{Name: "plain", URL: "https://hooks.example/u"},
	}
	merged := MergeWebhookHeaders(existing, edited)
	if merged[0].Headers["Authorization"] != "Bearer A" {
		t.Fatalf("authed lost its credential: %v", merged[0].Headers)
	}
	if len(merged[1].Headers) != 0 {
		t.Fatalf("plain was given %v, a credential it never had", merged[1].Headers)
	}

	// Removing the authed webhook must not move its credential to the other.
	remaining := MergeWebhookHeaders(existing, edited[1:])
	if len(remaining[0].Headers) != 0 {
		t.Fatalf("after removing authed, plain holds %v", remaining[0].Headers)
	}
}
