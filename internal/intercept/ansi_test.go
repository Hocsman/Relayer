package intercept

import "testing"

func TestSplitIncompleteANSISeparatesTrailingSequence(t *testing.T) {
	complete, carry := splitIncompleteANSI("ready\x1b[31mred\x1b[")
	if complete != "ready\x1b[31mred" || carry != "\x1b[" {
		t.Fatalf("splitIncompleteANSI = %q / %q", complete, carry)
	}
}

func TestSanitizeTerminalTextNormalizesCarriageReturnsAndControls(t *testing.T) {
	input := "one\r\ntwo\rthree\x00\ttab"
	if got, want := sanitizeTerminalText(input), "one\ntwo\nthree\ttab"; got != want {
		t.Fatalf("sanitizeTerminalText(%q) = %q, want %q", input, got, want)
	}
}
