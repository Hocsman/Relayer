package screen

import (
	"strings"
	"testing"
)

func render(t *testing.T, width, height int, writes ...string) string {
	t.Helper()
	s := New(width, height)
	for _, write := range writes {
		if _, err := s.Write([]byte(write)); err != nil {
			t.Fatal(err)
		}
	}
	return s.Text()
}

// The behaviour the whole package exists for: what an agent erases is gone, and
// what it addresses lands where it was addressed.
func TestRepaintIsRendered(t *testing.T) {
	for _, test := range []struct {
		name    string
		input   string
		want    string
		notWant string
	}{
		{
			name:    "erase display removes what it erased",
			input:   "Overwrite file? [Y/n]\x1b[2J\x1b[HBuilding",
			want:    "Building",
			notWant: "Overwrite",
		},
		{
			name:  "cursor addressing lands inside the frame",
			input: "\x1b[2J\x1b[H\x1b[1;1H+------------+\x1b[2;1H|            |\x1b[3;1H+------------+\x1b[2;3HOverwrite?",
			want:  "| Overwrite? |",
		},
		{
			name:    "erase line clears only to the right",
			input:   "keep this and drop that\r\x1b[15Cthen\x1b[K!",
			want:    "keep this and dthen!",
			notWant: "drop that",
		},
		{
			name:  "carriage return rewrites the same row",
			input: "working 10%\rworking 90%",
			want:  "working 90%",
		},
		{
			name:  "backspace steps back over a cell",
			input: "abcX\b!",
			want:  "abc!",
		},
		{
			name:    "the alternate screen does not leak into the primary one",
			input:   "primary question\n\x1b[?1049hALTERNATE PAINT\x1b[?1049l",
			want:    "primary question",
			notWant: "ALTERNATE",
		},
		{
			name:  "delete characters closes the gap",
			input: "abcdef\r\x1b[3C\x1b[2P",
			want:  "abcf",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := render(t, 40, 10, test.input)
			if test.want != "" && !strings.Contains(got, test.want) {
				t.Fatalf("want %q in:\n%s", test.want, got)
			}
			if test.notWant != "" && strings.Contains(got, test.notWant) {
				t.Fatalf("did not want %q in:\n%s", test.notWant, got)
			}
		})
	}
}

// Append-only output must render exactly as it was written. Every existing
// Relayer fixture depends on this, and a screen model that changed plain output
// would trade one silent failure for another.
func TestAppendOnlyOutputIsUnchanged(t *testing.T) {
	const plain = "Building the project\nCompiling 12 files\nOverwrite file? [Y/n]"
	if got := render(t, 80, 24, plain); got != plain {
		t.Fatalf("plain output changed:\ngot  %q\nwant %q", got, plain)
	}
}

// The result must not depend on where a read happened to end — the same defect,
// one layer down. A screen model is only worth having if it is exact here.
func TestRenderingIsIndependentOfChunkBoundaries(t *testing.T) {
	const input = "\x1b[2J\x1b[H\x1b[1;1H+------------+\x1b[2;1H|            |" +
		"\x1b[3;1H+------------+\x1b[2;3HOverwrite?\x1b[5;1HPress enter"
	whole := render(t, 40, 10, input)

	for _, size := range []int{1, 2, 3, 7, 13, 64} {
		chunks := []string{}
		for start := 0; start < len(input); start += size {
			end := start + size
			if end > len(input) {
				end = len(input)
			}
			chunks = append(chunks, input[start:end])
		}
		if split := render(t, 40, 10, chunks...); split != whole {
			t.Fatalf("chunk size %d rendered differently:\ngot\n%s\nwant\n%s", size, split, whole)
		}
	}
}

// A sequence the screen does nothing with must still be consumed. If it were
// printed instead, its bytes would become text a pattern could match — and an
// unrecognised erase would leave stale cells live, which is the failure this
// package removes.
func TestUnhandledSequencesAreConsumedNotPrinted(t *testing.T) {
	for _, test := range []struct {
		name  string
		input string
	}{
		{name: "SGR colour", input: "\x1b[31;1mred\x1b[0m"},
		{name: "OSC window title", input: "\x1b]0;a title\x07text"},
		{name: "OSC string terminator", input: "\x1b]8;;https://example.com\x1b\\text"},
		{name: "DCS", input: "\x1bPsome device string\x1b\\text"},
		{name: "APC", input: "\x1b_application command\x1b\\text"},
		{name: "device status report", input: "\x1b[6ntext"},
		{name: "bracketed paste mode", input: "\x1b[?2004htext"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := render(t, 40, 6, test.input)
			for _, forbidden := range []string{"\x1b", "31;1m", "a title", "device string", "application command", "2004"} {
				if strings.Contains(got, forbidden) {
					t.Fatalf("%q leaked into the rendered text: %q", forbidden, got)
				}
			}
			if !strings.Contains(got, "text") && !strings.Contains(got, "red") {
				t.Fatalf("the printable text was lost: %q", got)
			}
		})
	}
}

