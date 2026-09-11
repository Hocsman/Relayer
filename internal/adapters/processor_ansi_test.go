package adapters

import (
	"strings"
	"testing"
)

func TestProcessorAnsiOutputPreservesColorsAndProgressCarriageReturns(t *testing.T) {
	processor := newGenericTestProcessor(t, 4096, Hooks{})

	// ANSI colored string with a carriage return progress bar overwrite
	stream := "\x1b[32m[INFO]\x1b[0m Starting job...\r\x1b[33m[WARN]\x1b[0m Step 1: 50%\r\x1b[32m[OK]\x1b[0m Done 100%\nOverwrite? [Y/n] "
	if err := processor.Consume([]byte(stream)); err != nil {
		t.Fatalf("Consume: %v", err)
	}

	// Output() MUST strip ANSI according to existing Relayer contract
	clean := processor.Output()
	if strings.Contains(clean, "\x1b") {
		t.Fatalf("Output() must strip ANSI sequences, got: %q", clean)
	}

	// AnsiOutput() MUST preserve ANSI sequences for xterm.js
	ansi := processor.AnsiOutput()
	if !strings.Contains(ansi, "\x1b[32m[INFO]\x1b[0m") {
		t.Fatalf("AnsiOutput() did not preserve green ANSI sequence, got: %q", ansi)
	}
	if !strings.Contains(ansi, "\x1b[33m[WARN]\x1b[0m") {
		t.Fatalf("AnsiOutput() did not preserve yellow ANSI sequence, got: %q", ansi)
	}
	if !strings.Contains(ansi, "\r") {
		t.Fatalf("AnsiOutput() did not preserve carriage return for progress overwrite, got: %q", ansi)
	}
}

func TestProcessorAnsiOutputFallbackWhenEmpty(t *testing.T) {
	processor := newGenericTestProcessor(t, 4096, Hooks{})

	if got := processor.AnsiOutput(); got != "" {
		t.Fatalf("expected empty AnsiOutput, got %q", got)
	}
}
