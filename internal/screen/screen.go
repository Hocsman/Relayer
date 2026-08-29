// Package screen renders a terminal byte stream into the grid of cells a
// person would actually see.
//
// Relayer's detection has always normalized a byte stream: escape sequences are
// stripped and the surviving bytes kept in write order. That is exact for an
// agent that only appends, and wrong for one that repaints. The cursor
// movements that say WHERE each fragment lands are discarded, and the erases
// that say what is no longer on screen are discarded with them — so a question
// the agent has already withdrawn is still matchable, and a question painted
// into a frame is concatenated in write order instead of landing inside it.
//
// The parser is deliberately TOTAL: every CSI, OSC, DCS, SOS, PM and APC
// sequence is recognised and consumed, even the ones the screen does nothing
// with. Acting on a small set is safe; failing to RECOGNISE a sequence is not,
// because its bytes would then be printed as text — and an unrecognised erase
// leaves stale cells live, which is the exact failure this package exists to
// remove. The recognition comes from github.com/charmbracelet/x/ansi, already
// in the module graph by way of bubbletea, so the riskiest part is not
// hand-written here.
package screen

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
)

// Bounds on what one screen may hold, so a hostile or broken agent cannot make
// Relayer allocate without limit.
const (
	MinWidth        = 2
	MinHeight       = 1
	MaxWidth        = 1000
	MaxHeight       = 500
	MaxScrollback   = 512
	defaultWidth    = 120
	defaultHeight   = 40
	maxTabStop      = 8
	maxParsedParams = 32
)

type cell struct {
	value rune
	// width 0 marks the continuation column of a double-width rune, so the
	// renderer can skip it instead of emitting a second copy.
	width int8
}

type row struct {
	cells []cell
	// wrapped marks a row whose text continues on the next one because it ran
	// past the right margin rather than because the agent ended a line. Joining
	// on it is what makes a question broken by the margin one logical line.
	wrapped bool
	// dirty marks a row this write touched. On a byte stream "what the agent
	// just wrote" is a range of offsets; on a grid it is a set of rows, and a
	// repaint touches them out of order. Detection needs to know which ones so
	// it can judge the current screen rather than the whole history.
	dirty bool
}

// newRow is a blank row, and a blank row is a change: it replaces whatever was
// there, which is exactly what an erase or a scroll does.
func newRow(width int) row {
	cells := make([]cell, width)
	for index := range cells {
		cells[index] = cell{value: ' ', width: 1}
	}
	return row{cells: cells, dirty: true}
}

func (r *row) clear(from, to int) {
	if to > from {
		r.dirty = true
	}
	for index := from; index < to && index < len(r.cells); index++ {
		r.cells[index] = cell{value: ' ', width: 1}
	}
}

// text renders one row. A row that wraps keeps its trailing spaces, because the
// text continues on the next row and the space at the margin is part of the
// sentence: trimming it joined "continue" to "with".
func (r row) text() string {
	var builder strings.Builder
	for _, current := range r.cells {
		if current.width == 0 {
			continue
		}
		builder.WriteRune(current.value)
	}
	if r.wrapped {
		return builder.String()
	}
	return strings.TrimRight(builder.String(), " ")
}

type cursor struct {
	row, column int
	// pendingWrap defers the wrap until the next printable rune, which is what
	// a real terminal does: writing the last column does not by itself move the
	// cursor to the next line.
	pendingWrap bool
}

