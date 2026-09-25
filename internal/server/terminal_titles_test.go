package server

import (
	"strings"
	"testing"
	"time"
)

// A window title and a current directory report name the host, not anything
// the web terminal shows: they are removed from the output every client
// receives, and everything else is kept.
func TestTheOutputClientsReceiveNamesNoHostPath(t *testing.T) {
	for _, test := range []struct{ in, want string }{
		{"\x1b[2J\x1b[m\x1b[Hagent ready\r\n\x1b]0;C:\\Users\\alice\\agent.exe\a\x1b[?25h", "\x1b[2J\x1b[m\x1b[Hagent ready\r\n\x1b[?25h"},
		{"a\x1b]2;/home/alice/project\x1b\\b", "ab"},
		{"a\x1b]7;file://host/home/alice/project\ab", "ab"},
		{"a\x1b]1;icon\ab\x1b]0;title\a", "ab"},
		{"kept \x1b]8;;https://example.com\aa link\x1b]8;;\a", "kept \x1b]8;;https://example.com\aa link\x1b]8;;\a"},
		{"cut \x1b]0;C:\\Users\\al", "cut "},
		{"plain text", "plain text"},
	} {
		if got := withoutHostReports(test.in); got != test.want {
			t.Errorf("withoutHostReports(%q) = %q, want %q", test.in, got, test.want)
		}
	}
}

// On Windows the pseudo console titles every agent's terminal with its full
// path; no client, a viewer included, receives it.
func TestAWebAgentsOutputCarriesNoWindowTitle(t *testing.T) {
	g := startWebRun(t, webRun{
		agents: []webAgent{{id: "web-listen", mode: webAgentListen, adapter: "generic"}},
	})
	g.awaitScreen("web-listen", "agent ready", 30*time.Second)
	for _, agent := range g.ctrl.GetState().Agents {
		if strings.Contains(agent.Output, "\x1b]0;") || strings.Contains(agent.Output, "\x1b]2;") {
			t.Fatalf("the output every client reads carries a window title: %q", agent.Output)
		}
	}
	for _, frame := range g.broadcast(eventSnapshot) {
		if snapshot, ok := frame.payload.(SnapshotEvent); ok && strings.Contains(snapshot.Output, "\x1b]0;") {
			t.Fatalf("a snapshot carries a window title: %q", snapshot.Output)
		}
	}
}
