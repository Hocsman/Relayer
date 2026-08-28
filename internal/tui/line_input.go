package tui

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/terminal"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

func isLineInputKey(message tea.KeyMsg) bool {
	return message.Type == tea.KeyRunes && len(message.Runes) == 1 && message.Runes[0] == 'i'
}

// beginLineInput gives the shared input field to one focused agent only after
// every UI-side safety gate is clear. The core repeats the authoritative
// pending-event check atomically with terminal delivery.
func (m *Model) beginLineInput(paneIndex int) tea.Cmd {
	if paneIndex < 0 || paneIndex >= len(m.panes) {
		return nil
	}
	target := &m.panes[paneIndex]
	name := safePolicyField(target.name)
	switch {
	case m.auditUnavailable:
		m.appendLog("Consigne directe refusée: audit indisponible")
		return nil
	case target.exited:
		m.appendLog(fmt.Sprintf("Consigne directe refusée pour %s: session terminée", name))
		return nil
	case target.policyFrozen:
		m.appendLog(fmt.Sprintf("Consigne directe refusée pour %s: état de livraison incertain", name))
		return nil
	case target.blocked || target.prompt.ID != "" || m.inputTarget != "" || len(m.pending) != 0:
		m.appendLog("Consigne directe refusée: une réponse superviseur est prioritaire")
		return nil
	case m.writePending:
		m.appendLog("Consigne directe refusée: une réponse manuelle est en cours")
		return nil
	case m.lineWritePending != "":
		m.appendLog("Consigne directe refusée: un envoi opérateur est déjà en cours")
		return nil
	case m.attachPending != "":
		m.appendLog("Consigne directe refusée: un attachement tmux est en cours")
		return nil
	}
	sessionKey := semanticEventKey(target.sessionID, "unused").sessionID
	if _, automatic := m.automaticBySession[sessionKey]; automatic {
		m.appendLog(fmt.Sprintf("Consigne directe refusée pour %s: une décision automatique est en cours", name))
		return nil
	}
	if _, ok := m.backend.(LineInputBackend); !ok {
		m.appendLog(fmt.Sprintf("Consigne directe indisponible pour %s: backend incompatible", name))
		return nil
	}

	m.inputTarget = ""
	m.lineInputTarget = target.sessionID
	m.input.Reset()
	m.input.EchoMode = textinput.EchoNormal
	m.input.Placeholder = fmt.Sprintf("Consigne pour %s (Entrée: envoyer • Échap: annuler)", target.name)
	setInputInterceptionStyle(&m.input, false)
	return m.input.Focus()
}

// cancelLineInput destroys the only model-owned copy of an unfinished line.
// It deliberately never logs the value or its length.
func (m *Model) cancelLineInput(announce bool) {
	target := m.lineInputTarget
	m.lineInputTarget = ""
	m.input.Reset()
	m.input.Blur()
	m.input.EchoMode = textinput.EchoNormal
	m.input.Placeholder = "En attente d'une validation interactive…"
	setInputInterceptionStyle(&m.input, false)
	if announce && target != "" {
		m.appendLog("Saisie de consigne directe annulée")
	}
}

func (m *Model) submitLineInput() tea.Cmd {
	target := m.lineInputTarget
	paneIndex := m.paneIndex(target)
	if target == "" || paneIndex < 0 {
		m.cancelLineInput(false)
		return nil
	}

	// Capture into the command closure, then erase the UI-owned value before
	// any audit write or asynchronous backend call can occur.
	value := m.input.Value()
	m.cancelLineInput(false)
	m.lineWritePending = target
	if !m.recordOperatorInput(paneIndex, audit.OutcomeInFlight, "operator_input_started") {
		m.lineWritePending = ""
		return nil
	}
	return deliverLineInput(m.backend, target, value, m.auditGate)
}

func (m *Model) applyLineInputResult(message lineInputDeliveredMsg) tea.Cmd {
	if !strings.EqualFold(strings.TrimSpace(m.lineWritePending), strings.TrimSpace(message.SessionID)) {
		return nil
	}
	m.lineWritePending = ""
	paneIndex := m.paneIndex(message.SessionID)
	if paneIndex < 0 {
		return nil
	}

	switch {
	case message.Err == nil:
		if !m.recordOperatorInput(paneIndex, audit.OutcomeApplied, "operator_input_applied") {
			return nil
		}
		m.appendLog(fmt.Sprintf("Consigne directe transmise à %s", safePolicyField(m.panes[paneIndex].name)))
		return m.resumeDeferredLineEvent(message.SessionID, nil, false)
	case errors.Is(message.Err, errAuditUnavailable):
		m.freezeAudit(paneIndex)
		return nil
	case errors.Is(message.Err, terminal.ErrEventPending):
		if !m.recordOperatorInput(paneIndex, audit.OutcomeFallbackStale, "operator_input_prompt_pending") {
			return nil
		}
		m.appendLog(fmt.Sprintf(
			"Consigne directe non envoyée à %s: prompt détecté, réponse superviseur prioritaire",
			safePolicyField(m.panes[paneIndex].name),
		))
		return m.resumeDeferredLineEvent(message.SessionID, message.Pending, message.PendingKnown)
	case errors.Is(message.Err, terminal.ErrInvalidLine):
		if !m.recordOperatorInput(paneIndex, audit.OutcomeSkipped, "operator_input_invalid") {
			return nil
		}
		m.appendLog("Consigne directe refusée: une seule ligne de texte valide est requise")
		return m.resumeDeferredLineEvent(message.SessionID, nil, false)
	case errors.Is(message.Err, terminal.ErrLineUnsupported):
		if !m.recordOperatorInput(paneIndex, audit.OutcomeSkipped, "operator_input_unsupported") {
			return nil
		}
		m.appendLog("Consigne directe indisponible: backend incompatible")
		return m.resumeDeferredLineEvent(message.SessionID, nil, false)
	default:
		if !m.recordOperatorInput(
			paneIndex,
			audit.OutcomeFallbackDeliveryUncertain,
			"operator_input_delivery_uncertain",
		) {
			return nil
		}
		deferred := m.takeDeferredLineEvent(message.SessionID)
		return m.freezeLineDelivery(paneIndex, deferred)
	}
}

