package tui

import "github.com/charmbracelet/bubbles/viewport"

const (
	minTerminalWidth  = 30
	minTerminalHeight = 10
	maxAgentsPerPage  = 4
	maxAgentCount     = 8
)

// Rect is an outer terminal rectangle. Coordinates are zero based and Width
// and Height include the Lip Gloss border of the panel.
type Rect struct {
	X      int
	Y      int
	Width  int
	Height int
}

// Contains reports whether a terminal coordinate is inside the rectangle.
func (r Rect) Contains(x, y int) bool {
	return x >= r.X && x < r.X+r.Width && y >= r.Y && y < r.Y+r.Height
}

// Cell is the complete layout contract for one visible agent. Its viewport
// dimensions are the dimensions that must also be applied to the agent PTY.
type Cell struct {
	AgentIndex     int
	Outer          Rect
	ViewportWidth  int
	ViewportHeight int
}

// Geometry is the single source of truth for rendering, mouse hit testing and
// PTY sizing. Page is zero based; PageCount is always at least one.
type Geometry struct {
	Width                    int
	Height                   int
	AgentArea                Rect
	Supervisor               Rect
	Cells                    []Cell
	Page                     int
	PageCount                int
	SupervisorViewportWidth  int
	SupervisorViewportHeight int
	InputWidth               int
}

// CalculateLayout divides the terminal into a 75% agent area and a 25%
// supervisor area. Each page displays at most four agents: one full-width
// cell, two columns, a 2+1 layout, or a 2x2 grid.
func CalculateLayout(width, height, agentCount, page int) Geometry {
	width = maxInt(1, width)
	height = maxInt(1, height)
	agentCount = clampInt(agentCount, 1, maxAgentCount)

	pageCount := (agentCount + maxAgentsPerPage - 1) / maxAgentsPerPage
	page = clampInt(page, 0, pageCount-1)

	agentHeight, supervisorHeight := sectionHeights(height)
	result := Geometry{
		Width:      width,
		Height:     height,
		AgentArea:  Rect{Width: width, Height: agentHeight},
		Supervisor: Rect{Y: agentHeight, Width: width, Height: supervisorHeight},
		Page:       page,
		PageCount:  pageCount,
	}

	first := page * maxAgentsPerPage
	visibleCount := minInt(maxAgentsPerPage, agentCount-first)
	outerCells := pageRects(result.AgentArea, visibleCount)
	result.Cells = make([]Cell, visibleCount)
	frame := agentPanelStyle(0, false)
	for index, outer := range outerCells {
		result.Cells[index] = Cell{
			AgentIndex:     first + index,
			Outer:          outer,
			ViewportWidth:  maxInt(1, outer.Width-frame.GetHorizontalFrameSize()),
			ViewportHeight: maxInt(1, outer.Height-frame.GetVerticalFrameSize()-1),
		}
	}

	supervisorFrame := supervisorPanelStyle(false)
	result.SupervisorViewportWidth = maxInt(
		1,
		result.Supervisor.Width-supervisorFrame.GetHorizontalFrameSize(),
	)
	supervisorInnerHeight := maxInt(
		1,
		result.Supervisor.Height-supervisorFrame.GetVerticalFrameSize(),
	)
	result.SupervisorViewportHeight = maxInt(1, supervisorInnerHeight-3)
	result.InputWidth = maxInt(1, result.SupervisorViewportWidth-2)
	return result
}

// AgentViewportSize returns the viewport (and therefore PTY) size of an agent,
// including agents that are currently on a hidden page.
func AgentViewportSize(width, height, agentCount, agentIndex int) (columns, rows int) {
	if agentCount < 1 || agentIndex < 0 || agentIndex >= agentCount {
		return 1, 1
	}
	layout := CalculateLayout(width, height, agentCount, agentIndex/maxAgentsPerPage)
	for _, cell := range layout.Cells {
		if cell.AgentIndex == agentIndex {
			return cell.ViewportWidth, cell.ViewportHeight
		}
	}
	return 1, 1
}

func sectionHeights(height int) (agentHeight, supervisorHeight int) {
	if height == 1 {
		return 1, 0
	}
	if height < minTerminalHeight {
		agentHeight = maxInt(1, height/2)
		return agentHeight, height - agentHeight
	}
	supervisorHeight = maxInt(6, height/4)
	if supervisorHeight >= height {
		supervisorHeight = height - 1
	}
	return height - supervisorHeight, supervisorHeight
}

func pageRects(area Rect, count int) []Rect {
	if count <= 0 {
		return nil
	}
	if count == 1 {
		return []Rect{area}
	}

	leftWidth := area.Width / 2
	rightWidth := area.Width - leftWidth
	if count == 2 {
		return []Rect{
			{X: area.X, Y: area.Y, Width: leftWidth, Height: area.Height},
			{X: area.X + leftWidth, Y: area.Y, Width: rightWidth, Height: area.Height},
		}
	}

	topHeight := area.Height / 2
	bottomHeight := area.Height - topHeight
	result := []Rect{
		{X: area.X, Y: area.Y, Width: leftWidth, Height: topHeight},
		{X: area.X + leftWidth, Y: area.Y, Width: rightWidth, Height: topHeight},
	}
	if count == 3 {
		return append(result, Rect{
			X: area.X, Y: area.Y + topHeight, Width: area.Width, Height: bottomHeight,
		})
	}
	return append(result,
		Rect{X: area.X, Y: area.Y + topHeight, Width: leftWidth, Height: bottomHeight},
		Rect{X: area.X + leftWidth, Y: area.Y + topHeight, Width: rightWidth, Height: bottomHeight},
	)
}

func resizeViewport(target *viewport.Model, width, height int) {
	wasAtBottom := target.AtBottom()
	previousOffset := target.YOffset
	target.Width = maxInt(1, width)
	target.Height = maxInt(1, height)
	if wasAtBottom {
		target.GotoBottom()
		return
	}
	target.SetYOffset(previousOffset)
}

func (m *Model) resize(width, height int) {
	m.width = maxInt(1, width)
	m.height = maxInt(1, height)
	m.page = clampInt(m.page, 0, pageCount(len(m.panes))-1)
	m.layout = CalculateLayout(m.width, m.height, len(m.panes), m.page)

	for index := range m.panes {
		columns, rows := AgentViewportSize(m.width, m.height, len(m.panes), index)
		resizeViewport(&m.panes[index].viewport, columns, rows)
	}
	resizeViewport(
		&m.supervisor,
		m.layout.SupervisorViewportWidth,
		m.layout.SupervisorViewportHeight,
	)
	m.input.Width = m.layout.InputWidth

	for index := range m.panes {
		if err := m.backend.Resize(
			m.panes[index].sessionID,
			m.panes[index].viewport.Width,
			m.panes[index].viewport.Height,
		); err != nil && m.backend.Context().Err() == nil {
			m.appendLog("Redimensionnement de " + m.panes[index].name + " impossible: " + err.Error())
		}
	}
}

func pageCount(agentCount int) int {
	return maxInt(1, (agentCount+maxAgentsPerPage-1)/maxAgentsPerPage)
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func clampInt(value, minimum, maximum int) int {
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}
