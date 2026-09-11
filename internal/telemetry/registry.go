package telemetry

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Hocsman/Relayer/internal/audit"
)

// DefaultHistogramBuckets defines latency duration buckets in seconds.
var DefaultHistogramBuckets = []float64{0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0, 30.0, 60.0, 120.0, 300.0}

// MetricSample represents a single metric observation with key-value labels.
type MetricSample struct {
	Labels map[string]string `json:"labels"`
	Value  float64           `json:"value"`
}

// HistogramSample represents the aggregated state of a histogram series.
type HistogramSample struct {
	Labels  map[string]string   `json:"labels"`
	Count   uint64              `json:"count"`
	Sum     float64             `json:"sum"`
	Buckets map[float64]uint64  `json:"buckets"`
}

// Snapshot contains an immutable point-in-time capture of all registry metrics.
type Snapshot struct {
	Timestamp            time.Time                  `json:"timestamp"`
	SessionsActive       []MetricSample             `json:"sessions_active"`
	EventsPending        int64                      `json:"events_pending"`
	SessionsTotal        []MetricSample             `json:"sessions_total"`
	EventsDetectedTotal  []MetricSample             `json:"events_detected_total"`
	EventsWithdrawnTotal []MetricSample             `json:"events_withdrawn_total"`
	DecisionsTotal       []MetricSample             `json:"decisions_total"`
	OperatorInputsTotal  []MetricSample             `json:"operator_inputs_total"`
	GuardrailsViolations []MetricSample             `json:"guardrails_violations"`
	DecisionDurations    []HistogramSample          `json:"decision_durations"`
}

// Registry aggregates and tracks metrics from audit events in a thread-safe manner.
type Registry struct {
	mu                   sync.RWMutex
	sessionsActive       map[string]int64
	sessionsTotal        map[string]int64
	eventsDetectedTotal  map[string]int64
	eventsWithdrawnTotal map[string]int64
	decisionsTotal       map[string]int64
	operatorInputsTotal  map[string]int64
	guardrailViolations  map[string]int64
	decisionDurations    map[string]*histogramSeries
	pendingEvents        map[string]time.Time
}

type histogramSeries struct {
	labels  map[string]string
	buckets []float64
	counts  []uint64
	count   uint64
	sum     float64
}

// NewRegistry constructs a clean telemetry registry.
func NewRegistry() *Registry {
	return &Registry{
		sessionsActive:       make(map[string]int64),
		sessionsTotal:        make(map[string]int64),
		eventsDetectedTotal:  make(map[string]int64),
		eventsWithdrawnTotal: make(map[string]int64),
		decisionsTotal:       make(map[string]int64),
		operatorInputsTotal:  make(map[string]int64),
		guardrailViolations:  make(map[string]int64),
		decisionDurations:    make(map[string]*histogramSeries),
		pendingEvents:        make(map[string]time.Time),
	}
}

// Observe implements audit.EntryObserver, updating all metrics corresponding
// to the sanitized audit entry.
func (r *Registry) Observe(entry audit.Entry) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	backend := entry.Backend
	if backend == "" {
		backend = "pty"
	}
	agentID := entry.AgentID
	if agentID == "" {
		agentID = "default"
	}
	adapter := entry.Adapter
	if adapter == "" {
		adapter = "generic"
	}

	switch entry.Kind {
	case audit.KindSessionStarted:
		r.sessionsActive[backend]++
		key := fmt.Sprintf("adapter=%s,agent_id=%s,backend=%s", adapter, agentID, backend)
		r.sessionsTotal[key]++

	case audit.KindSessionFinished, audit.KindSupervisionFinished:
		if r.sessionsActive[backend] > 0 {
			r.sessionsActive[backend]--
		}

	case audit.KindEventDetected:
		sensitiveStr := "false"
		if entry.Sensitive {
			sensitiveStr = "true"
		}
		riskStr := string(entry.Risk)
		if riskStr == "" {
			riskStr = "unknown"
		}
		typeStr := string(entry.EventType)
		if typeStr == "" {
			typeStr = "confirmation"
		}
		key := fmt.Sprintf("adapter=%s,agent_id=%s,event_type=%s,risk=%s,sensitive=%s",
			adapter, agentID, typeStr, riskStr, sensitiveStr)
		r.eventsDetectedTotal[key]++

		eventKey := makeEventKey(entry.RunID, entry.SessionID, entry.EventID)
		r.pendingEvents[eventKey] = entry.Timestamp

	case audit.KindEventWithdrawn:
		key := fmt.Sprintf("adapter=%s,agent_id=%s", adapter, agentID)
		r.eventsWithdrawnTotal[key]++

		eventKey := makeEventKey(entry.RunID, entry.SessionID, entry.EventID)
		delete(r.pendingEvents, eventKey)

	case audit.KindDecision, audit.KindPolicyEvaluated:
		decisionStr := string(entry.Decision)
		if decisionStr == "" {
			decisionStr = "unknown"
		}
		byStr := string(entry.DecisionBy)
		if byStr == "" {
			byStr = "unknown"
		}
		outcomeStr := string(entry.Outcome)
		if outcomeStr == "" {
			outcomeStr = "unknown"
		}
		ruleStr := entry.Rule
		if ruleStr == "" {
			ruleStr = "default"
		}

		key := fmt.Sprintf("adapter=%s,agent_id=%s,decision=%s,decision_by=%s,outcome=%s,rule=%s",
			adapter, agentID, decisionStr, byStr, outcomeStr, ruleStr)
		r.decisionsTotal[key]++

		// Track decision latency duration if this event was previously detected
		eventKey := makeEventKey(entry.RunID, entry.SessionID, entry.EventID)
		if detectedAt, found := r.pendingEvents[eventKey]; found {
			duration := entry.Timestamp.Sub(detectedAt).Seconds()
			if duration < 0 {
				duration = 0
			}
			r.recordDurationLocked(agentID, adapter, byStr, duration)
			delete(r.pendingEvents, eventKey)
		}

		// Check guardrail violation
		if strings.HasSuffix(entry.Reason, "_blocked") || entry.Outcome == audit.OutcomeFailed {
			gKey := fmt.Sprintf("reason=%s,rule=%s", entry.Reason, ruleStr)
			r.guardrailViolations[gKey]++
		}

	case audit.KindOperatorInput:
		r.operatorInputsTotal[agentID]++
	}
}

