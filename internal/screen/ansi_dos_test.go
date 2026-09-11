package screen

import (
	"testing"
	"time"
)

func TestANSIDoSBillionCountDoesNotHang(t *testing.T) {
	s := New(80, 24)

	// Fill screen with test data
	_, _ = s.Write([]byte("Line 1\r\nLine 2\r\nLine 3\r\nLine 4\r\n"))

	sequences := []struct {
		name string
		seq  string
	}{
		{name: "Insert Lines (CSI L)", seq: "\x1b[1000000000L"},
		{name: "Delete Lines (CSI M)", seq: "\x1b[1000000000M"},
		{name: "Scroll Up (CSI S)", seq: "\x1b[1000000000S"},
		{name: "Scroll Down (CSI T)", seq: "\x1b[1000000000T"},
	}

	for _, tc := range sequences {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			_, err := s.Write([]byte(tc.seq))
			duration := time.Since(start)

			if err != nil {
				t.Fatalf("Write error: %v", err)
			}
			if duration > 100*time.Millisecond {
				t.Fatalf("Operation took too long (%v), loop was likely unbounded", duration)
			}
		})
	}
}
