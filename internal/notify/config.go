package notify

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// WebhookFormat defines the target service serialization schema.
type WebhookFormat string

const (
	FormatGeneric WebhookFormat = "generic"
	FormatSlack   WebhookFormat = "slack"
	FormatDiscord WebhookFormat = "discord"
)

// WebhookConfig specifies an HTTP webhook destination for notifications.
type WebhookConfig struct {
	Name        string            `yaml:"name,omitempty"`
	URL         string            `yaml:"url"`
	Format      string            `yaml:"format"`       // "generic", "slack", "discord" (default: "generic")
	MinSeverity string            `yaml:"min_severity"` // "info", "warning", "critical" (default: "warning")
	Timeout     string            `yaml:"timeout"`      // e.g. "5s" (default: "5s")
	Headers     map[string]string `yaml:"headers,omitempty"`
}

// Config controls operator alert and notification behavior.
type Config struct {
	// Enabled is the master switch for notifications and alerts.
	Enabled bool `yaml:"enabled"`
	// Bell enables emitting the ASCII BEL character (\a) to the terminal.
	Bell bool `yaml:"bell"`
	// Desktop enables native operating-system desktop notifications.
	Desktop bool `yaml:"desktop"`
	// MinSeverity specifies the minimum severity required to trigger alerts ("info", "warning", "critical").
	MinSeverity string `yaml:"min_severity,omitempty"`
	// Webhooks defines remote webhook endpoints.
	Webhooks []WebhookConfig `yaml:"webhooks,omitempty"`
}

// DefaultConfig returns the default notification settings (local bell & desktop enabled, no webhooks).
func DefaultConfig() Config {
	return Config{
		Enabled:     true,
		Bell:        true,
		Desktop:     true,
		MinSeverity: SeverityInfo,
		Webhooks:    nil,
	}
}

// DisabledConfig returns settings with all notifications disabled.
func DisabledConfig() Config {
	return Config{
		Enabled:     false,
		Bell:        false,
		Desktop:     false,
		MinSeverity: SeverityInfo,
		Webhooks:    nil,
	}
}

// Validate checks whether the notification configuration is valid.
func Validate(cfg Config) error {
	if !cfg.Enabled {
		return nil
	}
	if cfg.MinSeverity != "" && !isValidSeverity(cfg.MinSeverity) {
		return fmt.Errorf("invalid notification min_severity %q, must be info, warning, or critical", cfg.MinSeverity)
	}
	for i, w := range cfg.Webhooks {
		rawURL := strings.TrimSpace(w.URL)
		if rawURL == "" {
			return fmt.Errorf("webhook[%d] missing required url", i)
		}
		parsed, err := url.Parse(rawURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("webhook[%d] has invalid url %q (must be http or https)", i, rawURL)
		}
		format := strings.ToLower(strings.TrimSpace(w.Format))
		if format != "" && format != string(FormatGeneric) && format != string(FormatSlack) && format != string(FormatDiscord) {
			return fmt.Errorf("webhook[%d] has unsupported format %q (must be generic, slack, or discord)", i, w.Format)
		}
		if w.MinSeverity != "" && !isValidSeverity(w.MinSeverity) {
			return fmt.Errorf("webhook[%d] has invalid min_severity %q (must be info, warning, or critical)", i, w.MinSeverity)
		}
		if w.Timeout != "" {
			d, err := time.ParseDuration(w.Timeout)
			if err != nil || d <= 0 {
				return fmt.Errorf("webhook[%d] has invalid timeout %q", i, w.Timeout)
			}
		}
	}
	return nil
}

func isValidSeverity(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case SeverityInfo, SeverityWarning, SeverityCritical:
		return true
	default:
		return false
	}
}