func (r *Registry) recordDurationLocked(agentID, adapter, decisionBy string, seconds float64) {
	key := fmt.Sprintf("adapter=%s,agent_id=%s,decision_by=%s", adapter, agentID, decisionBy)
	hist, exists := r.decisionDurations[key]
	if !exists {
		hist = &histogramSeries{
			labels: map[string]string{
				"adapter":     adapter,
				"agent_id":    agentID,
				"decision_by": decisionBy,
			},
			buckets: append([]float64(nil), DefaultHistogramBuckets...),
			counts:  make([]uint64, len(DefaultHistogramBuckets)),
		}
		r.decisionDurations[key] = hist
	}

	hist.count++
	hist.sum += seconds
	for i, bucket := range hist.buckets {
		if seconds <= bucket {
			hist.counts[i]++
		}
	}
}

// Snapshot returns an immutable point-in-time copy of all metrics in the registry.
func (r *Registry) Snapshot() Snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()

	snap := Snapshot{
		Timestamp:            time.Now().UTC(),
		EventsPending:        int64(len(r.pendingEvents)),
		SessionsActive:       mapToSamples(r.sessionsActive, "backend"),
		SessionsTotal:        parsedMapToSamples(r.sessionsTotal),
		EventsDetectedTotal:  parsedMapToSamples(r.eventsDetectedTotal),
		EventsWithdrawnTotal: parsedMapToSamples(r.eventsWithdrawnTotal),
		DecisionsTotal:       parsedMapToSamples(r.decisionsTotal),
		OperatorInputsTotal:  mapToSamples(r.operatorInputsTotal, "agent_id"),
		GuardrailsViolations: parsedMapToSamples(r.guardrailViolations),
	}

	snap.DecisionDurations = make([]HistogramSample, 0, len(r.decisionDurations))
	for _, hist := range r.decisionDurations {
		bucketMap := make(map[float64]uint64, len(hist.buckets))
		for i, b := range hist.buckets {
			bucketMap[b] = hist.counts[i]
		}
		snap.DecisionDurations = append(snap.DecisionDurations, HistogramSample{
			Labels:  cloneMap(hist.labels),
			Count:   hist.count,
			Sum:     hist.sum,
			Buckets: bucketMap,
		})
	}

	return snap
}

func makeEventKey(runID, sessionID, eventID string) string {
	if eventID == "" {
		return sessionID
	}
	return runID + ":" + sessionID + ":" + eventID
}

func mapToSamples(m map[string]int64, labelName string) []MetricSample {
	samples := make([]MetricSample, 0, len(m))
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		samples = append(samples, MetricSample{
			Labels: map[string]string{labelName: k},
			Value:  float64(m[k]),
		})
	}
	return samples
}

func parsedMapToSamples(m map[string]int64) []MetricSample {
	samples := make([]MetricSample, 0, len(m))
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		labels := parseLabelKey(k)
		samples = append(samples, MetricSample{
			Labels: labels,
			Value:  float64(m[k]),
		})
	}
	return samples
}

func parseLabelKey(key string) map[string]string {
	labels := make(map[string]string)
	pairs := strings.Split(key, ",")
	for _, p := range pairs {
		parts := strings.SplitN(p, "=", 2)
		if len(parts) == 2 {
			labels[parts[0]] = parts[1]
		}
	}
	return labels
}

func cloneMap(m map[string]string) map[string]string {
	c := make(map[string]string, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}
