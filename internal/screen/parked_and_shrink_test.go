package screen

import (
	"fmt"
	"math/rand"
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

// A terminal that lost height while a full-screen program had the grid gives
// the primary screen back at the new height, resized when the program exits:
// with its cursor's row in view and the rows above it in its history. The
// alternate screen keeps its cursor's row in view as well, but has no history
// to push into.
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

// While a full-screen program has the grid, only its screen follows the
// terminal. The primary one is resized once, to the size in force, when the
// program exits, which is what ConPTY does: a window dragged smaller and back
// while an editor was open gives the primary screen back exactly as it was, and
// ConPTY repaints it that way. Following every step of the drag, the parked
// screen pushed its top rows into the history on the shrink and did not pull
// them back on the grow, so the rows came back higher than ConPTY drew them:
// the answered question was repainted on a row the memory did not know, and
// was asked again.
func TestThePrimaryScreenIsResizedOnceWhenTheProgramExits(t *testing.T) {
	const primary = "line 1\nline 2\nline 3\nline 4\nline 5\nApply? [y/n] y"
	for _, testCase := range []struct {
		name  string
		sizes []int
	}{
		{name: "shrink then grow back", sizes: []int{3, 6}},
		{name: "a window dragged smaller and back", sizes: []int{5, 4, 2, 3, 6}},
		// ConPTY grows its primary buffer at the bottom: the same rows, and
		// blank ones below them.
		{name: "shrink, then grow past the old height", sizes: []int{3, 9}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			grid := New(40, 6)
			grid.Write([]byte("\x1b[Hline 1\r\nline 2\r\nline 3\r\nline 4\r\nline 5\r\nApply? [y/n] y"))
			id, _, found := grid.UniqueRowShowing("Apply? [y/n] y")
			if !found {
				t.Fatal("the question was not on the grid")
			}
			grid.Write([]byte("\x1b[?1049h\x1b[Heditor contents\r\n~\r\n~"))
			for _, height := range testCase.sizes {
				grid.Resize(40, height)
			}
			grid.Write([]byte("\x1b[?1049l"))
			if got := grid.VisibleText(); got != primary {
				t.Fatalf("the primary screen came back as %q, want %q", got, primary)
			}
			if row, _, _ := grid.VisibleRowOf("Apply? [y/n] y"); row != 5 {
				t.Fatalf("the question came back on row %d, want 5", row)
			}
			if !grid.RowShows(id, "Apply? [y/n] y") {
				t.Fatal("the question's row lost its identity")
			}
			grid.Write([]byte("es"))
			if got, want := grid.CursorLine(), "Apply? [y/n] yes"; got != want {
				t.Fatalf("the cursor came back on %q, want %q", got, want)
			}
		})
	}
}

// checkRowIdentities asserts what the answered memory relies on: every row
// identity names one line, across the live grid, its history, the parked
// primary screen and that screen's history. Along with it, the shapes a resize
// could break: every grid has its height in rows and its width in cells, every
// cursor is on its grid, and every history is bounded.
func checkRowIdentities(t *testing.T, grid *Screen, step string) {
	t.Helper()
	seen := map[RowID]string{}
	add := func(where string, rows []row) {
		for index, line := range rows {
			if line.id == 0 {
				t.Fatalf("%s: %s row %d has no identity", step, where, index)
			}
			if previous, taken := seen[line.id]; taken {
				t.Fatalf("%s: row identity %d names %s and %s row %d", step, line.id, previous, where, index)
			}
			seen[line.id] = fmt.Sprintf("%s row %d", where, index)
		}
	}
	shape := func(where string, screen *Screen) {
		if len(screen.rows) != screen.height {
			t.Fatalf("%s: %s has %d rows for a height of %d", step, where, len(screen.rows), screen.height)
		}
		for index, line := range screen.rows {
			if len(line.cells) != screen.width {
				t.Fatalf("%s: %s row %d has %d cells for a width of %d", step, where, index, len(line.cells), screen.width)
			}
		}
		if screen.cursor.row < 0 || screen.cursor.row >= screen.height || screen.cursor.column < 0 || screen.cursor.column >= screen.width {
			t.Fatalf("%s: %s cursor %d,%d is off its %dx%d grid", step, where, screen.cursor.row, screen.cursor.column, screen.width, screen.height)
		}
		if len(screen.scrollback) > MaxScrollback {
			t.Fatalf("%s: %s history holds %d rows", step, where, len(screen.scrollback))
		}
	}
	add("the live grid", grid.rows)
	add("the live history", grid.scrollback)
	shape("the live grid", grid)
	if grid.alternate != nil {
		add("the parked grid", grid.alternate.rows)
		add("the parked history", grid.alternate.scrollback)
		shape("the parked grid", grid.alternate)
	}
}

