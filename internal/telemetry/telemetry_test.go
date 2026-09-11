package telemetry

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/audit"
)

func TestConfigValidation(t *testing.T) {
	// Disabled config is always valid
	disabled := DefaultConfig()
	disabled.Enabled = false
	if err := Validate(disabled); err != nil {
		t.Fatalf("disabled config should be valid: %v", err)
	}

	// Default enabled config
	enabled := DefaultConfig()
	enabled.Enabled = true
	if err := Validate(enabled); err != nil {
		t.Fatalf("default enabled config should be valid: %v", err)
	}

	// Empty service name
	badService := enabled
	badService.ServiceName = ""
	if err := Validate(badService); err == nil {
		t.Fatal("empty service_name should be rejected")
	}

	// Invalid prometheus path
	badPath := enabled
	badPath.Prometheus.Path = "metrics"
	if err := Validate(badPath); err == nil {
		t.Fatal("prometheus path without leading slash should be rejected")
	}

	// Invalid OTLP endpoint
	badOTLP := enabled
	badOTLP.OTLP.Enabled = true
	badOTLP.OTLP.Endpoint = "ftp://localhost:4318"
	if err := Validate(badOTLP); err == nil {
		t.Fatal("non-http otlp endpoint should be rejected")
	}

	// Valid OTLP
	goodOTLP := enabled
	goodOTLP.OTLP.Enabled = true
	goodOTLP.OTLP.Endpoint = "http://127.0.0.1:4318/v1/metrics"
	if err := Validate(goodOTLP); err != nil {
		t.Fatalf("valid otlp config failed: %v", err)
	}
}

func TestRegistryObservationAndPrometheusRendering(t *testing.T) {
	reg := NewRegistry()
	now := time.Now().UTC()

	// 1. Session started
	reg.Observe(audit.Entry{
		Kind:      audit.KindSessionStarted,
		AgentID:   "claude-code",
		Adapter:   "claude",
		Backend:   "pty",
		Timestamp: now,
	})

	// 2. Event detected
	reg.Observe(audit.Entry{
		RunID:     "run-1",
		SessionID: "sess-1",
		EventID:   "ev-1",
		Kind:      audit.KindEventDetected,
		AgentID:   "claude-code",
		Adapter:   "claude",
		EventType: adapters.EventConfirmation,
		Risk:      adapters.RiskHigh,
		Sensitive: true,
		Timestamp: now,
	})

	snap1 := reg.Snapshot()
	if snap1.EventsPending != 1 {
		t.Fatalf("expected 1 pending event, got %d", snap1.EventsPending)
	}
	if len(snap1.SessionsActive) != 1 || snap1.SessionsActive[0].Value != 1 {
		t.Fatalf("expected 1 active session, got %#v", snap1.SessionsActive)
	}

	// 3. Decision recorded (latency 250ms)
	reg.Observe(audit.Entry{
		RunID:      "run-1",
		SessionID:  "sess-1",
		EventID:    "ev-1",
		Kind:       audit.KindDecision,
		AgentID:    "claude-code",
		Adapter:    "claude",
		Decision:   audit.DecisionAllow,
		DecisionBy: audit.DecisionByHuman,
		Outcome:    audit.OutcomeApplied,
		Rule:       "operator_allow",
		Timestamp:  now.Add(250 * time.Millisecond),
	})

	// 4. Guardrail violation
	reg.Observe(audit.Entry{
		Kind:       audit.KindPolicyEvaluated,
		AgentID:    "claude-code",
		Adapter:    "claude",
		Decision:   audit.DecisionDeny,
		DecisionBy: audit.DecisionByPolicy,
		Outcome:    audit.OutcomeFailed,
		Rule:       "block_sensitive_paths",
		Reason:     "sensitive_path_blocked",
		Timestamp:  now.Add(300 * time.Millisecond),
	})

	// 5. Operator input
	reg.Observe(audit.Entry{
		Kind:      audit.KindOperatorInput,
		AgentID:   "claude-code",
		Timestamp: now.Add(400 * time.Millisecond),
	})

	// 6. Session finished
	reg.Observe(audit.Entry{
		Kind:      audit.KindSessionFinished,
		AgentID:   "claude-code",
		Backend:   "pty",
		Timestamp: now.Add(500 * time.Millisecond),
	})

	snap2 := reg.Snapshot()
	if snap2.EventsPending != 0 {
		t.Fatalf("expected 0 pending events, got %d", snap2.EventsPending)
	}
	if len(snap2.SessionsActive) != 1 || snap2.SessionsActive[0].Value != 0 {
		t.Fatalf("expected 0 active sessions, got %#v", snap2.SessionsActive)
	}
	if len(snap2.GuardrailsViolations) != 1 {
		t.Fatalf("expected 1 guardrail violation, got %#v", snap2.GuardrailsViolations)
	}
	if len(snap2.DecisionDurations) != 1 || snap2.DecisionDurations[0].Count != 1 {
		t.Fatalf("expected 1 duration recorded, got %#v", snap2.DecisionDurations)
	}

	// Render Prometheus text format
	promText := string(RenderPrometheus(snap2))

	expectedMarkers := []string{
		"relayer_sessions_active{backend=\"pty\"} 0",
		"relayer_events_pending 0",
		"relayer_sessions_total{adapter=\"claude\",agent_id=\"claude-code\",backend=\"pty\"} 1",
		"relayer_events_detected_total{adapter=\"claude\",agent_id=\"claude-code\",event_type=\"confirmation\",risk=\"high\",sensitive=\"true\"} 1",
		"relayer_decisions_total{adapter=\"claude\",agent_id=\"claude-code\",decision=\"allow\",decision_by=\"human\",outcome=\"applied\",rule=\"operator_allow\"} 1",
		"relayer_operator_inputs_total{agent_id=\"claude-code\"} 1",
		"relayer_guardrail_violations_total{reason=\"sensitive_path_blocked\",rule=\"block_sensitive_paths\"} 1",
		"relayer_decision_duration_seconds_bucket",
		"relayer_decision_duration_seconds_count",
		"relayer_decision_duration_seconds_sum",
	}

	for _, marker := range expectedMarkers {
		if !strings.Contains(promText, marker) {
			t.Errorf("rendered Prometheus text missing expected marker %q:\n%s", marker, promText)
		}
	}
}

