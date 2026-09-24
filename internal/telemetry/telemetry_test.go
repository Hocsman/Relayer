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

// TestHandoverAndRecordingEventsAreCounted covers the eleven kinds the registry
// used to fall through: a forced takeover or a failed recording reached the
// journal but never a dashboard.
func TestHandoverAndRecordingEventsAreCounted(t *testing.T) {
	reg := NewRegistry()
	observe := func(kind audit.Kind, outcome audit.Outcome) {
		reg.Observe(audit.Entry{Kind: kind, AgentID: "claude-code", Outcome: outcome, Timestamp: time.Now().UTC()})
	}

	observe(audit.KindAttachStarted, audit.OutcomeApplied)
	observe(audit.KindControlRequested, audit.OutcomePending)
	observe(audit.KindControlGranted, audit.OutcomeApplied)
	observe(audit.KindControlDeclined, audit.OutcomeApplied)
	observe(audit.KindControlReleased, audit.OutcomeApplied)
	observe(audit.KindControlForced, audit.OutcomeFailed)
	observe(audit.KindControlForced, audit.OutcomeFailed)
	observe(audit.KindAttachFinished, audit.OutcomeApplied)
	observe(audit.KindRecordingStarted, audit.OutcomeStarted)
	observe(audit.KindRecordingFinished, audit.OutcomeFinished)
	observe(audit.KindRecordingExported, audit.OutcomeApplied)
	observe(audit.KindRecordingDeleted, audit.OutcomeApplied)
	observe(audit.KindRecordingStarted, audit.OutcomeFailed)

	snap := reg.Snapshot()
	promText := string(RenderPrometheus(snap))
	for _, marker := range []string{
		`relayer_control_events_total{action="attached",agent_id="claude-code",outcome="applied"} 1`,
		`relayer_control_events_total{action="requested",agent_id="claude-code",outcome="pending"} 1`,
		`relayer_control_events_total{action="granted",agent_id="claude-code",outcome="applied"} 1`,
		`relayer_control_events_total{action="declined",agent_id="claude-code",outcome="applied"} 1`,
		`relayer_control_events_total{action="released",agent_id="claude-code",outcome="applied"} 1`,
		`relayer_control_events_total{action="forced",agent_id="claude-code",outcome="failed"} 2`,
		`relayer_control_events_total{action="detached",agent_id="claude-code",outcome="applied"} 1`,
		`relayer_recording_events_total{action="started",agent_id="claude-code",outcome="started"} 1`,
		`relayer_recording_events_total{action="started",agent_id="claude-code",outcome="failed"} 1`,
		`relayer_recording_events_total{action="finished",agent_id="claude-code",outcome="finished"} 1`,
		`relayer_recording_events_total{action="exported",agent_id="claude-code",outcome="applied"} 1`,
		`relayer_recording_events_total{action="deleted",agent_id="claude-code",outcome="applied"} 1`,
	} {
		if !strings.Contains(promText, marker) {
			t.Errorf("rendered Prometheus text missing %q:\n%s", marker, promText)
		}
	}

	payload, err := BuildOTLPPayload(snap, "relayer-test", "test")
	if err != nil {
		t.Fatalf("build otlp payload: %v", err)
	}
	for _, name := range []string{"relayer.control.events.total", "relayer.recording.events.total"} {
		if !strings.Contains(string(payload), name) {
			t.Errorf("OTLP payload missing %q", name)
		}
	}
}

// TestProcessExitsAreNeverPending: an exit is journaled as a detected event but
// is never decided, so each one stayed in events_pending forever.
func TestProcessExitsAreNeverPending(t *testing.T) {
	reg := NewRegistry()
	for i, id := range []string{"exit-1", "exit-2", "exit-3"} {
		reg.Observe(audit.Entry{
			RunID:     "run-1",
			SessionID: "agent",
			EventID:   id,
			Kind:      audit.KindEventDetected,
			EventType: adapters.EventProcessExit,
			Timestamp: time.Now().UTC().Add(time.Duration(i) * time.Second),
		})
	}
	if pending := reg.Snapshot().EventsPending; pending != 0 {
		t.Fatalf("events_pending after three process exits = %d, want 0", pending)
	}
}

