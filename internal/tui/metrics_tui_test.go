package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestMetricsOverlayToggleAndRender(t *testing.T) {
	application, _, _ := newModelHarness(t)

	if application.MetricsActive() {
		t.Fatal("expected metrics to be initially inactive")
	}

	// Press 'm' to open metrics
	application, _ = updateModel(t, application, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m'}})
	if !application.MetricsActive() {
		t.Fatal("expected metrics overlay to be active after pressing 'm'")
	}

	// Verify View() renders metrics
	view := application.View()
	if !strings.Contains(view, "RELAYER OBSERVABILITY & SESSION METRICS") {
		t.Fatalf("expected view to contain metrics header, got:\n%s", view)
	}
	if !strings.Contains(view, "UPTIME:") || !strings.Contains(view, "DECISIONS:") {
		t.Fatalf("expected view to contain uptime and decisions metrics, got:\n%s", view)
	}

	// Press 'm' again to close metrics
	application, _ = updateModel(t, application, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m'}})
	if application.MetricsActive() {
		t.Fatal("expected metrics overlay to be closed after pressing 'm' again")
	}

	// Open again with 'M' (uppercase)
	application, _ = updateModel(t, application, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'M'}})
	if !application.MetricsActive() {
		t.Fatal("expected metrics overlay to be active after pressing 'M'")
	}

	// Close with Esc
	application, _ = updateModel(t, application, tea.KeyMsg{Type: tea.KeyEsc})
	if application.MetricsActive() {
		t.Fatal("expected metrics overlay to be closed after pressing Esc")
	}
}

func TestMetricsTrackingCountersAndLatencies(t *testing.T) {
	application, _, _ := newModelHarness(t)

	application.humanAllows = 3
	application.humanDenies = 1
	application.autoAllows = 5
	application.autoDenies = 2
	application.guardrailBlocks = 4
	application.operatorInputs = 2
	application.latencies = []time.Duration{500 * time.Millisecond, 1500 * time.Millisecond}

	application.ToggleMetrics()
	if !application.MetricsActive() {
		t.Fatal("ToggleMetrics did not activate metrics")
	}

	view := application.View()
	if !strings.Contains(view, "11 total") {
		t.Fatalf("expected 11 total decisions in view, got:\n%s", view)
	}
	if !strings.Contains(view, "Human Operator:") || !strings.Contains(view, "Allow: 3, Deny: 1") {
		t.Fatalf("expected human decision breakdown in view, got:\n%s", view)
	}
	if !strings.Contains(view, "Automated Rules:") || !strings.Contains(view, "Allow: 5, Deny: 2") {
		t.Fatalf("expected auto decision breakdown in view, got:\n%s", view)
	}
	if !strings.Contains(view, "4 intercept(s) blocked") {
		t.Fatalf("expected guardrail intercepts in view, got:\n%s", view)
	}
	if !strings.Contains(view, "1.00s") {
		t.Fatalf("expected 1.00s average latency in view, got:\n%s", view)
	}
}
