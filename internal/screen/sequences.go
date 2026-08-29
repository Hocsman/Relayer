package screen

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// The sequences a screen must act on to keep detection honest, and nothing
// more. Colour, fonts and window titles change what a screen looks like, never
// what it says, so they are recognised and discarded.
//
// The dividing line is not "common" versus "rare": it is whether ignoring the
// sequence would leave the grid describing something the operator is not
// looking at. An erase that does not erase is exactly that.

func (s *Screen) param(params ansi.Params, index, fallback int) int {
	value, _, ok := params.Param(index, fallback)
	if !ok || value == 0 {
		return fallback
	}
	return value
}

func (s *Screen) csi(cmd ansi.Cmd, params ansi.Params) {
	// A private-marker sequence ('?') is a mode set or reset; the command byte
	// alone would collide with the public ones.
	if marker := cmd.Prefix(); marker == '?' {
		s.privateMode(cmd.Final(), params)
		return
	}
	switch cmd.Final() {
	case 'A': // CUU
		s.repainted = true
		s.moveCursor(-s.param(params, 0, 1), 0)
	case 'B': // CUD
		s.repainted = true
		s.moveCursor(s.param(params, 0, 1), 0)
	case 'C': // CUF
		s.moveCursor(0, s.param(params, 0, 1))
	case 'D': // CUB
		s.moveCursor(0, -s.param(params, 0, 1))
	case 'E': // CNL
		s.cursor.column = 0
		s.moveCursor(s.param(params, 0, 1), 0)
	case 'F': // CPL
		s.cursor.column = 0
		s.moveCursor(-s.param(params, 0, 1), 0)
	case 'G', '`': // CHA, HPA
		s.setCursor(s.cursor.row, s.param(params, 0, 1)-1)
	case 'd': // VPA
		s.repainted = true
		s.setCursor(s.param(params, 0, 1)-1, s.cursor.column)
	case 'H', 'f': // CUP, HVP
		s.repainted = true
		s.setCursor(s.param(params, 0, 1)-1, s.param(params, 1, 1)-1)
	case 'J': // ED
		s.repainted = true
		s.eraseDisplay(s.param(params, 0, 0))
	case 'K': // EL
		s.repainted = true
		s.eraseLine(s.param(params, 0, 0))
	case 'L': // IL
		s.repainted = true
		s.insertLines(s.param(params, 0, 1))
	case 'M': // DL
		s.repainted = true
		s.deleteLines(s.param(params, 0, 1))
	case 'P': // DCH
		s.deleteCharacters(s.param(params, 0, 1))
	case '@': // ICH
		s.insertCharacters(s.param(params, 0, 1))
	case 'X': // ECH
		s.eraseCharacters(s.param(params, 0, 1))
	case 'S': // SU
		s.repainted = true
		s.scrollUp(s.param(params, 0, 1))
	case 'T': // SD
		s.repainted = true
		s.scrollDown(s.param(params, 0, 1))
	case 'r': // DECSTBM
		s.repainted = true
		s.setScrollRegion(s.param(params, 0, 1)-1, s.param(params, 1, s.height)-1)
	case 's':
		s.saved = s.cursor
	case 'u':
		s.restoreCursor()
	}
	// Everything else — SGR, device reports, mode queries — is recognised by
	// the parser and deliberately does nothing here.
}

func (s *Screen) esc(cmd ansi.Cmd) {
	switch cmd.Final() {
	case '7':
		s.saved = s.cursor
	case '8':
		s.restoreCursor()
	case 'D': // IND
		s.lineFeed()
	case 'M': // RI
		if s.cursor.row == s.scrollTop {
			s.scrollDown(1)
		} else if s.cursor.row > 0 {
			s.cursor.row--
		}
	case 'E': // NEL
		s.cursor.column = 0
		s.lineFeed()
	case 'c': // RIS
		s.reset()
	}
}

func (s *Screen) privateMode(command byte, params ansi.Params) {
	mode, _, _ := params.Param(0, 0)
	set := command == 'h'
	if command != 'h' && command != 'l' {
		return
	}
	switch mode {
	case 7: // DECAWM
		s.autowrap = set
	case 1047, 1049: // alternate screen
		s.repainted = true
		s.switchScreen(set)
	}
}

// switchScreen enters or leaves the alternate screen.
//
// A full-screen agent paints its interface there and restores the primary one
// when it exits. Modelling it matters for detection specifically because the
// primary screen must come back UNCHANGED: a question that was on it before the
// agent took over is still the question, and one painted on the alternate
// screen must vanish with it rather than linger as history.
func (s *Screen) switchScreen(toAlternate bool) {
	if toAlternate == (s.alternate != nil) {
		return
	}
	if toAlternate {
		saved := &Screen{width: s.width, height: s.height, rows: s.rows,
			scrollback: s.scrollback, cursor: s.cursor, saved: s.saved,
			scrollTop: s.scrollTop, scrollBottom: s.scrollBottom, autowrap: s.autowrap}
		s.alternate = saved
		s.rows = make([]row, s.height)
		for index := range s.rows {
			s.rows[index] = newRow(s.width)
		}
		s.scrollback = nil
		s.cursor = cursor{}
		s.scrollTop, s.scrollBottom = 0, s.height-1
		return
	}
	restored := s.alternate
	s.alternate = nil
	s.rows = restored.rows
	s.scrollback = restored.scrollback
	s.cursor = restored.cursor
	s.saved = restored.saved
	s.scrollTop, s.scrollBottom = restored.scrollTop, restored.scrollBottom
	s.autowrap = restored.autowrap
}