// TestPromptsOfAnEndedProcessAreNotPending: a prompt pending when its process
// exits, or when a restart replaces it, is never decided, and each one stayed
// in events_pending for good.
func TestPromptsOfAnEndedProcessAreNotPending(t *testing.T) {
	reg := NewRegistry()
	detect := func(session, id string) {
		reg.Observe(audit.Entry{RunID: "run-1", SessionID: session, EventID: id, Kind: audit.KindEventDetected, EventType: adapters.EventConfirmation, Timestamp: time.Now().UTC()})
	}
	detect("exits", "evt-1")
	detect("restarts", "evt-2")
	detect("keeps", "evt-3")
	reg.Observe(audit.Entry{RunID: "run-1", SessionID: "exits", Kind: audit.KindSessionFinished, Timestamp: time.Now().UTC()})
	reg.Observe(audit.Entry{RunID: "run-1", SessionID: "restarts", Kind: audit.KindSessionStarted, Timestamp: time.Now().UTC()})
	if pending := reg.Snapshot().EventsPending; pending != 1 {
		t.Fatalf("events_pending = %d, want only the live session's prompt", pending)
	}
}

// TestEachDecisionIsCountedOnce: every prompt is journaled policy_evaluated,
// and the prompt's decision, by the policy or a human, is journaled once more
// as a decision. relayer_decisions_total counted both, so each decision was
// counted twice, the policy's evaluation of a prompt a human then answered
// was counted as a decision of its own, and a prompt nobody answered yet
// already counted as decided. It counts decision entries alone; the guardrail
// counter still reads the policy's evaluations, where a guardrail's reason is.
func TestEachDecisionIsCountedOnce(t *testing.T) {
	reg := NewRegistry()
	now := time.Now().UTC()
	observe := func(entry audit.Entry) {
		now = now.Add(time.Millisecond)
		entry.RunID, entry.SessionID, entry.AgentID, entry.Adapter = "run-1", "agent", "agent", "generic"
		entry.Timestamp = now
		reg.Observe(entry)
	}
	// The policy answers one prompt on its own.
	observe(audit.Entry{Kind: audit.KindEventDetected, EventID: "evt-auto"})
	observe(audit.Entry{
		Kind: audit.KindPolicyEvaluated, EventID: "evt-auto", Decision: audit.DecisionAllow,
		DecisionBy: audit.DecisionByPolicy, Outcome: audit.OutcomeInFlight, Rule: "allow-safe", Reason: "rule_match",
	})
	observe(audit.Entry{
		Kind: audit.KindDecision, EventID: "evt-auto", Decision: audit.DecisionAllow,
		DecisionBy: audit.DecisionByPolicy, Outcome: audit.OutcomeInFlight, Rule: "allow-safe", Reason: "decision_selected",
	})
	// A guardrail sends one to a human, who answers it.
	observe(audit.Entry{Kind: audit.KindEventDetected, EventID: "evt-ask"})
	observe(audit.Entry{
		Kind: audit.KindPolicyEvaluated, EventID: "evt-ask", Decision: audit.DecisionDeny,
		DecisionBy: audit.DecisionByPolicy, Outcome: audit.OutcomeAsk, Rule: "no-rm", Reason: "destructive_command_blocked",
	})
	observe(audit.Entry{
		Kind: audit.KindDecision, EventID: "evt-ask", Decision: audit.DecisionDeny,
		DecisionBy: audit.DecisionByHuman, Outcome: audit.OutcomeInFlight, Reason: "decision_selected",
	})
	// A third waits on a human.
	observe(audit.Entry{Kind: audit.KindEventDetected, EventID: "evt-waiting"})
	observe(audit.Entry{
		Kind: audit.KindPolicyEvaluated, EventID: "evt-waiting", Decision: audit.DecisionAsk,
		DecisionBy: audit.DecisionByPolicy, Outcome: audit.OutcomeAsk, Reason: "default_action",
	})

	snapshot := reg.Snapshot()
	total := 0.0
	for _, sample := range snapshot.DecisionsTotal {
		total += sample.Value
	}
	if total != 2 || len(snapshot.DecisionsTotal) != 2 {
		t.Fatalf("decisions_total = %#v, want the two decisions once each", snapshot.DecisionsTotal)
	}
	promText := string(RenderPrometheus(snapshot))
	for _, marker := range []string{
		`relayer_decisions_total{adapter="generic",agent_id="agent",decision="allow",decision_by="policy",outcome="in_flight",rule="allow-safe"} 1`,
		`relayer_decisions_total{adapter="generic",agent_id="agent",decision="deny",decision_by="human",outcome="in_flight",rule="default"} 1`,
		`relayer_guardrail_violations_total{reason="destructive_command_blocked",rule="no-rm"} 1`,
	} {
		if !strings.Contains(promText, marker) {
			t.Errorf("rendered Prometheus text missing %q:\n%s", marker, promText)
		}
	}
	if snapshot.EventsPending != 1 {
		t.Fatalf("events_pending = %d, want the prompt still waiting on a human", snapshot.EventsPending)
	}
}

