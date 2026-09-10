package screen

import (
	"strings"
	"testing"
)

// Text, TextAndBurst and VisibleText used to lay the screen out three times,
// and the three implementations already disagreed: one ended with
// `if joining || current.Len() > 0`, the other two with `if current.Len() > 0`.
// They now share one serialisation, which is what makes a byte offset held by
// the detector mean the same thing as a row held by the screen. This test is
// the reason that unification is allowed to exist.
func TestRenderAgreesWithText(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		width int
		input string
	}{
		{"plain lines", 20, "one\r\ntwo\r\nthree"},
		{"a line wrapped by the margin", 10, "a question that runs past the margin"},
		{"trailing blank rows", 20, "\x1b[1;1Hone"},
		{"an erased screen", 20, "one\r\ntwo\x1b[2J"},
		{"a wrapped line at the very bottom", 10, "\x1b[6;1Hthis line wraps at the bottom edge"},
		{"content scrolled into the scrollback", 10, strings.Repeat("line\r\n", 12)},
		{"a frame painted by addressing", 20, "\x1b[2J\x1b[1;1Htop\x1b[3;1Hbottom"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			grid := New(testCase.width, 6)
			grid.Write([]byte(testCase.input))

			text := grid.Text()
			rendered, _, anchors := grid.Render()
			if rendered != text {
				t.Fatalf("Render disagrees with Text:\nRender %q\nText   %q", rendered, text)
			}
			burstText, _ := grid.TextAndBurst()
			if burstText != text {
				t.Fatalf("TextAndBurst disagrees with Text:\n%q\n%q", burstText, text)
			}

			// Every line start must resolve to the row that painted it, and
			// nothing may resolve outside the text.
			offset := 0
			for _, line := range strings.Split(text, "\n") {
				if _, ok := anchors.RowAt(offset); !ok && text != "" {
					t.Fatalf("no row for offset %d of %q", offset, text)
				}
				offset += len(line) + 1
			}
			if _, ok := anchors.RowAt(len(text) + 1); ok && text != "" {
				t.Fatal("an offset past the end resolved to a row")
			}
			if _, ok := anchors.RowAt(-1); ok {
				t.Fatal("a negative offset resolved to a row")
			}
		})
	}
}

// A row keeps its name while it is repainted, and loses it when it leaves.
//
// An erase is not a departure: a full-screen agent erases the frame it is about
// to repaint on every tick, and a name that changed every tick would name
// nothing.
func TestRowIdentitySurvivesRepaintAndErase(t *testing.T) {
	grid := New(30, 6)
	grid.Write([]byte("\x1b[2;1HOverwrite file? [Y/n]"))
	id, _, found := grid.UniqueRowShowing("Overwrite file? [Y/n]")
	if !found {
		t.Fatal("the question was not on the grid")
	}

	grid.Write([]byte("\x1b[2;1H\x1b[2KOverwrite file? [Y/n]"))
	if !grid.RowShows(id, "Overwrite file? [Y/n]") {
		t.Fatal("erasing and repainting the same row renamed it")
	}
	grid.Write([]byte("\x1b[2J\x1b[2;1HOverwrite file? [Y/n]"))
	if !grid.RowShows(id, "Overwrite file? [Y/n]") {
		t.Fatal("erasing the display renamed the row")
	}
	grid.Write([]byte("\x1b[2;1H\x1b[2Ksomething else"))
	if grid.RowShows(id, "Overwrite file? [Y/n]") {
		t.Fatal("a row rewritten with other content still answers for the question")
	}
}

// The coordinate that made this necessary: a scroll region whose top is row 0
// used to move the origin while the rows below the region had not moved at all.
func TestARowBelowAScrollRegionKeepsItsIdentity(t *testing.T) {
	grid := New(40, 12)
	grid.Write([]byte("\x1b[6;1HQuestion? [y/n]"))
	id, _, found := grid.UniqueRowShowing("Question? [y/n]")
	if !found {
		t.Fatal("the question was not on the grid")
	}
	// Region rows 0..3, one scroll inside it. Row 5 does not move.
	grid.Write([]byte("\x1b[1;4r\x1b[4;1H\n"))
	if !grid.RowShows(id, "Question? [y/n]") {
		t.Fatal("a scroll region moved the identity of a row it never touched")
	}
}

// A pager takes the whole grid away and gives it back. Identities must not be
// reused in between, or a memory anchored on one would follow the wrong line
// home. Resize is the path that mints rows on the saved screen.
func TestAlternateScreenDoesNotReuseRowIdentities(t *testing.T) {
	grid := New(30, 6)
	grid.Write([]byte("\x1b[2;1HOverwrite file? [Y/n]"))
	id, _, found := grid.UniqueRowShowing("Overwrite file? [Y/n]")
	if !found {
		t.Fatal("the question was not on the grid")
	}

	grid.Write([]byte("\x1b[?1049h"))
	grid.Resize(40, 10)
	grid.Write([]byte("\x1b[1;1Hpager output"))
	if grid.RowShows(id, "Overwrite file? [Y/n]") {
		t.Fatal("the alternate screen answered for a row of the primary")
	}
	grid.Write([]byte("\x1b[?1049l"))

	// Whatever the alternate screen minted, no live row may carry the saved id
	// unless it is the saved row itself.
	for index := range grid.rows {
		if grid.rows[index].id != id {
			continue
		}
		if !strings.Contains(logicalLineAt(grid.rows, index), "Overwrite file? [Y/n]") {
			t.Fatalf("row %d reuses the identity of the question row", index)
		}
	}
}