// A pane that more than doubles its height while a full-screen program has it:
// four agents to one in the desktop grid, a small pane maximised. The parked
// primary screen was resized with it and minted its new rows from the counter it
// was parked with, which the program's screen had been using since: a row of
// the program's grid and a parked row had one name. RowParked then answered yes
// for a live row, and an answer given on it was kept for the rest of the
// program, so the same dialog asked again there was put to nobody.
func TestRowIdentitiesStayUniqueWhenTheTerminalGrowsWhileAProgramHasTheScreen(t *testing.T) {
	for _, heights := range [][2]int{{10, 15}, {10, 21}, {10, 30}, {30, 70}} {
		t.Run(fmt.Sprintf("%d rows to %d", heights[0], heights[1]), func(t *testing.T) {
			grid := New(40, heights[0])
			grid.Write([]byte("\x1b[Hprimary\r\nApply? [y/n] y\r\n"))
			grid.Write([]byte("\x1b[?1049h\x1b[Hagent header"))
			checkRowIdentities(t, grid, "on the alternate screen")

			grid.Resize(40, heights[1])
			checkRowIdentities(t, grid, "after the grow")
			for index := range grid.rows {
				if grid.RowParked(grid.rows[index].id) {
					t.Fatalf("row %d of the program's grid is reported parked", index)
				}
			}

			grid.Write([]byte("\x1b[?1049l"))
			checkRowIdentities(t, grid, "back on the primary screen")
		})
	}
}

// Writes, screen switches and resizes at random, with the invariants checked
// after every step. This is how the shared name above was found: the first
// seed hits it within a hundred steps.
func TestRandomSessionsKeepEveryRowIdentityUnique(t *testing.T) {
	pieces := []string{
		"Apply? [y/n] ", "y\r\n", "output line\r\n", "\r\n", "\x1b[H\x1b[2J", "\x1b[3J",
		"\x1b[5;1H", "\x1b[1A\x1b[2K", "\x1b[?1049h", "\x1b[?1049l", "\x1b[?1047h", "\x1b[?1047l",
		strings.Repeat("wrapped ", 12), "\x1b7", "\x1b8", "\x1b[2;4r", "\x1b[r", "\x1bM", "\x1b[3L", "\x1b[2M",
		"\x1bc", "\x1b[S", "\x1b[T",
	}
	seeds := int64(300)
	if testing.Short() {
		seeds = 30
	}
	for seed := int64(1); seed <= seeds; seed++ {
		random := rand.New(rand.NewSource(seed))
		grid := New(20+random.Intn(40), 1+random.Intn(40))
		for step := range 400 {
			if random.Intn(6) == 0 {
				grid.Resize(2+random.Intn(60), 1+random.Intn(80))
				checkRowIdentities(t, grid, fmt.Sprintf("seed %d, step %d, a resize", seed, step))
				continue
			}
			piece := pieces[random.Intn(len(pieces))]
			grid.Write([]byte(piece))
			checkRowIdentities(t, grid, fmt.Sprintf("seed %d, step %d, the write %q", seed, step, piece))
		}
	}
}
