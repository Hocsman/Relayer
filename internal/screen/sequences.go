package screen

import (
	"sort"
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
		// nextRowID travels into the saved screen and back out again. Resize
		// resizes the ALTERNATE screen too, and resizeTo mints identities from
		// whatever counter that struct holds: leaving it at zero there would
		// hand the saved primary rows names that are already in use on the live
		// grid, and a memory anchored on one of them would follow the wrong
		// line home.
		saved := &Screen{width: s.width, height: s.height, rows: s.rows,
			scrollback: s.scrollback, cursor: s.cursor, saved: s.saved,
			scrollTop: s.scrollTop, scrollBottom: s.scrollBottom, autowrap: s.autowrap,
			nextRowID: s.nextRowID}
		s.alternate = saved
		s.rows = make([]row, s.height)
		for index := range s.rows {
			s.rows[index] = s.newRow(s.width)
		}
		s.scrollback = nil
		s.cursor = cursor{}
		s.scrollTop, s.scrollBottom = 0, s.height-1
		return
	}
	restored := s.alternate
	s.alternate = nil
	if restored.nextRowID > s.nextRowID {
		s.nextRowID = restored.nextRowID
	}
	s.rows = restored.rows
	s.scrollback = restored.scrollback
	s.cursor = restored.cursor
	s.saved = restored.saved
	s.scrollTop, s.scrollBottom = restored.scrollTop, restored.scrollBottom
	s.autowrap = restored.autowrap
}

func (s *Screen) reset() {
	for index := range s.rows {
		s.rows[index].blank()
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
			s.rows[index].blank()
		}
	case 1: // start to cursor
		for index := 0; index < s.cursor.row; index++ {
			s.rows[index].blank()
		}
		s.rows[s.cursor.row].clear(0, s.cursor.column+1)
	case 2, 3: // whole display; 3 also drops scrollback
		for index := range s.rows {
			s.rows[index].blank()
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
		s.rows[s.cursor.row] = s.newRow(s.width)
	}
}

