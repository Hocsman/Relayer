package adapters

import (
	"math/rand"
	"strings"
	"testing"
)

// referenceAppendDetectionText is the original per-rune implementation, kept as
// the behavioural specification of the buffered one. The rewrite was made for
// throughput only, so any observable difference is a bug.
func referenceAppendDetectionText(s *DetectionState, chunk []byte) (int, int, bool) {
	if s == nil || len(chunk) == 0 {
		return 0, 0, false
	}
	for _, character := range string(chunk) {
		switch character {
		case '\r':
			if index := strings.LastIndexByte(s.detectionText, '\n'); index >= 0 {
				s.detectionText = s.detectionText[:index+1]
			} else {
				s.detectionText = ""
			}
		case '\n':
			lineStart := strings.LastIndexByte(s.detectionText, '\n') + 1
			if strings.HasPrefix(strings.TrimSpace(s.detectionText[lineStart:]), "```") {
				s.inCodeFence = !s.inCodeFence
			}
			s.detectionText += string(character)
		case '\t':
			s.detectionText += string(character)
		default:
			if character >= 0x20 && (character < 0x7f || character >= 0xa0) {
				s.detectionText += string(character)
			}
		}
	}
	if len(s.detectionText) > detectionWindowSize {
		s.detectionText = s.detectionText[len(s.detectionText)-detectionWindowSize:]
	}
	return activeLineRange(s.detectionText)
}

// windowCorpus mixes the shapes that actually drive the window: CR rewrites,
// fenced blocks, control characters, tabs, multi-byte runes, empty lines and
// content long enough to force truncation.
func windowCorpus() [][]byte {
	return [][]byte{
		[]byte("Overwrite file? [Y/n]"),
		[]byte("progress 10%\rprogress 20%\rprogress 100%\n"),
		[]byte("```\nOverwrite file? [Y/n]\n```\n"),
		[]byte("  ```sh\n"),
		[]byte("\r"),
		[]byte("\r\n\r\n"),
		[]byte("\n"),
		[]byte("\t\tindented\n"),
		[]byte("caf\u00e9 \u00e9\u00e0\u00fc \u4f60\u597d \U0001F600\n"),
		[]byte("\x00\x01\x07\x1b bell and control\n"),
		[]byte("\x7f\u00a0 delete and nbsp\n"),
		[]byte(strings.Repeat("x", 300) + "\n"),
		[]byte(strings.Repeat("long line without newline ", 900)),
		[]byte(""),
		[]byte("Do you want to continue? (y/N) "),
	}
}

func TestAppendDetectionTextMatchesTheReferenceImplementation(t *testing.T) {
	corpus := windowCorpus()
	random := rand.New(rand.NewSource(20260828))

	for trial := 0; trial < 400; trial++ {
		fast := NewDetectionState("s", "a", "generic")
		reference := NewDetectionState("s", "a", "generic")

		for step := 0; step < 12; step++ {
			chunk := corpus[random.Intn(len(corpus))]
			gotStart, gotEnd, gotOK := fast.appendDetectionText(chunk)
			wantStart, wantEnd, wantOK := referenceAppendDetectionText(reference, chunk)

			if gotOK != wantOK || gotStart != wantStart || gotEnd != wantEnd {
				t.Fatalf("trial %d step %d chunk %q: range = (%d,%d,%v), want (%d,%d,%v)",
					trial, step, chunk, gotStart, gotEnd, gotOK, wantStart, wantEnd, wantOK)
			}
			if fast.detectionText != reference.detectionText {
				t.Fatalf("trial %d step %d chunk %q: window diverged\n got %q\nwant %q",
					trial, step, chunk, fast.detectionText, reference.detectionText)
			}
			if fast.inCodeFence != reference.inCodeFence {
				t.Fatalf("trial %d step %d chunk %q: code fence = %v, want %v",
					trial, step, chunk, fast.inCodeFence, reference.inCodeFence)
			}
		}
	}
}

// The window must stay bounded whatever arrives, including output that never
// contains a newline.
func TestAppendDetectionTextStaysBounded(t *testing.T) {
	state := NewDetectionState("s", "a", "generic")
	for i := 0; i < 64; i++ {
		state.appendDetectionText([]byte(strings.Repeat("y", 4096)))
		if len(state.detectionText) > detectionWindowSize {
			t.Fatalf("window grew to %d bytes, cap is %d", len(state.detectionText), detectionWindowSize)
		}
	}
}

func FuzzAppendDetectionText(f *testing.F) {
	for _, seed := range windowCorpus() {
		f.Add(seed)
	}
	f.Add([]byte("\r\r\r```\n\n```\r"))

	f.Fuzz(func(t *testing.T, chunk []byte) {
		fast := NewDetectionState("s", "a", "generic")
		reference := NewDetectionState("s", "a", "generic")

		gotStart, gotEnd, gotOK := fast.appendDetectionText(chunk)
		wantStart, wantEnd, wantOK := referenceAppendDetectionText(reference, chunk)

		if gotOK != wantOK || gotStart != wantStart || gotEnd != wantEnd ||
			fast.detectionText != reference.detectionText || fast.inCodeFence != reference.inCodeFence {
			t.Fatalf("diverged for %q:\n fast = (%d,%d,%v) %q fence=%v\n ref  = (%d,%d,%v) %q fence=%v",
				chunk, gotStart, gotEnd, gotOK, fast.detectionText, fast.inCodeFence,
				wantStart, wantEnd, wantOK, reference.detectionText, reference.inCodeFence)
		}
		if len(fast.detectionText) > detectionWindowSize {
			t.Fatalf("window grew to %d bytes for %q", len(fast.detectionText), chunk)
		}
		if gotOK && (gotStart < 0 || gotEnd > len(fast.detectionText) || gotStart > gotEnd) {
			t.Fatalf("active range (%d,%d) outside window of %d bytes", gotStart, gotEnd, len(fast.detectionText))
		}
	})
}

// BenchmarkAppendDetectionText guards the throughput of the hot path. It runs
// while the processor lock is held, so a regression here stalls detection,
// SendLine, and every snapshot the UI polls.
func BenchmarkAppendDetectionText(b *testing.B) {
	chunk := []byte(strings.Repeat("npm WARN deprecated some-package@1.2.3: please upgrade to v2\n", 68))
	b.SetBytes(int64(len(chunk)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		state := NewDetectionState("s", "a", "generic")
		for j := 0; j < 8; j++ {
			state.appendDetectionText(chunk)
		}
	}
}
