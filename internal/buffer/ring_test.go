package buffer_test

import (
	"testing"

	"github.com/Hocsman/Relayer/internal/buffer"
)

func TestRetainsNewestBytesInOrder(t *testing.T) {
	target := buffer.New(5)
	writes := []struct {
		input string
		want  string
	}{
		{input: "abc", want: "abc"},
		{input: "de", want: "abcde"},
		{input: "f", want: "bcdef"},
		{input: "1234567", want: "34567"},
	}

	for _, test := range writes {
		written, err := target.Write([]byte(test.input))
		if err != nil {
			t.Fatalf("Write(%q) returned an error: %v", test.input, err)
		}
		if written != len(test.input) {
			t.Fatalf("Write(%q) reported %d bytes, want %d", test.input, written, len(test.input))
		}
		if got := target.String(); got != test.want {
			t.Fatalf("after Write(%q), buffer is %q, want %q", test.input, got, test.want)
		}
		if target.Len() > target.Capacity() {
			t.Fatalf("length %d exceeds capacity %d", target.Len(), target.Capacity())
		}
	}
}

func TestBytesReturnsIndependentCopy(t *testing.T) {
	target := buffer.New(4)
	_, _ = target.Write([]byte("test"))

	snapshot := target.Bytes()
	snapshot[0] = 'b'
	if got := target.String(); got != "test" {
		t.Fatalf("mutating Bytes result changed buffer to %q", got)
	}
}

func TestNonPositiveCapacityIsClamped(t *testing.T) {
	for _, capacity := range []int{0, -1} {
		target := buffer.New(capacity)
		if got := target.Capacity(); got != 1 {
			t.Fatalf("New(%d) capacity = %d, want 1", capacity, got)
		}
		_, _ = target.Write([]byte("ab"))
		if got := target.String(); got != "b" {
			t.Fatalf("New(%d) retained %q, want %q", capacity, got, "b")
		}
	}
}

func TestEmptyWriteDoesNotChangeBuffer(t *testing.T) {
	target := buffer.New(3)
	_, _ = target.Write([]byte("abc"))

	written, err := target.Write(nil)
	if err != nil || written != 0 {
		t.Fatalf("empty Write returned (%d, %v), want (0, nil)", written, err)
	}
	if got := target.String(); got != "abc" {
		t.Fatalf("empty Write changed buffer to %q", got)
	}
}
