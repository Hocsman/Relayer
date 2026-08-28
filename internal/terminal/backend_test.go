package terminal

import (
	"errors"
	"testing"
)

func TestSizeNormalizeClampsTerminalDimensions(t *testing.T) {
	tests := []struct {
		input Size
		want  Size
	}{
		{input: Size{}, want: Size{Columns: 1, Rows: 1}},
		{input: Size{Columns: 80, Rows: 24}, want: Size{Columns: 80, Rows: 24}},
		{input: Size{Columns: 100000, Rows: 100000}, want: Size{Columns: 65535, Rows: 65535}},
	}
	for _, test := range tests {
		if got := test.input.Normalize(); got != test.want {
			t.Fatalf("Normalize(%#v) = %#v, want %#v", test.input, got, test.want)
		}
	}
}

func TestOperationErrorPreservesSentinelWithoutLeakingExtraContext(t *testing.T) {
	err := &OperationError{
		Backend:   "tmux",
		Operation: "resize",
		SessionID: "agent-a",
		Err:       ErrSessionNotFound,
	}
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("OperationError does not unwrap its cause: %v", err)
	}
	if got, want := err.Error(), "tmux resize (session agent-a): terminal session not found"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}