// Screen is one terminal's visible grid plus a bounded scrollback. It is not
// safe for concurrent use; the caller owns the lock, as Processor already does.
type Screen struct {
	width, height int
	rows          []row
	scrollback    []row
	cursor        cursor
	saved         cursor
	scrollTop     int
	scrollBottom  int
	autowrap      bool

	// alternate holds the primary screen while the agent is on the alternate
	// one. A full-screen TUI switches to it and switches back, and the primary
	// contents must survive that untouched.
	alternate *Screen

	// scrolledOff counts the rows that have left the top of the grid. Added to
	// a row's index it gives an absolute coordinate that stays valid while the
	// screen moves underneath it, which is what lets a caller ask "is that same
	// row still showing that same thing" rather than "does this text appear
	// somewhere" — two identical questions in two different rows are two
	// questions.
	scrolledOff uint64

	// repainted records that the agent has done something a byte stream cannot
	// express: addressed the cursor to a row, erased, scrolled a region, or
	// switched screens. Everything else — printable text, carriage returns,
	// line feeds, cursor movement within a line — an appended byte stream
	// already renders correctly, so a caller can keep its existing behaviour
	// for those agents and change only for the ones that need it.
	repainted bool

	parser  *ansi.Parser
	handler ansi.Handler
}

// New creates a screen. A width or height outside the supported range is
// clamped rather than rejected: a caller that has not measured its terminal yet
// still needs somewhere to put output.
func New(width, height int) *Screen {
	s := &Screen{}
	s.resizeTo(width, height, false)
	s.autowrap = true
	s.parser = ansi.NewParser()
	s.parser.SetParamsSize(maxParsedParams)
	s.handler = ansi.Handler{
		Print:     s.print,
		Execute:   s.execute,
		HandleCsi: s.csi,
		HandleEsc: s.esc,
		// Recognised so their payload is never printed as text. A screen has
		// nothing to do with a window title or a device control string.
		HandleOsc: func(int, []byte) {},
		HandleDcs: func(ansi.Cmd, ansi.Params, []byte) {},
		HandlePm:  func([]byte) {},
		HandleApc: func([]byte) {},
		HandleSos: func([]byte) {},
	}
	s.parser.SetHandler(s.handler)
	return s
}

