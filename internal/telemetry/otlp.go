package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	buildversion "github.com/Hocsman/Relayer/internal/version"
)

// BuildOTLPPayload constructs an OpenTelemetry Protocol (OTLP/HTTP JSON v1) metric payload.
func BuildOTLPPayload(snap Snapshot, serviceName, environment string) ([]byte, error) {
	if serviceName == "" {
		serviceName = DefaultServiceName
	}
	if environment == "" {
		environment = DefaultEnvironment
	}
	timeUnixNano := strconv.FormatInt(snap.Timestamp.UnixNano(), 10)

	resourceAttrs := []map[string]any{
		{"key": "service.name", "value": map[string]any{"stringValue": serviceName}},
		{"key": "deployment.environment", "value": map[string]any{"stringValue": environment}},
		{"key": "service.version", "value": map[string]any{"stringValue": buildversion.Version}},
	}

	metrics := make([]map[string]any, 0, 10)

	// 1. Sessions Active (Gauge)
	if len(snap.SessionsActive) > 0 {
		dataPoints := make([]map[string]any, 0, len(snap.SessionsActive))
		for _, s := range snap.SessionsActive {
			dataPoints = append(dataPoints, map[string]any{
				"timeUnixNano": timeUnixNano,
				"asInt":        strconv.FormatInt(int64(s.Value), 10),
				"attributes":   otlpAttributes(s.Labels),
			})
		}
		metrics = append(metrics, map[string]any{
			"name":        "relayer.sessions.active",
			"description": "Number of active agent sessions currently supervised",
			"gauge": map[string]any{
				"dataPoints": dataPoints,
			},
		})
	}

	// 2. Events Pending (Gauge)
	metrics = append(metrics, map[string]any{
		"name":        "relayer.events.pending",
		"description": "Number of interception events currently waiting for a decision",
		"gauge": map[string]any{
			"dataPoints": []map[string]any{
				{
					"timeUnixNano": timeUnixNano,
					"asInt":        strconv.FormatInt(snap.EventsPending, 10),
				},
			},
		},
	})

	// 3. Sessions Total (Sum)
	if len(snap.SessionsTotal) > 0 {
		metrics = append(metrics, buildSumMetric("relayer.sessions.total", "Total number of agent sessions started", snap.SessionsTotal, timeUnixNano))
	}

	// 4. Events Detected Total (Sum)
	if len(snap.EventsDetectedTotal) > 0 {
		metrics = append(metrics, buildSumMetric("relayer.events.detected.total", "Total number of interception prompts detected", snap.EventsDetectedTotal, timeUnixNano))
	}

	// 5. Events Withdrawn Total (Sum)
	if len(snap.EventsWithdrawnTotal) > 0 {
		metrics = append(metrics, buildSumMetric("relayer.events.withdrawn.total", "Total number of prompts withdrawn by the agent", snap.EventsWithdrawnTotal, timeUnixNano))
	}

	// 6. Decisions Total (Sum)
	if len(snap.DecisionsTotal) > 0 {
		metrics = append(metrics, buildSumMetric("relayer.decisions.total", "Total number of policy or human decisions recorded", snap.DecisionsTotal, timeUnixNano))
	}

	// 7. Guardrail Violations Total (Sum)
	if len(snap.GuardrailsViolations) > 0 {
		metrics = append(metrics, buildSumMetric("relayer.guardrails.violations.total", "Total number of guardrail policy violations blocked", snap.GuardrailsViolations, timeUnixNano))
	}

	// 8. Operator Inputs Total (Sum)
	if len(snap.OperatorInputsTotal) > 0 {
		metrics = append(metrics, buildSumMetric("relayer.operator.inputs.total", "Total number of manual line inputs submitted", snap.OperatorInputsTotal, timeUnixNano))
	}

	// 9. Decision Duration (Histogram)
	if len(snap.DecisionDurations) > 0 {
		dataPoints := make([]map[string]any, 0, len(snap.DecisionDurations))
		for _, h := range snap.DecisionDurations {
			buckets := make([]float64, 0, len(h.Buckets))
			for b := range h.Buckets {
				buckets = append(buckets, b)
			}
			sort.Float64s(buckets)

			bucketCounts := make([]string, 0, len(buckets))
			explicitBounds := make([]float64, 0, len(buckets))
			for _, b := range buckets {
				explicitBounds = append(explicitBounds, b)
				bucketCounts = append(bucketCounts, strconv.FormatUint(h.Buckets[b], 10))
			}

			dataPoints = append(dataPoints, map[string]any{
				"timeUnixNano":   timeUnixNano,
				"count":          strconv.FormatUint(h.Count, 10),
				"sum":            h.Sum,
				"bucketCounts":   bucketCounts,
				"explicitBounds": explicitBounds,
				"attributes":     otlpAttributes(h.Labels),
			})
		}

		metrics = append(metrics, map[string]any{
			"name":        "relayer.decision.duration.seconds",
			"description": "Latency between prompt detection and decision in seconds",
			"histogram": map[string]any{
				"aggregationTemporality": 2, // CUMULATIVE
				"dataPoints":             dataPoints,
			},
		})
	}

	payload := map[string]any{
		"resourceMetrics": []map[string]any{
			{
				"resource": map[string]any{
					"attributes": resourceAttrs,
				},
				"scopeMetrics": []map[string]any{
					{
						"scope": map[string]any{
							"name":    "github.com/Hocsman/Relayer/internal/telemetry",
							"version": buildversion.Version,
						},
						"metrics": metrics,
					},
				},
			},
		},
	}

	return json.Marshal(payload)
}

