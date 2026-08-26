package tmuxbackend

import (
	"regexp"
	"strings"
	"testing"
)

func TestSessionNameIsStableIsolatedAndSafe(t *testing.T) {
	t.Parallel()
	hostile := "Agent ; $(touch /tmp/no) '`\n⚠️"
	first := SessionName("run one", hostile)
	if first != SessionName("run one", hostile) {
		t.Fatalf("SessionName is not stable: %q", first)
	}
	if first == SessionName("run two", hostile) {
		t.Fatalf("run IDs did not isolate session name %q", first)
	}
	if first == SessionName("run one", "Agent---touch tmp no") {
		t.Fatalf("hash did not disambiguate colliding slugs: %q", first)
	}
	if !strings.HasPrefix(first, "relayer-") {
		t.Fatalf("session name %q lacks Relayer prefix", first)
	}
	if !regexp.MustCompile(`^[a-z0-9-]+$`).MatchString(first) {
		t.Fatalf("session name contains unsafe characters: %q", first)
	}
	if len(first) > 64 {
		t.Fatalf("session name is unexpectedly long: %d (%q)", len(first), first)
	}
}

func TestSessionNameHandlesEmptyAndUnicodeOnlyComponents(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		SessionName("", ""),
		SessionName("⚠️", "🤖"),
	} {
		if !regexp.MustCompile(`^relayer-[a-z0-9-]+$`).MatchString(name) {
			t.Fatalf("fallback name is unsafe: %q", name)
		}
	}
}
