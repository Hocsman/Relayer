package screen

import (
	"strings"
	"testing"
)

// A full-screen program parks the primary screen and gives it back unchanged.
// A row parked with it is on no visible grid, so RowState answers for it as for
// a row that is gone; RowParked is how a caller tells the two apart, and it has
// to name the primary rows only, and only while they are parked.
func TestARowOfThePrimaryScreenIsParkedWhileTheAlternateOneIsShown(t *testing.T) {
	grid := New(30, 6)
	grid.Write([]byte("\x1b[2;1HOverwrite file? [Y/n]"))
	id, _, found := grid.UniqueRowShowing("Overwrite file? [Y/n]")
	if !found {
		t.Fatal("the question was not on the grid")
	}
	if grid.OnAlternate() || grid.RowParked(id) {
		t.Fatal("nothing is parked while the primary screen is shown")
	}

	grid.Write([]byte("\x1b[?1049h\x1b[1;1Hpager output"))
	if !grid.OnAlternate() {
		t.Fatal("the alternate screen is shown and OnAlternate says it is not")
	}
	if present, _ := grid.RowState(id); present {
		t.Fatal("a parked row is reported on the visible grid")
	}
	if !grid.RowParked(id) {
		t.Fatal("the question's row is not reported parked while the pager has the screen")
	}
	pager, _, found := grid.UniqueRowShowing("pager output")
	if !found {
		t.Fatal("the pager's line was not on the grid")
	}
	if grid.RowParked(pager) || grid.RowParked(0) {
		t.Fatal("a row of the alternate screen, or no row at all, is reported parked")
	}

	grid.Write([]byte("\x1b[?1049l"))
	if grid.OnAlternate() || grid.RowParked(id) {
		t.Fatal("the primary screen is back and its row is still reported parked")
	}
	if !grid.RowShows(id, "Overwrite file? [Y/n]") {
		t.Fatal("the primary screen came back without the question on its row")
	}
}

// A terminal that loses height keeps the cursor's row in view and pushes the
// rows above it into the scrollback, xterm and conhost alike. Keeping the top
// rows dropped the cursor's row, where an agent that stopped to ask leaves its
// question, and a view of the screen lost the rows the agent was working on:
// Text, which the terminal interface shows for an agent that repaints, ended
// above them.
func TestATerminalThatLosesHeightKeepsTheCursorsRowInView(t *testing.T) {
	grid := New(40, 6)
	grid.Write([]byte("\x1b[Hline 1\r\nline 2\r\nline 3\r\nline 4\r\nline 5\r\nApply? [y/n] "))
	id, _, found := grid.UniqueRowShowing("Apply? [y/n]")
	if !found {
		t.Fatal("the question was not on the grid")
	}
	before := grid.Text()

	grid.Resize(40, 3)
	if got, want := grid.VisibleText(), "line 4\nline 5\nApply? [y/n]"; got != want {
		t.Fatalf("the grid after the shrink is %q, want %q", got, want)
	}
	if got := grid.Text(); got != before {
		t.Fatalf("the rows above the cursor did not go to the scrollback: %q, want %q", got, before)
	}
	if !grid.RowShows(id, "Apply? [y/n]") {
		t.Fatal("the question's row lost its identity in the shrink")
	}
	grid.Write([]byte("y"))
	if got, want := grid.CursorLine(), "Apply? [y/n] y"; got != want {
		t.Fatalf("the cursor left the question's row: it writes on %q, want %q", got, want)
	}
}

// With the cursor high enough, nothing has to leave the top: what does not fit
// is below the cursor, and it goes, as it does on a terminal.
func TestATerminalThatLosesHeightDropsTheRowsBelowTheCursorFirst(t *testing.T) {
	grid := New(40, 6)
	grid.Write([]byte("\x1b[Htop\r\nApply? [y/n] \x1b[5;1Hstatus line\x1b[2;14H"))

	grid.Resize(40, 3)
	if got, want := grid.Text(), "top\nApply? [y/n]"; got != want {
		t.Fatalf("the screen after the shrink is %q, want %q", got, want)
	}
}

// While a full-screen program has the grid, the parked primary screen follows
// the terminal too: it comes back with its cursor's row in view and the rows
// above it in its history. The alternate screen keeps its cursor's row in view
// as well, but has no history to push into.
func TestTheParkedPrimaryScreenKeepsItsCursorsRowInViewWhenTheTerminalLosesHeight(t *testing.T) {
	grid := New(40, 6)
	grid.Write([]byte("\x1b[Hline 1\r\nline 2\r\nline 3\r\nline 4\r\nline 5\r\nApply? [y/n] y"))
	grid.Write([]byte("\x1b[?1049h\x1b[Ha1\r\na2\r\na3\r\na4\r\na5\r\na6"))

	grid.Resize(40, 3)
	if got, want := grid.Text(), "a4\na5\na6"; got != want {
		t.Fatalf("the alternate screen after the shrink is %q, want %q", got, want)
	}
	grid.Write([]byte("\x1b[?1049l"))
	if got, want := grid.VisibleText(), "line 4\nline 5\nApply? [y/n] y"; got != want {
		t.Fatalf("the primary screen came back as %q, want %q", got, want)
	}
	if got := grid.Text(); !strings.HasPrefix(got, "line 1\nline 2\nline 3\n") {
		t.Fatalf("the rows above the cursor are not in the primary screen's history: %q", got)
	}
}