func buildSumMetric(name, desc string, samples []MetricSample, timeUnixNano string) map[string]any {
	dataPoints := make([]map[string]any, 0, len(samples))
	for _, s := range samples {
		dataPoints = append(dataPoints, map[string]any{
			"timeUnixNano": timeUnixNano,
			"asInt":        strconv.FormatInt(int64(s.Value), 10),
			"attributes":   otlpAttributes(s.Labels),
		})
	}
	return map[string]any{
		"name":        name,
		"description": desc,
		"sum": map[string]any{
			"aggregationTemporality": 2, // CUMULATIVE
			"isMonotonic":            true,
			"dataPoints":             dataPoints,
		},
	}
}

func otlpAttributes(labels map[string]string) []map[string]any {
	if len(labels) == 0 {
		return nil
	}
	attrs := make([]map[string]any, 0, len(labels))
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		attrs = append(attrs, map[string]any{
			"key":   k,
			"value": map[string]any{"stringValue": labels[k]},
		})
	}
	return attrs
}

// OTLPExporter periodically exports metrics to an OTLP/HTTP collector.
type OTLPExporter struct {
	config      OTLPConfig
	registry    *Registry
	serviceName string
	environment string
	client      *http.Client
	stopCh      chan struct{}
	wg          sync.WaitGroup
	mu          sync.Mutex
	started     bool
}

// NewOTLPExporter builds an exporter for the configured endpoint.
func NewOTLPExporter(cfg OTLPConfig, reg *Registry, serviceName, environment string) *OTLPExporter {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultExportTimeout
	}
	return &OTLPExporter{
		config:      cfg,
		registry:    reg,
		serviceName: serviceName,
		environment: environment,
		client:      &http.Client{Timeout: timeout},
		stopCh:      make(chan struct{}),
	}
}

// Start launches the background periodic exporter worker.
func (e *OTLPExporter) Start() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started {
		return
	}
	e.started = true

	interval := e.config.ExportInterval
	if interval <= 0 {
		interval = DefaultExportInterval
	}

	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-e.stopCh:
				// Push final snapshot before terminating
				_ = e.ExportOnce(context.Background())
				return
			case <-ticker.C:
				_ = e.ExportOnce(context.Background())
			}
		}
	}()
}

// ExportOnce sends an immediate snapshot of metrics to the OTLP endpoint.
func (e *OTLPExporter) ExportOnce(ctx context.Context) error {
	snap := e.registry.Snapshot()
	body, err := BuildOTLPPayload(snap, e.serviceName, e.environment)
	if err != nil {
		return fmt.Errorf("serialize otlp payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.config.Endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create otlp request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	for k, v := range e.config.Headers {
		req.Header.Set(k, v)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("send otlp metrics: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("otlp collector returned non-2xx status: %d", resp.StatusCode)
	}

	return nil
}

// Close terminates the background exporter and awaits completion.
func (e *OTLPExporter) Close() error {
	e.mu.Lock()
	if !e.started {
		e.mu.Unlock()
		return nil
	}
	e.mu.Unlock()

	close(e.stopCh)
	e.wg.Wait()
	return nil
}
