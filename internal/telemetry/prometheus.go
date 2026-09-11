package telemetry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// RenderPrometheus formats a Registry Snapshot into standard Prometheus text format (v0.0.4).
func RenderPrometheus(snap Snapshot) []byte {
	var buf bytes.Buffer

	// 1. Sessions active (gauge)
	buf.WriteString("# HELP relayer_sessions_active Number of active agent sessions currently supervised.\n")
	buf.WriteString("# TYPE relayer_sessions_active gauge\n")
	if len(snap.SessionsActive) == 0 {
		buf.WriteString("relayer_sessions_active 0\n")
	} else {
		for _, s := range snap.SessionsActive {
			writeMetricLine(&buf, "relayer_sessions_active", s.Labels, s.Value)
		}
	}
	buf.WriteByte('\n')

	// 2. Events pending (gauge)
	buf.WriteString("# HELP relayer_events_pending Number of interception events currently waiting for a decision.\n")
	buf.WriteString("# TYPE relayer_events_pending gauge\n")
	writeMetricLine(&buf, "relayer_events_pending", nil, float64(snap.EventsPending))
	buf.WriteByte('\n')

	// 3. Sessions total (counter)
	buf.WriteString("# HELP relayer_sessions_total Total number of supervised agent sessions started.\n")
	buf.WriteString("# TYPE relayer_sessions_total counter\n")
	for _, s := range snap.SessionsTotal {
		writeMetricLine(&buf, "relayer_sessions_total", s.Labels, s.Value)
	}
	buf.WriteByte('\n')

	// 4. Events detected total (counter)
	buf.WriteString("# HELP relayer_events_detected_total Total number of interception prompts detected.\n")
	buf.WriteString("# TYPE relayer_events_detected_total counter\n")
	for _, s := range snap.EventsDetectedTotal {
		writeMetricLine(&buf, "relayer_events_detected_total", s.Labels, s.Value)
	}
	buf.WriteByte('\n')

	// 5. Events withdrawn total (counter)
	buf.WriteString("# HELP relayer_events_withdrawn_total Total number of prompts withdrawn by the agent.\n")
	buf.WriteString("# TYPE relayer_events_withdrawn_total counter\n")
	for _, s := range snap.EventsWithdrawnTotal {
		writeMetricLine(&buf, "relayer_events_withdrawn_total", s.Labels, s.Value)
	}
	buf.WriteByte('\n')

	// 6. Decisions total (counter)
	buf.WriteString("# HELP relayer_decisions_total Total number of policy or human decisions recorded.\n")
	buf.WriteString("# TYPE relayer_decisions_total counter\n")
	for _, s := range snap.DecisionsTotal {
		writeMetricLine(&buf, "relayer_decisions_total", s.Labels, s.Value)
	}
	buf.WriteByte('\n')

	// 7. Operator inputs total (counter)
	buf.WriteString("# HELP relayer_operator_inputs_total Total number of manual line inputs submitted.\n")
	buf.WriteString("# TYPE relayer_operator_inputs_total counter\n")
	for _, s := range snap.OperatorInputsTotal {
		writeMetricLine(&buf, "relayer_operator_inputs_total", s.Labels, s.Value)
	}
	buf.WriteByte('\n')

	// 8. Guardrail violations total (counter)
	buf.WriteString("# HELP relayer_guardrail_violations_total Total number of guardrail policy violations blocked.\n")
	buf.WriteString("# TYPE relayer_guardrail_violations_total counter\n")
	for _, s := range snap.GuardrailsViolations {
		writeMetricLine(&buf, "relayer_guardrail_violations_total", s.Labels, s.Value)
	}
	buf.WriteByte('\n')

	// 9. Decision duration seconds (histogram)
	if len(snap.DecisionDurations) > 0 {
		buf.WriteString("# HELP relayer_decision_duration_seconds Latency between prompt detection and decision in seconds.\n")
		buf.WriteString("# TYPE relayer_decision_duration_seconds histogram\n")
		for _, h := range snap.DecisionDurations {
			// Sort buckets ascending
			buckets := make([]float64, 0, len(h.Buckets))
			for b := range h.Buckets {
				buckets = append(buckets, b)
			}
			sort.Float64s(buckets)

			for _, b := range buckets {
				bucketLabels := cloneMap(h.Labels)
				bucketLabels["le"] = formatFloat(b)
				writeMetricLine(&buf, "relayer_decision_duration_seconds_bucket", bucketLabels, float64(h.Buckets[b]))
			}
			infLabels := cloneMap(h.Labels)
			infLabels["le"] = "+Inf"
			writeMetricLine(&buf, "relayer_decision_duration_seconds_bucket", infLabels, float64(h.Count))
			writeMetricLine(&buf, "relayer_decision_duration_seconds_sum", h.Labels, h.Sum)
			writeMetricLine(&buf, "relayer_decision_duration_seconds_count", h.Labels, float64(h.Count))
		}
	}

	return buf.Bytes()
}

func writeMetricLine(buf *bytes.Buffer, name string, labels map[string]string, val float64) {
	buf.WriteString(name)
	if len(labels) > 0 {
		buf.WriteByte('{')
		keys := make([]string, 0, len(labels))
		for k := range labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			buf.WriteString(k)
			buf.WriteString("=\"")
			buf.WriteString(escapeLabelValue(labels[k]))
			buf.WriteByte('"')
		}
		buf.WriteByte('}')
	}
	buf.WriteByte(' ')
	buf.WriteString(formatFloat(val))
	buf.WriteByte('\n')
}

func escapeLabelValue(val string) string {
	val = strings.ReplaceAll(val, `\`, `\\`)
	val = strings.ReplaceAll(val, `"`, `\"`)
	val = strings.ReplaceAll(val, "\n", `\n`)
	return val
}

func formatFloat(val float64) string {
	if val == float64(int64(val)) {
		return fmt.Sprintf("%d", int64(val))
	}
	return fmt.Sprintf("%.4f", val)
}

// PrometheusServer serves Prometheus metrics via HTTP.
type PrometheusServer struct {
	config   PrometheusConfig
	registry *Registry
	server   *http.Server
	listener net.Listener
	mu       sync.Mutex
	started  bool
	closed   bool
}

// NewPrometheusServer creates a server configured to scrape metrics from the registry.
func NewPrometheusServer(cfg PrometheusConfig, reg *Registry) *PrometheusServer {
	return &PrometheusServer{
		config:   cfg,
		registry: reg,
	}
}

// Start opens the TCP listener and begins serving HTTP requests.
func (s *PrometheusServer) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return errors.New("prometheus server already started")
	}

	mux := http.NewServeMux()
	path := s.config.Path
	if path == "" {
		path = DefaultPrometheusPath
	}
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		snap := s.registry.Snapshot()
		body := RenderPrometheus(snap)
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})

	addr := s.config.Address
	if addr == "" {
		addr = DefaultPrometheusAddr
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on prometheus address %q: %w", addr, err)
	}

	s.listener = ln
	s.server = &http.Server{
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}
	s.started = true

	go func() {
		_ = s.server.Serve(ln)
	}()

	return nil
}

// Close gracefully stops the Prometheus HTTP server.
func (s *PrometheusServer) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || s.closed {
		return nil
	}
	s.closed = true

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	return s.server.Shutdown(ctx)
}

// Address returns the effective network address of the listener (e.g. for testing with port :0).
func (s *PrometheusServer) Address() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener != nil {
		return s.listener.Addr().String()
	}
	return s.config.Address
}