func (s *Screen) deleteLines(count int) {
	if s.cursor.row < s.scrollTop || s.cursor.row > s.scrollBottom {
		return
	}
	for range count {
		copy(s.rows[s.cursor.row:s.scrollBottom], s.rows[s.cursor.row+1:s.scrollBottom+1])
		s.rows[s.scrollBottom] = s.newRow(s.width)
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

// serialize renders a set of rows exactly once, and reports for every logical
// line where it starts in the text and which row painted its first cell.
//
// Text, TextAndBurst, Render and VisibleText all go through here on purpose.
// The offset a caller holds is an offset into THIS text: joined at the wraps,
// trimmed at the right margin, with trailing blank lines dropped. Any second
// implementation of that layout would be a second opinion about where a byte
// is, and the row a match is attributed to would drift from the row the
// operator is looking at the day the two disagree.
func serialize(rows []row) (lines []string, lineRows []RowID, firstDirty int) {
	lines = make([]string, 0, len(rows))
	lineRows = make([]RowID, 0, len(rows))
	firstDirty = -1

	var current strings.Builder
	owner := RowID(0)
	joining := false
	for _, line := range rows {
		if line.dirty && firstDirty < 0 {
			firstDirty = len(lines)
		}
		if !joining {
			owner = line.id
		}
		current.WriteString(line.text())
		if line.wrapped {
			joining = true
			continue
		}
		lines = append(lines, current.String())
		lineRows = append(lineRows, owner)
		current.Reset()
		joining = false
	}
	if joining || current.Len() > 0 {
		lines = append(lines, current.String())
		lineRows = append(lineRows, owner)
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
		lineRows = lineRows[:len(lines)]
	}
	if firstDirty > len(lines) {
		firstDirty = -1
	}
	return lines, lineRows, firstDirty
}

func (s *Screen) allRows() []row {
	rows := make([]row, 0, len(s.scrollback)+len(s.rows))
	rows = append(rows, s.scrollback...)
	rows = append(rows, s.rows...)
	return rows
}

// Anchors translates a byte offset in the text one render produced into the row
// that painted it.
//
// It is a value, rebuilt whole by each render and never mutated afterwards, so
// a caller that copies it — the Codex adapter probes by copying its whole
// detection state — shares a reading of the past, never a handle on the live
// screen.
type Anchors struct {
	lineStart []int
	lineRow   []RowID
	textLen   int
}

// RowAt names the row that painted the byte at offset. It reports false for an
// offset outside the text the anchors were built from, which is the only honest
// answer: a coordinate invented for an unknown offset would be a coordinate
// pointing at the wrong question.
func (a Anchors) RowAt(offset int) (RowID, bool) {
	if offset < 0 || offset >= a.textLen || len(a.lineStart) == 0 {
		return 0, false
	}
	index := sort.Search(len(a.lineStart), func(i int) bool { return a.lineStart[i] > offset }) - 1
	if index < 0 || index >= len(a.lineRow) {
		return 0, false
	}
	return a.lineRow[index], true
}

func newAnchors(lines []string, lineRows []RowID) Anchors {
	starts := make([]int, len(lines))
	offset := 0
	for index, line := range lines {
		starts[index] = offset
		offset += len(line) + 1
	}
	// offset has counted one separator too many: strings.Join puts one between
	// lines, not after the last. An offset at or past the end of the text is
	// refused rather than attributed to the last line, because a coordinate
	// invented for an unknown offset is a coordinate pointing at the wrong
	// question.
	length := offset - 1
	if length < 0 {
		length = 0
	}
	return Anchors{lineStart: starts, lineRow: lineRows, textLen: length}
}

// Text renders the screen as the operator sees it: scrollback first, then the
// live grid, with rows joined where they wrapped so a sentence broken by the
// right margin is one line again. Trailing blank rows are dropped, because a
// mostly empty screen is not the same as a screen full of blank lines.
func (s *Screen) Text() string {
	lines, _, _ := serialize(s.allRows())
	return strings.Join(lines, "\n")
}

// TextAndBurst renders the screen and reports where in that text the rows this
// write touched begin.
//
// On a byte stream "what the agent just wrote" is a range of offsets. On a grid
// it is a set of rows, and a repaint touches them out of order — a frame drawn
// top to bottom then filled in the middle changes row 1, 3 and 2 in that order.
// The offset returned is that of the EARLIEST touched row, so the region is
// contiguous and conservative: it can include a row the write did not touch,
// never exclude one it did. Excluding is the unsafe direction, because a
// question in an excluded row is a question nobody is shown.
//
// A burst offset of len(text) means this write changed nothing that survives on
// screen.
func (s *Screen) TextAndBurst() (text string, burstStart int) {
	text, burstStart, _ = s.Render()
	return text, burstStart
}

// Render is TextAndBurst plus the map from that text back to the rows.
//
// Detection finds a question at a byte offset. Where that question IS on the
// screen is knowledge this render already has and used to throw away, leaving
// the caller to search the grid for the matched text later — which finds the
// wrong row as soon as two rows carry the same fragment, and "[y/n]" is
// everywhere.
func (s *Screen) Render() (text string, burstStart int, anchors Anchors) {
	lines, lineRows, firstDirty := serialize(s.allRows())
	text = strings.Join(lines, "\n")
	anchors = newAnchors(lines, lineRows)
	if firstDirty < 0 || firstDirty >= len(lines) {
		return text, len(text), anchors
	}
	for index := range firstDirty {
		burstStart += len(lines[index]) + 1
	}
	if burstStart > len(text) {
		burstStart = len(text)
	}
	return text, burstStart, anchors
}

// VisibleText renders only the live grid, without scrollback.
//
// Detection reads the scrollback too, because a question can legitimately have
// scrolled just above the fold. But "is this question still on screen?" must be
// answered by the screen alone: history keeps a question findable long after the
// agent stopped showing it, and a memory released by text-matching would then
// never be released at all.
func (s *Screen) VisibleText() string {
	lines, _, _ := serialize(s.rows)
	return strings.Join(lines, "\n")
}

// RowShows reports whether the named row is still on the visible grid and still
// carries text.
//
// The pair is the point. The row alone would answer yes to a line the agent
// rewrote with something else; the text alone would answer yes to the same
// words on another line. A row that scrolled away, that was erased, or that now
// says something different answers false — which is how a caller learns that
// what it remembered about that question no longer holds.
func (s *Screen) RowShows(id RowID, text string) bool {
	if id == 0 || text == "" {
		return false
	}
	for index := range s.rows {
		if s.rows[index].id != id {
			continue
		}
		return strings.Contains(s.linesFrom(index, strings.Count(text, "\n")+1), text)
	}
	return false
}

// linesFrom joins logical lines starting at a visible row, the way a render
// joins them.
//
// A match is not always one line. The vendor rules are (?is) regexes that run
// across a whole prompt block — Claude's folder-trust prompt spans five — and
// such a match is compared against the text a render produced, where those
// lines are separated by newlines. Comparing it against a single logical line
// could only ever answer false, which would release the memory of a question
// plainly still on screen and put it to the operator a second time, the second
// keystroke reaching an agent that has already moved on.
func (s *Screen) linesFrom(index, count int) string {
	if index < 0 || index >= len(s.rows) || count < 1 {
		return ""
	}
	lines := make([]string, 0, count)
	for row := index; row < len(s.rows) && len(lines) < count; row++ {
		lines = append(lines, logicalLineAt(s.rows, row))
		for row < len(s.rows) && s.rows[row].wrapped {
			row++
		}
	}
	return strings.Join(lines, "\n")
}

// UniqueRowShowing names the only visible row whose logical line contains text,
// and reports false when none does or when more than one does.
//
// It is the last resort for a caller holding no coordinate at all — an
// occurrence rebuilt from a snapshot, which crossed a process boundary where a
// screen coordinate has no meaning. Ambiguity is refused rather than guessed:
// picking one of two identical lines is how a memory latches onto the wrong
// question in the first place.
func (s *Screen) UniqueRowShowing(text string) (RowID, string, bool) {
	if text == "" {
		return 0, "", false
	}
	span := strings.Count(text, "\n") + 1
	found := RowID(0)
	line := ""
	for index := range s.rows {
		if index > 0 && s.rows[index-1].wrapped {
			continue
		}
		if !strings.Contains(s.linesFrom(index, span), text) {
			continue
		}
		if found != 0 {
			return 0, "", false
		}
		found = s.rows[index].id
		line = logicalLineAt(s.rows, index)
	}
	return found, line, found != 0
}

// logicalLineAt joins the row at index with the wrapped continuation below it,
// so a question broken by the right margin is read as one line.
func logicalLineAt(rows []row, index int) string {
	start := index
	for start > 0 && rows[start-1].wrapped {
		start--
	}
	var joined strings.Builder
	for row := start; row < len(rows); row++ {
		joined.WriteString(rows[row].text())
		if !rows[row].wrapped {
			break
		}
	}
	return joined.String()
}

// ClearDirty forgets which rows the last write touched, so the next one starts
// its own burst.
func (s *Screen) ClearDirty() {
	for index := range s.rows {
		s.rows[index].dirty = false
	}
	for index := range s.scrollback {
		s.scrollback[index].dirty = false
	}
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