// A question broken by the right margin is one question. Joining on the wrap
// flag is what lets a pattern match across it.
func TestWrappedLinesRejoin(t *testing.T) {
	s := New(20, 6)
	if _, err := s.Write([]byte("Do you want to continue with this?")); err != nil {
		t.Fatal(err)
	}
	if got := s.Text(); !strings.Contains(got, "Do you want to continue with this?") {
		t.Fatalf("the wrapped question did not rejoin: %q", got)
	}
	if got := s.CursorLine(); !strings.Contains(got, "Do you want to continue") {
		t.Fatalf("the cursor line lost the start of its own question: %q", got)
	}
}

// Output that scrolls past the screen must survive, or an append-only agent
// would lose everything above the last row.
func TestScrolledOutputIsKeptInScrollback(t *testing.T) {
	lines := make([]string, 0, 30)
	for index := range 30 {
		lines = append(lines, "line "+string(rune('a'+index%26)))
	}
	got := render(t, 40, 5, strings.Join(lines, "\n"))
	if !strings.Contains(got, lines[0]) {
		t.Fatalf("the first line scrolled away entirely:\n%s", got)
	}
	if !strings.Contains(got, lines[len(lines)-1]) {
		t.Fatalf("the last line is missing:\n%s", got)
	}
}

// A grid must never be talked into allocating without bound by a hostile agent.
func TestSizeIsClamped(t *testing.T) {
	for _, test := range []struct{ width, height int }{
		{0, 0}, {-5, -5}, {1 << 20, 1 << 20}, {1, 1},
	} {
		s := New(test.width, test.height)
		width, height := s.Size()
		if width < MinWidth || width > MaxWidth || height < MinHeight || height > MaxHeight {
			t.Fatalf("New(%d,%d) produced %dx%d", test.width, test.height, width, height)
		}
		// It must still work at whatever size it settled on.
		if _, err := s.Write([]byte("\x1b[99;99Hx")); err != nil {
			t.Fatal(err)
		}
	}
}

// Absolute addressing beyond the grid is clamped rather than panicking. Real
// captures address row 24 and column 71 without ever stating the screen size.
func TestAddressingBeyondTheGridIsClamped(t *testing.T) {
	s := New(20, 5)
	if _, err := s.Write([]byte("\x1b[99;99Hedge\x1b[1;1Htop")); err != nil {
		t.Fatal(err)
	}
	got := s.Text()
	if !strings.Contains(got, "top") {
		t.Fatalf("the in-range write was lost: %q", got)
	}
}

// The switch that lets a caller adopt the rendered screen only where a byte
// stream is actually wrong. An agent that merely prints must be reported as not
// repainting, or every existing behaviour would change for no reason.
func TestRepaintedIsFalseForAnAppendOnlyAgent(t *testing.T) {
	for _, test := range []struct {
		name  string
		input string
		want  bool
	}{
		{name: "plain text", input: "Building\nCompiling\nOverwrite file? [Y/n]", want: false},
		{name: "carriage return progress", input: "10%\r50%\r100%", want: false},
		{name: "backspace and tab", input: "abc\b\bx\ty", want: false},
		{name: "colour only", input: "\x1b[31mred\x1b[0m plain", want: false},
		{name: "cursor forward spacing", input: "a\x1b[5Cb", want: false},
		{name: "window title", input: "\x1b]0;title\x07text", want: false},
		{name: "erase display", input: "text\x1b[2J", want: true},
		{name: "absolute addressing", input: "\x1b[3;1Htext", want: true},
		{name: "erase line", input: "text\x1b[K", want: true},
		{name: "alternate screen", input: "\x1b[?1049h", want: true},
		{name: "scroll region", input: "\x1b[2;5r", want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := New(40, 10)
			if _, err := s.Write([]byte(test.input)); err != nil {
				t.Fatal(err)
			}
			if got := s.Repainted(); got != test.want {
				t.Fatalf("Repainted() = %v, want %v", got, test.want)
			}
		})
	}
}