func (m *Model) freezeLineDelivery(paneIndex int, deferred *adapters.Event) tea.Cmd {
	if paneIndex < 0 || paneIndex >= len(m.panes) || m.panes[paneIndex].exited {
		return nil
	}
	target := &m.panes[paneIndex]
	target.policyFrozen = true
	target.policyTag = "LIVRAISON INCERTAINE"
	target.blocked = true
	if deferred != nil {
		target.prompt = deferred.Clone()
	}
	m.removePending(target.sessionID)
	if m.inputTarget == target.sessionID {
		m.inputTarget = ""
		m.input.Reset()
		m.input.Blur()
		setInputInterceptionStyle(&m.input, false)
	}
	m.appendLog(fmt.Sprintf(
		"Consigne directe • agent=%s • status=delivery_uncertain • aucun nouvel envoi automatique • arrêt ou reprise contrôlée requis",
		safePolicyField(target.name),
	))
	if m.inputTarget == "" && !m.writePending {
		return m.activateNextPrompt()
	}
	return nil
}

func (m *Model) takeDeferredLineEvent(sessionID string) *adapters.Event {
	key := semanticEventKey(sessionID, "unused").sessionID
	event, exists := m.lineDeferredEvents[key]
	delete(m.lineDeferredEvents, key)
	if !exists {
		return nil
	}
	copy := event.Clone()
	return &copy
}

func (m *Model) resumeDeferredLineEvent(
	sessionID string,
	authoritative *adapters.Event,
	authoritativeKnown bool,
) tea.Cmd {
	deferred := m.takeDeferredLineEvent(sessionID)
	if authoritativeKnown {
		deferred = nil
		if authoritative != nil {
			copy := authoritative.Clone()
			deferred = &copy
		}
	}
	if deferred == nil {
		return nil
	}
	paneIndex := m.paneIndex(sessionID)
	if paneIndex < 0 || m.panes[paneIndex].exited {
		return nil
	}
	if m.panes[paneIndex].blocked && m.panes[paneIndex].prompt.ID == deferred.ID {
		return nil
	}
	current := deferred.Clone()
	current.SessionID = m.panes[paneIndex].sessionID
	return m.handleActionableEvent(current)
}

// finishLineInputOnExit makes a late command result inert. If a write was in
// flight, its exact consumption state is unknowable at the UI boundary, so the
// terminal audit record is conservative and no replacement input is admitted.
func (m *Model) finishLineInputOnExit(paneIndex int) {
	if paneIndex < 0 || paneIndex >= len(m.panes) {
		return
	}
	sessionID := m.panes[paneIndex].sessionID
	if strings.EqualFold(m.lineInputTarget, sessionID) {
		m.cancelLineInput(false)
	}
	if strings.EqualFold(m.lineWritePending, sessionID) {
		_ = m.recordOperatorInput(
			paneIndex,
			audit.OutcomeFallbackDeliveryUncertain,
			"operator_input_session_exit",
		)
		m.lineWritePending = ""
	}
	_ = m.takeDeferredLineEvent(sessionID)
}

func (m *Model) finishLineInputOnShutdown() {
	if m.lineInputTarget != "" {
		m.cancelLineInput(false)
	}
	if m.lineWritePending != "" {
		if paneIndex := m.paneIndex(m.lineWritePending); paneIndex >= 0 {
			_ = m.recordOperatorInput(
				paneIndex,
				audit.OutcomeFallbackDeliveryUncertain,
				"operator_input_shutdown",
			)
		}
		m.lineWritePending = ""
	}
	m.lineDeferredEvents = make(map[string]adapters.Event)
}

// cancelLineInputForPrompt discards a half-composed direct instruction because
// a prompt took the shared input field.
//
// The plain cancel is silent, which is right when the session ended or the line
// was just submitted. Here the operator was mid-sentence and the text vanishes
// under them, so it has to be said. The value itself is never logged, only that
// it was dropped.
func (m *Model) cancelLineInputForPrompt() {
	if m.lineInputTarget == "" {
		return
	}
	m.cancelLineInput(false)
	m.appendLog("Consigne directe abandonnée: une demande de supervision a pris la main")
}