// TestAStaleExitLeavesTheReplacementCounted: a restart journals the previous
// process's end (operator_restart) and the replacement's start, and the
// previous process's own exit can reach the journal after both. The core
// journals that exit session_finished with reason process_exit_stale, and it
// ends nothing the registry counts: the session active and the prompts
// pending are the replacement's. Read as an ordinary exit, it dropped the
// replacement's pending prompts and decremented relayer_sessions_active under
// a session that was still running, so the gauge read zero, or one short
// with other agents running.
func TestAStaleExitLeavesTheReplacementCounted(t *testing.T) {
	reg := NewRegistry()
	now := time.Now().UTC()
	observe := func(session string, kind audit.Kind, reason, eventID string) {
		now = now.Add(time.Millisecond)
		reg.Observe(audit.Entry{
			RunID: "run-1", SessionID: session, AgentID: session, Backend: "pty", EventID: eventID,
			Kind: kind, Reason: reason, EventType: adapters.EventConfirmation, Timestamp: now,
		})
	}
	active := func() float64 {
		t.Helper()
		samples := reg.Snapshot().SessionsActive
		if len(samples) != 1 {
			t.Fatalf("sessions_active = %#v, want one pty series", samples)
		}
		return samples[0].Value
	}
	observe("other", audit.KindSessionStarted, "", "")
	observe("agent", audit.KindSessionStarted, "", "")
	observe("agent", audit.KindEventDetected, "event_detected", "evt-old")
	observe("agent", audit.KindSessionFinished, "operator_restart", "")
	observe("agent", audit.KindSessionStarted, "operator_restart", "")
	observe("agent", audit.KindEventDetected, "event_detected", "evt-new")
	if got, pending := active(), reg.Snapshot().EventsPending; got != 2 || pending != 1 {
		t.Fatalf("after the restart: sessions_active = %v, events_pending = %d; want 2 and the replacement's prompt", got, pending)
	}

	observe("agent", audit.KindSessionFinished, "process_exit_stale", "")
	if got := active(); got != 2 {
		t.Fatalf("sessions_active after the previous process's stale exit = %v, want both sessions still counted", got)
	}
	if pending := reg.Snapshot().EventsPending; pending != 1 {
		t.Fatalf("events_pending after the previous process's stale exit = %d, want the replacement's prompt still pending", pending)
	}

	observe("agent", audit.KindSessionFinished, "process_exit", "")
	if got := active(); got != 1 {
		t.Fatalf("sessions_active after the replacement's own exit = %v, want the other session's", got)
	}
	if pending := reg.Snapshot().EventsPending; pending != 0 {
		t.Fatalf("events_pending after the replacement's own exit = %d, want 0", pending)
	}
}

// TestAPromptIsPendingUntilItIsDecided: the front ends journal the policy's
// evaluation right after each detection, and the registry closed the prompt
// there, so events_pending stayed at zero while an operator had a question in
// front of them, and the decision duration measured the policy engine. A
// prompt now stays pending until its decision, a withdrawal, or the end of
// its process; a sensitive prompt, whose ID the journal withholds, too.
func TestAPromptIsPendingUntilItIsDecided(t *testing.T) {
	reg := NewRegistry()
	now := time.Now().UTC()
	entry := func(kind audit.Kind, session, id string, at time.Duration) audit.Entry {
		return audit.Entry{RunID: "run-1", SessionID: session, EventID: id, Kind: kind, EventType: adapters.EventConfirmation, DecisionBy: audit.DecisionByHuman, Timestamp: now.Add(at)}
	}
	reg.Observe(entry(audit.KindEventDetected, "asks", "evt-1", 0))
	reg.Observe(entry(audit.KindPolicyEvaluated, "asks", "evt-1", time.Millisecond))
	if pending := reg.Snapshot().EventsPending; pending != 1 {
		t.Fatalf("events_pending after the policy asked the operator = %d, want 1", pending)
	}
	reg.Observe(entry(audit.KindDecision, "asks", "evt-1", 3*time.Second))
	snapshot := reg.Snapshot()
	if snapshot.EventsPending != 0 {
		t.Fatalf("events_pending after the decision = %d, want 0", snapshot.EventsPending)
	}
	recorded := false
	for _, histogram := range snapshot.DecisionDurations {
		if histogram.Count > 0 && histogram.Sum >= 2.9 {
			recorded = true
		}
	}
	if !recorded {
		t.Fatalf("decision durations = %#v, want the three seconds the operator took", snapshot.DecisionDurations)
	}

	// A sensitive prompt is journaled without its ID, and still ends with its
	// process.
	reg.Observe(entry(audit.KindEventDetected, "secret", "", 0))
	reg.Observe(entry(audit.KindSessionFinished, "secret", "", time.Second))
	if pending := reg.Snapshot().EventsPending; pending != 0 {
		t.Fatalf("events_pending after a sensitive prompt's process ended = %d, want 0", pending)
	}
}