// An unmeasured size must fall back to something a frame fits in. Clamping zero
// yields the minimum, so ordering this wrong produced a 2x1 grid — which is
// within bounds, and therefore invisible to a test that only checks bounds.
func TestUnmeasuredSizeUsesTheDefaultNotTheMinimum(t *testing.T) {
	for _, test := range []struct{ width, height int }{{0, 0}, {-1, -1}} {
		s := New(test.width, test.height)
		width, height := s.Size()
		if width != defaultWidth || height != defaultHeight {
			t.Fatalf("New(%d,%d) = %dx%d, want the %dx%d default",
				test.width, test.height, width, height, defaultWidth, defaultHeight)
		}
	}
	// An explicit tiny size is still honoured; only the unset one defaults.
	if width, _ := New(10, 3).Size(); width != 10 {
		t.Fatalf("an explicit width was overridden: %d", width)
	}
}

// The burst offset is what lets detection judge the current screen instead of
// the whole history. Getting it wrong in the narrowing direction hides a
// question; getting it wrong in the widening direction only costs a dismissal.
func TestTextAndBurstMarksWhatTheWriteTouched(t *testing.T) {
	s := New(40, 10)
	if _, err := s.Write([]byte("first line\nsecond line\n")); err != nil {
		t.Fatal(err)
	}
	s.ClearDirty()

	if _, err := s.Write([]byte("third line")); err != nil {
		t.Fatal(err)
	}
	text, burst := s.TextAndBurst()
	if got := text[burst:]; got != "third line" {
		t.Fatalf("burst covers %q, want just the new line", got)
	}

	// A frame painted out of order: the burst must start at the EARLIEST row
	// touched, not the last one written.
	s.ClearDirty()
	if _, err := s.Write([]byte("\x1b[8;1Hfooter\x1b[6;1Hquestion?")); err != nil {
		t.Fatal(err)
	}
	text, burst = s.TextAndBurst()
	region := text[burst:]
	if !strings.Contains(region, "question?") || !strings.Contains(region, "footer") {
		t.Fatalf("the burst lost part of an out-of-order paint: %q", region)
	}
	if strings.Contains(region, "first line") {
		t.Fatalf("the burst reached back into untouched history: %q", region)
	}
}

// A write that changes nothing on screen must not present the whole screen as
// new work.
func TestBurstIsEmptyWhenNothingChanged(t *testing.T) {
	s := New(40, 10)
	if _, err := s.Write([]byte("settled\n")); err != nil {
		t.Fatal(err)
	}
	s.ClearDirty()
	if _, err := s.Write([]byte("\x1b[31m\x1b[0m")); err != nil {
		t.Fatal(err)
	}
	text, burst := s.TextAndBurst()
	if burst != len(text) {
		t.Fatalf("a colour-only write claimed a burst of %q", text[burst:])
	}
}

// Resizing to the size already in use must not look like the agent repainted.
// A terminal interface resizes on every render, and rebuilding the rows marks
// them all touched — so without this the actionable region would be the whole
// screen forever.
func TestRedundantResizeDoesNotInflateTheBurst(t *testing.T) {
	s := New(80, 24)
	if _, err := s.Write([]byte("settled output\n")); err != nil {
		t.Fatal(err)
	}
	s.ClearDirty()

	s.Resize(80, 24)
	if _, err := s.Write([]byte("new line")); err != nil {
		t.Fatal(err)
	}
	text, burst := s.TextAndBurst()
	if got := text[burst:]; got != "new line" {
		t.Fatalf("a redundant resize widened the burst to %q", got)
	}

	// A real resize is still applied.
	s.Resize(40, 12)
	if width, height := s.Size(); width != 40 || height != 12 {
		t.Fatalf("a real resize was ignored: %dx%d", width, height)
	}
}

// Evicted counts what left the view, and only that.
//
// An erase removes content without moving a line, and it is precisely the
// signal that a question stopped being asked; counting it there would make the
// counter useless for telling an erase from a scroll.
func TestEvictedCountsMovementAndNotErasure(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		input   string
		evicted bool
	}{
		{"a line feed at the bottom scrolls", "\x1b[6;1H\n", true},
		{"inserting a line moves the ones below", "\x1b[2;1H\x1b[L", true},
		{"deleting a line moves the ones below", "\x1b[2;1H\x1b[M", true},
		{"switching to the alternate screen takes the grid away", "\x1b[?1049h", true},
		{"erasing the display moves nothing", "\x1b[2J", false},
		{"erasing a line moves nothing", "\x1b[2;1H\x1b[2K", false},
		{"printing in place moves nothing", "\x1b[1;1Hhello", false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			grid := New(20, 6)
			grid.Write([]byte("\x1b[1;1Hone\x1b[2;1Htwo\x1b[3;1Hthree"))
			before := grid.Evicted()
			grid.Write([]byte(testCase.input))
			moved := grid.Evicted() > before
			if moved != testCase.evicted {
				t.Fatalf("Evicted moved by %d, want moved=%t", grid.Evicted()-before, testCase.evicted)
			}
		})
	}
}