func clamp(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func (s *Screen) resizeTo(width, height int, keep bool) {
	// The default has to be chosen before the clamp, not after: clamping zero
	// yields the minimum, so a caller that has not measured its terminal would
	// get a 2x1 grid rather than something a frame can be painted into.
	if width <= 0 {
		width = defaultWidth
	}
	if height <= 0 {
		height = defaultHeight
	}
	width = clamp(width, MinWidth, MaxWidth)
	height = clamp(height, MinHeight, MaxHeight)
	previous := s.rows
	s.width, s.height = width, height
	s.rows = make([]row, height)
	for index := range s.rows {
		s.rows[index] = newRow(width)
	}
	if keep {
		// A resize cannot be aligned with a byte offset in the stream, so there
		// is no correct reflow: the agent will repaint, and until it does the
		// old rows are the best available. Copy what fits and let the repaint
		// replace it.
		for index := 0; index < len(previous) && index < height; index++ {
			copy(s.rows[index].cells, previous[index].cells)
			s.rows[index].wrapped = previous[index].wrapped
		}
	}
	s.scrollTop, s.scrollBottom = 0, height-1
	s.cursor.row = clamp(s.cursor.row, 0, height-1)
	s.cursor.column = clamp(s.cursor.column, 0, width-1)
	s.cursor.pendingWrap = false
}

// Resize adapts the grid to a new terminal size, keeping what fits.
//
// A resize to the size already in use does nothing. It has to: rebuilding the
// rows marks every one of them as touched, so a caller that resizes on each
// render — which is what a terminal interface does — would report the whole
// screen as new work on every write, and the actionable region would never
// narrow.
func (s *Screen) Resize(width, height int) {
	if width <= 0 {
		width = defaultWidth
	}
	if height <= 0 {
		height = defaultHeight
	}
	if clamp(width, MinWidth, MaxWidth) == s.width && clamp(height, MinHeight, MaxHeight) == s.height {
		return
	}
	if s.alternate != nil {
		s.alternate.resizeTo(width, height, true)
	}
	s.resizeTo(width, height, true)
}

// Size reports the current grid dimensions.
func (s *Screen) Size() (width, height int) { return s.width, s.height }

// Repainted reports whether the agent has ever done something an appended byte
// stream cannot represent. It is false for an agent that only prints and
// advances, which is what makes the rendered screen safe to adopt selectively.
func (s *Screen) Repainted() bool { return s.repainted }

// Write feeds raw terminal bytes, escape sequences included.
func (s *Screen) Write(data []byte) (int, error) {
	s.parser.Parse(data)
	return len(data), nil
}

func (s *Screen) print(r rune) {
	width := runewidth.RuneWidth(r)
	if width <= 0 {
		// Combining marks and zero-width characters are dropped rather than
		// merged: detection matches on text, and a mark that lands on nothing
		// would otherwise occupy a cell it does not own.
		return
	}
	if s.cursor.pendingWrap && s.autowrap {
		s.rows[s.cursor.row].wrapped = true
		s.cursor.column = 0
		s.lineFeed()
		s.cursor.pendingWrap = false
	}
	if s.cursor.column+width > s.width {
		if !s.autowrap {
			return
		}
		s.rows[s.cursor.row].wrapped = true
		s.cursor.column = 0
		s.lineFeed()
	}
	line := &s.rows[s.cursor.row]
	line.dirty = true
	line.cells[s.cursor.column] = cell{value: r, width: int8(width)}
	for offset := 1; offset < width && s.cursor.column+offset < s.width; offset++ {
		line.cells[s.cursor.column+offset] = cell{value: ' ', width: 0}
	}
	s.cursor.column += width
	if s.cursor.column >= s.width {
		s.cursor.column = s.width - 1
		s.cursor.pendingWrap = true
	}
}

func (s *Screen) execute(b byte) {
	switch b {
	case '\r':
		s.cursor.column = 0
		s.cursor.pendingWrap = false
	case '\n', '\v', '\f':
		// A line feed returns to column 0 as well as moving down.
		//
		// Strict VT keeps the column, and a terminal driver supplies the
		// carriage return through ONLCR — so a PTY master, which is what
		// Relayer reads, already carries "\r\n" and the extra return is a
		// no-op. The other two inputs do not: a tmux `capture-pane` result and
		// the recorded fixtures both use a bare "\n" for a line that plainly
		// starts at the left margin. Keeping the column there would render
		// every one of them as a staircase, so this deviates deliberately.
		s.cursor.column = 0
		s.lineFeed()
		s.cursor.pendingWrap = false
	case '\b':
		if s.cursor.pendingWrap {
			s.cursor.pendingWrap = false
		} else if s.cursor.column > 0 {
			s.cursor.column--
		}
	case '\t':
		next := ((s.cursor.column / maxTabStop) + 1) * maxTabStop
		s.cursor.column = clamp(next, 0, s.width-1)
		s.cursor.pendingWrap = false
	}
}

func (s *Screen) lineFeed() {
	if s.cursor.row == s.scrollBottom {
		s.scrollUp(1)
		return
	}
	if s.cursor.row < s.height-1 {
		s.cursor.row++
	}
}

func (s *Screen) scrollUp(count int) {
	for range count {
		evicted := s.rows[s.scrollTop]
		// Only the main screen keeps history. What scrolls off an alternate
		// screen is chrome the agent is repainting, not work that happened.
		if s.alternate == nil && s.scrollTop == 0 {
			s.scrolledOff++
			s.scrollback = append(s.scrollback, evicted)
			if len(s.scrollback) > MaxScrollback {
				s.scrollback = s.scrollback[len(s.scrollback)-MaxScrollback:]
			}
		}
		copy(s.rows[s.scrollTop:s.scrollBottom], s.rows[s.scrollTop+1:s.scrollBottom+1])
		s.rows[s.scrollBottom] = newRow(s.width)
	}
}

func (s *Screen) scrollDown(count int) {
	for range count {
		copy(s.rows[s.scrollTop+1:s.scrollBottom+1], s.rows[s.scrollTop:s.scrollBottom])
		s.rows[s.scrollTop] = newRow(s.width)
	}
}
