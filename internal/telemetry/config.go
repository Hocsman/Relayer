// Package telemetry provides Prometheus and OpenTelemetry (OTel) metrics export
// for Relayer's supervised agent sessions, policy decisions, and guardrails.
package telemetry

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

const (
	DefaultServiceName    = "relayer"
	DefaultEnvironment    = "production"
	DefaultPrometheusAddr = ":9090"
	DefaultPrometheusPath = "/metrics"
	DefaultExportInterval = 15 * time.Second
	DefaultExportTimeout  = 5 * time.Second
)

// Config controls Prometheus metrics serving and OTLP telemetry export.
type Config struct {
	Enabled     bool             `json:"enabled" yaml:"enabled"`
	ServiceName string           `json:"service_name" yaml:"service_name"`
	Environment string           `json:"environment" yaml:"environment"`
	Prometheus  PrometheusConfig `json:"prometheus" yaml:"prometheus"`
	OTLP        OTLPConfig       `json:"otlp" yaml:"otlp"`
}

// PrometheusConfig controls the local HTTP scraper endpoint.
type PrometheusConfig struct {
	Enabled bool   `json:"enabled" yaml:"enabled"`
	Address string `json:"address" yaml:"address"`
	Path    string `json:"path" yaml:"path"`
}

// OTLPConfig controls periodic pushing to an OpenTelemetry collector.
type OTLPConfig struct {
	Enabled        bool              `json:"enabled" yaml:"enabled"`
	Endpoint       string            `json:"endpoint" yaml:"endpoint"`
	Headers        map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
	ExportInterval time.Duration     `json:"export_interval" yaml:"export_interval"`
	Timeout        time.Duration     `json:"timeout" yaml:"timeout"`
}

// DefaultConfig returns safe, production-grade telemetry settings (disabled by default).
func DefaultConfig() Config {
	return Config{
		Enabled:     false,
		ServiceName: DefaultServiceName,
		Environment: DefaultEnvironment,
		Prometheus: PrometheusConfig{
			Enabled: true,
			Address: DefaultPrometheusAddr,
			Path:    DefaultPrometheusPath,
		},
		OTLP: OTLPConfig{
			Enabled:        false,
			Endpoint:       "http://localhost:4318/v1/metrics",
			ExportInterval: DefaultExportInterval,
			Timeout:        DefaultExportTimeout,
		},
	}
}

// Validate checks telemetry configuration for well-formed addresses, URLs, and timing.
func Validate(cfg Config) error {
	if !cfg.Enabled {
		return nil
	}

	if strings.TrimSpace(cfg.ServiceName) == "" {
		return errors.New("telemetry service_name cannot be empty")
	}

	if cfg.Prometheus.Enabled {
		addr := cfg.Prometheus.Address
		if strings.TrimSpace(addr) == "" {
			return errors.New("prometheus address cannot be empty when prometheus is enabled")
		}
		// Validate host:port format
		if _, _, err := net.SplitHostPort(addr); err != nil {
			// Allow bare port like ":9090"
			if !strings.HasPrefix(addr, ":") {
				return fmt.Errorf("invalid prometheus address %q: %w", addr, err)
			}
		}
		if !strings.HasPrefix(cfg.Prometheus.Path, "/") {
			return fmt.Errorf("prometheus path %q must start with '/'", cfg.Prometheus.Path)
		}
	}

	if cfg.OTLP.Enabled {
		ep := strings.TrimSpace(cfg.OTLP.Endpoint)
		if ep == "" {
			return errors.New("otlp endpoint cannot be empty when otlp is enabled")
		}
		parsed, err := url.Parse(ep)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return fmt.Errorf("invalid otlp endpoint %q: must be a valid http or https URL", ep)
		}
		if cfg.OTLP.ExportInterval <= 0 {
			return errors.New("otlp export_interval must be positive")
		}
		if cfg.OTLP.Timeout <= 0 {
			return errors.New("otlp timeout must be positive")
		}
	}

	return nil
}
