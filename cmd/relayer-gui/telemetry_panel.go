package main

import (
	"sort"
	"strings"
	"time"

	"github.com/Hocsman/Relayer/internal/telemetry"
)

// DecisionBreakdown groups decisions into intuitive operational categories.
type DecisionBreakdown struct {
	Allow     int64 `json:"allow"`
	Deny      int64 `json:"deny"`
	AutoAllow int64 `json:"autoAllow"`
	AutoDeny  int64 `json:"autoDeny"`
	Custom    int64 `json:"custom"`
}

// LatencyBucketView represents one discrete bucket in the human reaction latency distribution.
type LatencyBucketView struct {
	Le    float64 `json:"le"`
	Label string  `json:"label"`
	Count uint64  `json:"count"`
}

// TelemetrySnapshotView represents the sanitized, display-safe metrics snapshot for the frontend.
type TelemetrySnapshotView struct {
	Timestamp            string              `json:"timestamp"`
	Enabled              bool                `json:"enabled"`
	PrometheusEnabled    bool                `json:"prometheusEnabled"`
	PrometheusAddress    string              `json:"prometheusAddress,omitempty"`
	OTLPEnabled          bool                `json:"otlpEnabled"`
	OTLPEndpoint         string              `json:"otlpEndpoint,omitempty"`
	SessionsActive       int64               `json:"sessionsActive"`
	EventsPending        int64               `json:"eventsPending"`
	SessionsTotal        int64               `json:"sessionsTotal"`
	EventsDetectedTotal  int64               `json:"eventsDetectedTotal"`
	EventsWithdrawnTotal int64               `json:"eventsWithdrawnTotal"`
	DecisionsTotal       int64               `json:"decisionsTotal"`
	DecisionsBreakdown   DecisionBreakdown   `json:"decisionsBreakdown"`
	OperatorInputsTotal  int64               `json:"operatorInputsTotal"`
	GuardrailsTotal      int64               `json:"guardrailsTotal"`
	GuardrailsBreakdown  map[string]int64    `json:"guardrailsBreakdown"`
	AverageReactionTime  float64             `json:"averageReactionTime"`
	DecisionDurations    []LatencyBucketView `json:"decisionDurations"`
}

// GetTelemetrySnapshot extracts a point-in-time snapshot of metrics from the active engine
// and returns a frontend-tailored DTO with calculated ratios, averages, and histogram buckets.
func (a *App) GetTelemetrySnapshot() (TelemetrySnapshotView, error) {
	a.mu.RLock()
	active := a.active
	a.mu.RUnlock()

	emptyView := TelemetrySnapshotView{
		Timestamp:           time.Now().UTC().Format(time.RFC3339),
		GuardrailsBreakdown: make(map[string]int64),
		DecisionDurations:   buildDefaultLatencyBuckets(),
	}

	if active == nil || active.engine == nil {
		return emptyView, nil
	}

	engine := active.engine
	metadata := engine.Metadata()
	raw := engine.TelemetrySnapshot()

	view := TelemetrySnapshotView{
		Timestamp:            raw.Timestamp.Format(time.RFC3339),
		Enabled:              metadata.TelemetryEnabled,
		PrometheusEnabled:    metadata.TelemetryProm != "",
		PrometheusAddress:    metadata.TelemetryProm,
		SessionsActive:       sumMetricSampleValues(raw.SessionsActive),
		EventsPending:        raw.EventsPending,
		SessionsTotal:        sumMetricSampleValues(raw.SessionsTotal),
		EventsDetectedTotal:  sumMetricSampleValues(raw.EventsDetectedTotal),
		EventsWithdrawnTotal: sumMetricSampleValues(raw.EventsWithdrawnTotal),
		DecisionsTotal:       sumMetricSampleValues(raw.DecisionsTotal),
		OperatorInputsTotal:  sumMetricSampleValues(raw.OperatorInputsTotal),
		GuardrailsBreakdown:  make(map[string]int64),
	}

	// 1. Decisions breakdown
	for _, sample := range raw.DecisionsTotal {
		dec := strings.ToLower(sample.Labels["decision"])
		by := strings.ToLower(sample.Labels["decision_by"])
		val := int64(sample.Value)

		switch {
		case by == "policy" || by == "auto":
			if dec == "allow" {
				view.DecisionsBreakdown.AutoAllow += val
			} else {
				view.DecisionsBreakdown.AutoDeny += val
			}
		default: // human
			switch dec {
			case "allow":
				view.DecisionsBreakdown.Allow += val
			case "deny", "abort":
				view.DecisionsBreakdown.Deny += val
			default:
				view.DecisionsBreakdown.Custom += val
			}
		}
	}

	// 2. Guardrails violations breakdown
	for _, sample := range raw.GuardrailsViolations {
		rule := sample.Labels["rule"]
		if rule == "" {
			rule = "general"
		}
		val := int64(sample.Value)
		view.GuardrailsBreakdown[rule] += val
		view.GuardrailsTotal += val
	}

	// 3. Latency duration averages and histogram
	view.DecisionDurations, view.AverageReactionTime = computeLatencyDistribution(raw.DecisionDurations)

	return view, nil
}

func sumMetricSampleValues(samples []telemetry.MetricSample) int64 {
	var total int64
	for _, s := range samples {
		total += int64(s.Value)
	}
	return total
}

func buildDefaultLatencyBuckets() []LatencyBucketView {
	return []LatencyBucketView{
		{Le: 0.5, Label: "< 0.5s", Count: 0},
		{Le: 1.0, Label: "0.5s - 1s", Count: 0},
		{Le: 2.0, Label: "1s - 2s", Count: 0},
		{Le: 5.0, Label: "2s - 5s", Count: 0},
		{Le: 10.0, Label: "5s - 10s", Count: 0},
		{Le: 30.0, Label: "10s - 30s", Count: 0},
		{Le: 60.0, Label: "30s - 60s", Count: 0},
		{Le: 300.0, Label: "> 60s", Count: 0},
	}
}

func computeLatencyDistribution(samples []telemetry.HistogramSample) ([]LatencyBucketView, float64) {
	thresholds := []struct {
		le    float64
		label string
	}{
		{0.5, "< 0.5s"},
		{1.0, "0.5s - 1s"},
		{2.0, "1s - 2s"},
		{5.0, "2s - 5s"},
		{10.0, "5s - 10s"},
		{30.0, "10s - 30s"},
		{60.0, "30s - 60s"},
		{300.0, "> 60s"},
	}

	// Sum cumulative counts across all series
	cumulative := make(map[float64]uint64)
	var totalSum float64
	var totalCount uint64

	for _, sample := range samples {
		totalSum += sample.Sum
		totalCount += sample.Count
		for le, count := range sample.Buckets {
			cumulative[le] += count
		}
	}

	var avg float64
	if totalCount > 0 {
		avg = totalSum / float64(totalCount)
	}

	// Calculate discrete bucket counts: C_i = B_i - B_{i-1}
	buckets := make([]LatencyBucketView, len(thresholds))
	var prevCount uint64
	for i, t := range thresholds {
		cumCount := cumulative[t.le]
		var discrete uint64
		if cumCount >= prevCount {
			discrete = cumCount - prevCount
		}
		buckets[i] = LatencyBucketView{
			Le:    t.le,
			Label: t.label,
			Count: discrete,
		}
		prevCount = cumCount
	}

	// Sort buckets ascending by Le
	sort.Slice(buckets, func(i, j int) bool {
		return buckets[i].Le < buckets[j].Le
	})

	return buckets, avg
}
