package tui

import "github.com/charmbracelet/bubbles/viewport"

const (
	minTerminalWidth  = 30
	minTerminalHeight = 10
)

// Geometry is the single layout contract shared by the command bootstrap and
// the Bubble Tea model. The agent viewport dimensions are also the PTY sizes.
type Geometry struct {
	Width                    int
	Height                   int
	LeftWidth                int
	RightWidth               int
	TopHeight                int
	SupervisorHeight         int
	AgentViewportWidths      [2]int
	AgentViewportHeight      int
	SupervisorViewportWidth  int
	SupervisorViewportHeight int
	InputWidth               int
}

// CalculateLayout computes the complete three-panel geometry. Lip Gloss
// Width/Height describe the content box, so borders and the agent title line
// are removed explicitly.
func CalculateLayout(width, height int) Geometry {
	result := Geometry{
		Width:  maxInt(1, width),
		Height: maxInt(1, height),
	}
	result.LeftWidth = maxInt(1, result.Width/2)
	result.RightWidth = maxInt(1, result.Width-result.LeftWidth)

	if result.Height >= minTerminalHeight {
		// Reserve one quarter for supervision and leave roughly 75% of the
		// terminal to the two live agent streams.
		result.SupervisorHeight = maxInt(6, result.Height/4)
		result.TopHeight = result.Height - result.SupervisorHeight
		if result.TopHeight < 4 {
			result.TopHeight = 4
			result.SupervisorHeight = result.Height - result.TopHeight
		}
	} else {
		result.TopHeight = maxInt(1, result.Height/2)
		result.SupervisorHeight = maxInt(1, result.Height-result.TopHeight)
	}

	agentFrame := agentABorderStyle
	result.AgentViewportWidths[0] = maxInt(
		1,
		result.LeftWidth-agentFrame.GetHorizontalFrameSize(),
	)
	result.AgentViewportWidths[1] = maxInt(
		1,
		result.RightWidth-agentFrame.GetHorizontalFrameSize(),
	)
	agentInnerHeight := maxInt(1, result.TopHeight-agentFrame.GetVerticalFrameSize())
	result.AgentViewportHeight = maxInt(1, agentInnerHeight-1)

	supervisorFrame := supervisorBorderStyle
	result.SupervisorViewportWidth = maxInt(
		1,
		result.Width-supervisorFrame.GetHorizontalFrameSize(),
	)
	supervisorInnerHeight := maxInt(
		1,
		result.SupervisorHeight-supervisorFrame.GetVerticalFrameSize(),
	)
	result.SupervisorViewportHeight = maxInt(1, supervisorInnerHeight-3)
	result.InputWidth = maxInt(1, result.SupervisorViewportWidth-2)
	return result
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
	// Keep a manually selected history position through terminal resizes while
	// still clamping it if the viewport grew beyond the remaining content.
	target.SetYOffset(previousOffset)
}

func (m *Model) resize(width, height int) {
	layout := CalculateLayout(width, height)
	m.width = layout.Width
	m.height = layout.Height
	m.leftWidth = layout.LeftWidth
	m.rightWidth = layout.RightWidth
	m.topHeight = layout.TopHeight
	m.supervisorHeight = layout.SupervisorHeight

	for index := range m.panes {
		resizeViewport(
			&m.panes[index].viewport,
			layout.AgentViewportWidths[index],
			layout.AgentViewportHeight,
		)
	}
	resizeViewport(
		&m.supervisor,
		layout.SupervisorViewportWidth,
		layout.SupervisorViewportHeight,
	)
	m.input.Width = layout.InputWidth

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
