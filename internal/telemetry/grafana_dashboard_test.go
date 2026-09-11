package telemetry

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func findProjectRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not find project root containing go.mod")
	return ""
}

func TestGrafanaDashboardJSONValidity(t *testing.T) {
	root := findProjectRoot(t)

	dashboardPath := filepath.Join(root, "telemetry", "grafana", "dashboards", "relayer-dashboard.json")
	docsPath := filepath.Join(root, "docs", "grafana-dashboard.json")

	data1, err := os.ReadFile(dashboardPath)
	if err != nil {
		t.Fatalf("failed to read %s: %v", dashboardPath, err)
	}

	data2, err := os.ReadFile(docsPath)
	if err != nil {
		t.Fatalf("failed to read %s: %v", docsPath, err)
	}

	// 1. Both files must have identical content
	if !bytes.Equal(data1, data2) {
		t.Error("telemetry/grafana/dashboards/relayer-dashboard.json and docs/grafana-dashboard.json differ")
	}

	// 2. Validate JSON structure
	var parsed struct {
		Title  string `json:"title"`
		UID    string `json:"uid"`
		Panels []struct {
			ID      int    `json:"id"`
			Title   string `json:"title"`
			Type    string `json:"type"`
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}

	if err := json.Unmarshal(data1, &parsed); err != nil {
		t.Fatalf("unmarshal dashboard JSON failed: %v", err)
	}

	if parsed.UID != "relayer-supervisor-overview" {
		t.Errorf("expected UID 'relayer-supervisor-overview', got %q", parsed.UID)
	}
	if !strings.Contains(parsed.Title, "Relayer") {
		t.Errorf("expected title to contain 'Relayer', got %q", parsed.Title)
	}
	if len(parsed.Panels) < 8 {
		t.Errorf("expected at least 8 panels in dashboard, got %d", len(parsed.Panels))
	}

	// 3. Verify all PromQL queries reference known Relayer metrics
	knownMetrics := []string{
		"relayer_sessions_active",
		"relayer_events_pending",
		"relayer_sessions_total",
		"relayer_events_detected_total",
		"relayer_events_withdrawn_total",
		"relayer_decisions_total",
		"relayer_operator_inputs_total",
		"relayer_guardrail_violations_total",
		"relayer_decision_duration_seconds",
	}

	exprCount := 0
	for _, p := range parsed.Panels {
		for _, target := range p.Targets {
			if strings.TrimSpace(target.Expr) == "" {
				continue
			}
			exprCount++
			matched := false
			for _, m := range knownMetrics {
				if strings.Contains(target.Expr, m) {
					matched = true
					break
				}
			}
			if !matched && !strings.Contains(target.Expr, "vector(0)") {
				t.Errorf("panel %q (id %d) query %q does not reference any known relayer metric", p.Title, p.ID, target.Expr)
			}
		}
	}

	if exprCount < 5 {
		t.Errorf("expected at least 5 PromQL queries in dashboard, found %d", exprCount)
	}
}

func TestTelemetryYAMLConfigurationsValidity(t *testing.T) {
	root := findProjectRoot(t)

	yamlFiles := []string{
		filepath.Join(root, "docker-compose.telemetry.yml"),
		filepath.Join(root, "telemetry", "prometheus", "prometheus.yml"),
		filepath.Join(root, "telemetry", "grafana", "provisioning", "datasources", "datasources.yml"),
		filepath.Join(root, "telemetry", "grafana", "provisioning", "dashboards", "dashboards.yml"),
	}

	for _, path := range yamlFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("failed to read %s: %v", path, err)
		}

		var parsed interface{}
		if err := yaml.Unmarshal(data, &parsed); err != nil {
			t.Errorf("invalid YAML in %s: %v", path, err)
		}
	}
}