func (s *Screen) reset() {
	for index := range s.rows {
		s.rows[index] = newRow(s.width)
	}
	s.cursor = cursor{}
	s.saved = cursor{}
	s.scrollTop, s.scrollBottom = 0, s.height-1
	s.autowrap = true
}

func (s *Screen) restoreCursor() {
	s.cursor = s.saved
	s.cursor.row = clamp(s.cursor.row, 0, s.height-1)
	s.cursor.column = clamp(s.cursor.column, 0, s.width-1)
}

func (s *Screen) moveCursor(rows, columns int) {
	s.setCursor(s.cursor.row+rows, s.cursor.column+columns)
}

func (s *Screen) setCursor(row, column int) {
	s.cursor.row = clamp(row, 0, s.height-1)
	s.cursor.column = clamp(column, 0, s.width-1)
	s.cursor.pendingWrap = false
}

func (s *Screen) setScrollRegion(top, bottom int) {
	top = clamp(top, 0, s.height-1)
	bottom = clamp(bottom, 0, s.height-1)
	if top >= bottom {
		top, bottom = 0, s.height-1
	}
	s.scrollTop, s.scrollBottom = top, bottom
	s.setCursor(top, 0)
}

func (s *Screen) eraseDisplay(mode int) {
	switch mode {
	case 0: // cursor to end
		s.rows[s.cursor.row].clear(s.cursor.column, s.width)
		for index := s.cursor.row + 1; index < s.height; index++ {
			s.rows[index] = newRow(s.width)
		}
	case 1: // start to cursor
		for index := 0; index < s.cursor.row; index++ {
			s.rows[index] = newRow(s.width)
		}
		s.rows[s.cursor.row].clear(0, s.cursor.column+1)
	case 2, 3: // whole display; 3 also drops scrollback
		for index := range s.rows {
			s.rows[index] = newRow(s.width)
		}
		if mode == 3 {
			s.scrollback = nil
		}
	}
}

func (s *Screen) eraseLine(mode int) {
	line := &s.rows[s.cursor.row]
	switch mode {
	case 0:
		line.clear(s.cursor.column, s.width)
	case 1:
		line.clear(0, s.cursor.column+1)
	case 2:
		line.clear(0, s.width)
	}
	line.wrapped = false
}

func (s *Screen) eraseCharacters(count int) {
	s.rows[s.cursor.row].clear(s.cursor.column, s.cursor.column+count)
}

func (s *Screen) insertLines(count int) {
	if s.cursor.row < s.scrollTop || s.cursor.row > s.scrollBottom {
		return
	}
	for range count {
		copy(s.rows[s.cursor.row+1:s.scrollBottom+1], s.rows[s.cursor.row:s.scrollBottom])
		s.rows[s.cursor.row] = newRow(s.width)
	}
}

func (s *Screen) deleteLines(count int) {
	if s.cursor.row < s.scrollTop || s.cursor.row > s.scrollBottom {
		return
	}
	for range count {
		copy(s.rows[s.cursor.row:s.scrollBottom], s.rows[s.cursor.row+1:s.scrollBottom+1])
		s.rows[s.scrollBottom] = newRow(s.width)
	}
}

func (s *Screen) deleteCharacters(count int) {
	line := &s.rows[s.cursor.row]
	from := s.cursor.column
	copy(line.cells[from:], line.cells[clamp(from+count, from, s.width):])
	line.clear(max(s.width-count, from), s.width)
}

func (s *Screen) insertCharacters(count int) {
	line := &s.rows[s.cursor.row]
	from := s.cursor.column
	if from+count < s.width {
		copy(line.cells[from+count:], line.cells[from:s.width-count])
	}
	line.clear(from, clamp(from+count, from, s.width))
}

// Text renders the screen as the operator sees it: scrollback first, then the
// live grid, with rows joined where they wrapped so a sentence broken by the
// right margin is one line again. Trailing blank rows are dropped, because a
// mostly empty screen is not the same as a screen full of blank lines.
func (s *Screen) Text() string {
	rows := make([]row, 0, len(s.scrollback)+len(s.rows))
	rows = append(rows, s.scrollback...)
	rows = append(rows, s.rows...)

	lines := make([]string, 0, len(rows))
	var current strings.Builder
	joining := false
	for _, line := range rows {
		current.WriteString(line.text())
		if line.wrapped {
			joining = true
			continue
		}
		lines = append(lines, current.String())
		current.Reset()
		joining = false
	}
	if joining || current.Len() > 0 {
		lines = append(lines, current.String())
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

// CursorLine reports the logical line the cursor sits on, which is where an
// agent that has stopped to ask leaves its question.
func (s *Screen) CursorLine() string {
	start := s.cursor.row
	for start > 0 && s.rows[start-1].wrapped {
		start--
	}
	var builder strings.Builder
	for index := start; index < s.height; index++ {
		builder.WriteString(s.rows[index].text())
		if !s.rows[index].wrapped {
			break
		}
	}
	return builder.String()
}
