package telemetry

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
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
	Labels  map[string]string  `json:"labels"`
	Count   uint64             `json:"count"`
	Sum     float64            `json:"sum"`
	Buckets map[float64]uint64 `json:"buckets"`
}

// Snapshot contains an immutable point-in-time capture of all registry metrics.
type Snapshot struct {
	Timestamp            time.Time         `json:"timestamp"`
	SessionsActive       []MetricSample    `json:"sessions_active"`
	EventsPending        int64             `json:"events_pending"`
	SessionsTotal        []MetricSample    `json:"sessions_total"`
	EventsDetectedTotal  []MetricSample    `json:"events_detected_total"`
	EventsWithdrawnTotal []MetricSample    `json:"events_withdrawn_total"`
	DecisionsTotal       []MetricSample    `json:"decisions_total"`
	OperatorInputsTotal  []MetricSample    `json:"operator_inputs_total"`
	ControlEventsTotal   []MetricSample    `json:"control_events_total"`
	RecordingEventsTotal []MetricSample    `json:"recording_events_total"`
	GuardrailsViolations []MetricSample    `json:"guardrails_violations"`
	DecisionDurations    []HistogramSample `json:"decision_durations"`
}

// Registry aggregates and tracks metrics from audit events in a thread-safe manner.
type Registry struct {
	mu             sync.RWMutex
	sessionsActive map[string]int64
	// activeSessions is, per run and session, the backend under which a
	// session is counted in sessionsActive. A session is counted once from
	// its start to its first end, however many entries say it started or
	// ended: an operator Stop journals the session's end, and the exit of the
	// process it stopped is journaled as an end too; both decremented the
	// gauge, which then read one short while other agents ran.
	activeSessions       map[runSession]string
	sessionsTotal        map[string]int64
	eventsDetectedTotal  map[string]int64
	eventsWithdrawnTotal map[string]int64
	decisionsTotal       map[string]int64
	operatorInputsTotal  map[string]int64
	controlEventsTotal   map[string]int64
	recordingEventsTotal map[string]int64
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
		activeSessions:       make(map[runSession]string),
		sessionsTotal:        make(map[string]int64),
		eventsDetectedTotal:  make(map[string]int64),
		eventsWithdrawnTotal: make(map[string]int64),
		decisionsTotal:       make(map[string]int64),
		operatorInputsTotal:  make(map[string]int64),
		controlEventsTotal:   make(map[string]int64),
		recordingEventsTotal: make(map[string]int64),
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
		r.dropSessionPending(entry.RunID, entry.SessionID)
		r.countSessionActive(entry.RunID, entry.SessionID, backend)
		key := fmt.Sprintf("adapter=%s,agent_id=%s,backend=%s", adapter, agentID, backend)
		r.sessionsTotal[key]++

	case audit.KindSessionFinished, audit.KindSupervisionFinished:
		if entry.Kind == audit.KindSessionFinished && entry.Reason == audit.ReasonProcessExitStale {
			// The exit of a process a replacement had already superseded:
			// the session that is active, and the prompts that are pending,
			// are the replacement's. Its own end was counted when it was
			// stopped for the replacement.
			return
		}
		r.dropSessionPending(entry.RunID, entry.SessionID)
		r.uncountSessionActive(entry.RunID, entry.SessionID)

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

		// A process exit is journaled as a detected event, but nothing ever
		// decides it. Counting it as pending leaked one entry per exit: with
		// each process's exit now carrying its own ID, the gauge grew without
		// bound as agents were restarted.
		if entry.EventType != adapters.EventProcessExit {
			eventKey := makeEventKey(entry.RunID, entry.SessionID, entry.EventID)
			r.pendingEvents[eventKey] = entry.Timestamp
		}

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

		// Only the decision entry is a decision. Every prompt is journaled
		// policy_evaluated first, and its decision, by the policy or a human,
		// is journaled once more as a decision: counting both counted each
		// decision twice, and a prompt still waiting on a human as decided.
		if entry.Kind == audit.KindDecision {
			key := fmt.Sprintf("adapter=%s,agent_id=%s,decision=%s,decision_by=%s,outcome=%s,rule=%s",
				adapter, agentID, decisionStr, byStr, outcomeStr, ruleStr)
			r.decisionsTotal[key]++
		}

		// A prompt stays pending until it is decided. The policy's evaluation
		// is not a decision: it follows every detection within microseconds,
		// and closing the prompt there kept events_pending at zero and made
		// the decision duration measure the policy engine, not the operator.
		// The decision that follows an automatic evaluation is journaled as
		// one, by policy, and closes the prompt like a human's.
		eventKey := makeEventKey(entry.RunID, entry.SessionID, entry.EventID)
		if detectedAt, found := r.pendingEvents[eventKey]; found && entry.Kind == audit.KindDecision {
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

	case audit.KindControlRequested, audit.KindControlGranted, audit.KindControlDeclined,
		audit.KindControlReleased, audit.KindControlForced,
		audit.KindAttachStarted, audit.KindAttachFinished:
		key := fmt.Sprintf("action=%s,agent_id=%s,outcome=%s",
			controlAction(entry.Kind), agentID, outcomeLabel(entry.Outcome))
		r.controlEventsTotal[key]++

	case audit.KindRecordingStarted, audit.KindRecordingFinished,
		audit.KindRecordingExported, audit.KindRecordingDeleted:
		key := fmt.Sprintf("action=%s,agent_id=%s,outcome=%s",
			strings.TrimPrefix(string(entry.Kind), "recording_"), agentID, outcomeLabel(entry.Outcome))
		r.recordingEventsTotal[key]++
	}
}

