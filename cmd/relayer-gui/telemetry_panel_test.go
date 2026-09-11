package main

import (
	"context"
	"testing"
	"time"

	appcore "github.com/Hocsman/Relayer/internal/app"
	"github.com/Hocsman/Relayer/internal/telemetry"
)

type telemetryMockEngine struct {
	fakeDesktopEngine
	metadata appcore.DesktopMetadata
	snapshot telemetry.Snapshot
}

func (m *telemetryMockEngine) Metadata() appcore.DesktopMetadata {
	return m.metadata
}

func (m *telemetryMockEngine) TelemetrySnapshot() telemetry.Snapshot {
	return m.snapshot
}

func TestGetTelemetrySnapshotWhenIdle(t *testing.T) {
	app := NewApp()
	snapshot, err := app.GetTelemetrySnapshot()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if snapshot.DecisionsTotal != 0 {
		t.Fatalf("expected 0 decisions, got %d", snapshot.DecisionsTotal)
	}
	if len(snapshot.DecisionDurations) == 0 {
		t.Fatal("expected default latency buckets, got empty list")
	}
	if snapshot.AverageReactionTime != 0 {
		t.Fatalf("expected 0 average latency, got %f", snapshot.AverageReactionTime)
	}
}

func TestGetTelemetrySnapshotWithData(t *testing.T) {
	app := NewApp()

	mock := &telemetryMockEngine{
		metadata: appcore.DesktopMetadata{
			TelemetryEnabled: true,
			TelemetryProm:    ":9090",
		},
		snapshot: telemetry.Snapshot{
			Timestamp:            time.Date(2026, 9, 11, 19, 0, 0, 0, time.UTC),
			SessionsActive:       []telemetry.MetricSample{{Value: 2}},
			EventsPending:        1,
			SessionsTotal:        []telemetry.MetricSample{{Value: 5}},
			EventsDetectedTotal:  []telemetry.MetricSample{{Value: 12}},
			EventsWithdrawnTotal: []telemetry.MetricSample{{Value: 1}},
			DecisionsTotal: []telemetry.MetricSample{
				{Labels: map[string]string{"decision": "allow", "decision_by": "human"}, Value: 6},
				{Labels: map[string]string{"decision": "deny", "decision_by": "human"}, Value: 2},
				{Labels: map[string]string{"decision": "allow", "decision_by": "policy"}, Value: 3},
				{Labels: map[string]string{"decision": "deny", "decision_by": "policy"}, Value: 1},
				{Labels: map[string]string{"decision": "modify", "decision_by": "human"}, Value: 1},
			},
			OperatorInputsTotal: []telemetry.MetricSample{{Value: 3}},
			GuardrailsViolations: []telemetry.MetricSample{
				{Labels: map[string]string{"rule": "destructive_command"}, Value: 2},
				{Labels: map[string]string{"rule": "sensitive_paths"}, Value: 1},
			},
			DecisionDurations: []telemetry.HistogramSample{
				{
					Count: 8,
					Sum:   14.4, // avg = 1.8s
					Buckets: map[float64]uint64{
						0.5:   1, // <0.5s: 1
						1.0:   3, // 0.5-1s: 2
						2.0:   6, // 1-2s: 3
						5.0:   8, // 2-5s: 2
						10.0:  8,
						30.0:  8,
						60.0:  8,
						300.0: 8,
					},
				},
			},
		},
	}

	app.active = &runGeneration{
		id:     "run-test-telemetry",
		engine: mock,
		ctx:    context.Background(),
	}

	view, err := app.GetTelemetrySnapshot()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !view.Enabled || !view.PrometheusEnabled || view.PrometheusAddress != ":9090" {
		t.Fatalf("unexpected exporter state: %#v", view)
	}

	if view.SessionsActive != 2 || view.EventsPending != 1 || view.SessionsTotal != 5 {
		t.Fatalf("unexpected session metrics: %#v", view)
	}

	if view.DecisionsTotal != 13 {
		t.Fatalf("expected 13 total decisions, got %d", view.DecisionsTotal)
	}
	if view.DecisionsBreakdown.Allow != 6 ||
		view.DecisionsBreakdown.Deny != 2 ||
		view.DecisionsBreakdown.AutoAllow != 3 ||
		view.DecisionsBreakdown.AutoDeny != 1 ||
		view.DecisionsBreakdown.Custom != 1 {
		t.Fatalf("unexpected decisions breakdown: %#v", view.DecisionsBreakdown)
	}

	if view.GuardrailsTotal != 3 {
		t.Fatalf("expected 3 guardrails total, got %d", view.GuardrailsTotal)
	}
	if view.GuardrailsBreakdown["destructive_command"] != 2 || view.GuardrailsBreakdown["sensitive_paths"] != 1 {
		t.Fatalf("unexpected guardrails breakdown: %#v", view.GuardrailsBreakdown)
	}

	if view.AverageReactionTime != 1.8 {
		t.Fatalf("expected 1.8s average reaction time, got %f", view.AverageReactionTime)
	}

	// Verify discrete bucket counts
	// <0.5s: 1
	// 0.5-1s: 2
	// 1-2s: 3
	// 2-5s: 2
	if len(view.DecisionDurations) < 4 {
		t.Fatalf("expected at least 4 buckets, got %d", len(view.DecisionDurations))
	}
	if view.DecisionDurations[0].Count != 1 ||
		view.DecisionDurations[1].Count != 2 ||
		view.DecisionDurations[2].Count != 3 ||
		view.DecisionDurations[3].Count != 2 {
		t.Fatalf("unexpected discrete bucket counts: %#v", view.DecisionDurations)
	}
}
