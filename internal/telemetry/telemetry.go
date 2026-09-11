package telemetry

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Engine is the central telemetry coordinator managing the metric registry,
// Prometheus HTTP server and the OTLP exporter worker.
type Engine struct {
	config     Config
	registry   *Registry
	promServer *PrometheusServer
	otlpExp    *OTLPExporter
	mu         sync.Mutex
	started    bool
	closed     bool
}

// NewEngine initializes a telemetry engine from the given configuration.
func NewEngine(cfg Config) (*Engine, error) {
	if err := Validate(cfg); err != nil {
		return nil, fmt.Errorf("invalid telemetry configuration: %w", err)
	}

	reg := NewRegistry()
	var promServer *PrometheusServer
	if cfg.Enabled && cfg.Prometheus.Enabled {
		promServer = NewPrometheusServer(cfg.Prometheus, reg)
	}

	var otlpExp *OTLPExporter
	if cfg.Enabled && cfg.OTLP.Enabled {
		otlpExp = NewOTLPExporter(cfg.OTLP, reg, cfg.ServiceName, cfg.Environment)
	}

	return &Engine{
		config:     cfg,
		registry:   reg,
		promServer: promServer,
		otlpExp:    otlpExp,
	}, nil
}

// Registry returns the underlying metric registry that implements audit.EntryObserver.
func (e *Engine) Registry() *Registry {
	if e == nil {
		return nil
	}
	return e.registry
}

// Config returns the configuration of this telemetry engine.
func (e *Engine) Config() Config {
	if e == nil {
		return DefaultConfig()
	}
	return e.config
}

// Enabled reports whether telemetry collection or export is active.
func (e *Engine) Enabled() bool {
	return e != nil && e.config.Enabled
}

// Start launches the active servers and background export workers.
func (e *Engine) Start(ctx context.Context) error {
	if e == nil || !e.config.Enabled {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started {
		return errors.New("telemetry engine already started")
	}
	e.started = true

	if e.promServer != nil {
		if err := e.promServer.Start(); err != nil {
			return fmt.Errorf("start prometheus metrics server: %w", err)
		}
	}

	if e.otlpExp != nil {
		e.otlpExp.Start()
	}

	return nil
}

// Close gracefully stops the Prometheus HTTP server and flushes OTLP metrics.
func (e *Engine) Close() error {
	if e == nil || !e.config.Enabled {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.started || e.closed {
		return nil
	}
	e.closed = true

	var errs []error
	if e.promServer != nil {
		if err := e.promServer.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close prometheus server: %w", err))
		}
	}
	if e.otlpExp != nil {
		if err := e.otlpExp.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close otlp exporter: %w", err))
		}
	}

	return errors.Join(errs...)
}

// PrometheusAddress returns the listening address if Prometheus is enabled and started.
func (e *Engine) PrometheusAddress() string {
	if e == nil || e.promServer == nil {
		return ""
	}
	return e.promServer.Address()
}