// controlAction names a hand-over for the action label. The two attach kinds
// move the same terminal as the control kinds, through the attach verb, so
// they share one metric rather than splitting a question across two.
func controlAction(kind audit.Kind) string {
	switch kind {
	case audit.KindAttachStarted:
		return "attached"
	case audit.KindAttachFinished:
		return "detached"
	}
	return strings.TrimPrefix(string(kind), "control_")
}

func outcomeLabel(outcome audit.Outcome) string {
	if outcome == "" {
		return "unknown"
	}
	return string(outcome)
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
		ControlEventsTotal:   parsedMapToSamples(r.controlEventsTotal),
		RecordingEventsTotal: parsedMapToSamples(r.recordingEventsTotal),
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

// runSession names one session of one run.
type runSession struct {
	runID, sessionID string
}

// countSessionActive counts a session that started as active under backend,
// once: a session already counted, whose end was not journaled before it
// started again, is counted under its new backend alone. Its caller holds the
// registry's lock.
func (r *Registry) countSessionActive(runID, sessionID, backend string) {
	key := runSession{runID: runID, sessionID: sessionID}
	if counted, found := r.activeSessions[key]; found && r.sessionsActive[counted] > 0 {
		r.sessionsActive[counted]--
	}
	r.activeSessions[key] = backend
	r.sessionsActive[backend]++
}

// uncountSessionActive stops counting a session that ended, if it was counted:
// its first end ends it, and those journaled after it end nothing. Its caller
// holds the registry's lock.
func (r *Registry) uncountSessionActive(runID, sessionID string) {
	key := runSession{runID: runID, sessionID: sessionID}
	counted, found := r.activeSessions[key]
	if !found {
		return
	}
	delete(r.activeSessions, key)
	if r.sessionsActive[counted] > 0 {
		r.sessionsActive[counted]--
	}
}

// dropSessionPending forgets the prompts of a session whose process ended or
// was replaced: they can no longer be decided, and each one stayed in
// events_pending for good. Its caller holds the registry's lock.
func (r *Registry) dropSessionPending(runID, sessionID string) {
	if sessionID == "" {
		return
	}
	// A sensitive prompt's ID is withheld from the journal, so it is keyed
	// by its session alone.
	delete(r.pendingEvents, makeEventKey(runID, sessionID, ""))
	prefix := runID + ":" + sessionID + ":"
	for key := range r.pendingEvents {
		if strings.HasPrefix(key, prefix) {
			delete(r.pendingEvents, key)
		}
	}
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