func TestPrometheusServerLifecycle(t *testing.T) {
	reg := NewRegistry()
	cfg := PrometheusConfig{
		Enabled: true,
		Address: "127.0.0.1:0", // ephemeral port
		Path:    "/metrics",
	}
	server := NewPrometheusServer(cfg, reg)
	if err := server.Start(); err != nil {
		t.Fatalf("start prometheus server: %v", err)
	}
	defer func() { _ = server.Close() }()

	addr := server.Address()
	if addr == "" {
		t.Fatal("empty server address")
	}

	resp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "relayer_events_pending") {
		t.Fatalf("missing expected metrics in body: %s", string(body))
	}
}

func TestOTLPPayloadGeneration(t *testing.T) {
	reg := NewRegistry()
	reg.Observe(audit.Entry{
		Kind:      audit.KindSessionStarted,
		AgentID:   "agent-x",
		Backend:   "pty",
		Timestamp: time.Now().UTC(),
	})

	snap := reg.Snapshot()
	payloadBytes, err := BuildOTLPPayload(snap, "relayer-test", "staging")
	if err != nil {
		t.Fatalf("build otlp payload: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(payloadBytes, &parsed); err != nil {
		t.Fatalf("invalid json generated: %v", err)
	}

	rm, ok := parsed["resourceMetrics"].([]any)
	if !ok || len(rm) == 0 {
		t.Fatal("missing resourceMetrics")
	}
}

func TestOTLPExporterExportOnce(t *testing.T) {
	var receivedBody []byte
	var receivedContentType string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedContentType = r.Header.Get("Content-Type")
		var err error
		receivedBody, err = io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	reg := NewRegistry()
	reg.Observe(audit.Entry{
		Kind:      audit.KindSessionStarted,
		AgentID:   "test-agent",
		Backend:   "pty",
		Timestamp: time.Now().UTC(),
	})

	exporter := NewOTLPExporter(
		OTLPConfig{
			Enabled:  true,
			Endpoint: server.URL,
			Timeout:  2 * time.Second,
		},
		reg,
		"relayer-test",
		"test",
	)

	if err := exporter.ExportOnce(context.Background()); err != nil {
		t.Fatalf("export once failed: %v", err)
	}

	if receivedContentType != "application/json" {
		t.Fatalf("expected application/json, got %q", receivedContentType)
	}
	if len(receivedBody) == 0 {
		t.Fatal("expected non-empty otlp body")
	}
}

func TestTelemetryEngineLifecycle(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.Prometheus.Address = "127.0.0.1:0"

	engine, err := NewEngine(cfg)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	if !engine.Enabled() {
		t.Fatal("engine should be enabled")
	}

	ctx := context.Background()
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("start engine: %v", err)
	}

	// Verify registry is observer
	reg := engine.Registry()
	reg.Observe(audit.Entry{
		Kind:      audit.KindSessionStarted,
		AgentID:   "test",
		Backend:   "pty",
		Timestamp: time.Now().UTC(),
	})

	if err := engine.Close(); err != nil {
		t.Fatalf("close engine: %v", err)
	}
}
