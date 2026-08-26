package tui

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/policy"
	"github.com/Hocsman/Relayer/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

func (m *Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	commands := make([]tea.Cmd, 0, 3)
	if event, ok := message.(backendEventMsg); ok {
		message = event.Event
		commands = append(commands, waitForBackendEvent(m.backend.Context(), m.events))
	}

	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		commands = append(commands, m.resize(msg.Width, msg.Height, true))
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC:
			m.backend.BeginShutdown()
			return m, tea.Quit
		case tea.KeyCtrlLeft:
			m.moveFocus(-1)
			commands = append(commands, m.syncFocus())
			break
		case tea.KeyCtrlRight:
			m.moveFocus(1)
			commands = append(commands, m.syncFocus())
			break
		case tea.KeyCtrlPgUp:
			m.movePage(-1)
			commands = append(commands, m.syncFocus())
			break
		case tea.KeyCtrlPgDown:
			m.movePage(1)
			commands = append(commands, m.syncFocus())
			break
		default:
			// A pending human response always wins over pane actions. In
			// particular, Enter must keep submitting PTY responses even if the
			// user temporarily moved focus away from the supervisor.
			if msg.Type == tea.KeyEnter && m.inputTarget != "" && !m.writePending {
				commands = append(commands, m.submitInput())
				break
			}
			if m.focus.Kind == FocusSupervisor && isViewportNavigationKey(msg) {
				var command tea.Cmd
				m.supervisor, command = m.supervisor.Update(msg)
				commands = append(commands, command)
				break
			}

			if m.focus.Kind == FocusSupervisor && m.inputTarget != "" {
				var command tea.Cmd
				m.input, command = m.input.Update(msg)
				commands = append(commands, command)
				break
			}

			if paneIndex := m.focusedPaneIndex(); paneIndex >= 0 {
				if msg.Type == tea.KeyEnter {
					commands = append(commands, m.beginAttach(paneIndex))
					break
				}
				var command tea.Cmd
				m.panes[paneIndex].viewport, command = m.panes[paneIndex].viewport.Update(msg)
				commands = append(commands, command)
			}
		}
	case tea.MouseMsg:
		commands = append(commands, m.handleMouse(msg))
	case session.OutputAvailable:
		if paneIndex := m.paneIndex(msg.SessionID); paneIndex >= 0 {
			m.refreshPaneOutput(paneIndex)
		}
	case session.AdapterEvent:
		observed := msg.Event.Clone()
		if observed.Type == adapters.EventProcessExit {
			commands = append(commands, m.applyProcessExit(observed))
			break
		}
		commands = append(commands, m.handleActionableEvent(observed))
	case session.Exited:
		if paneIndex := m.paneIndex(msg.SessionID); paneIndex >= 0 {
			// Legacy producers may still emit Exited beside the canonical
			// process_exit adapter event. Treat the latter as authoritative and
			// never duplicate lifecycle logs or queue transitions.
			if m.panes[paneIndex].exited {
				break
			}
			m.clearAutomaticState(msg.SessionID)
			m.refreshPaneOutput(paneIndex)
			m.panes[paneIndex].exited = true
			m.panes[paneIndex].exitErr = msg.Err
			if msg.Err == nil {
				m.appendLog(fmt.Sprintf("%s terminé", m.panes[paneIndex].name))
			} else {
				m.appendLog(fmt.Sprintf("%s terminé avec erreur: %v", m.panes[paneIndex].name, msg.Err))
			}
			wasInputTarget := m.inputTarget == msg.SessionID
			m.removePending(msg.SessionID)
			m.panes[paneIndex].blocked = false
			m.panes[paneIndex].policyFrozen = false
			m.panes[paneIndex].policyTag = ""
			m.rememberResolved(msg.SessionID, m.panes[paneIndex].prompt.ID)
			m.panes[paneIndex].prompt = adapters.Event{}
			if wasInputTarget {
				m.inputTarget = ""
				// In particular, never carry a password into the next agent.
				m.input.Reset()
				commands = append(commands, m.activateNextPrompt())
			}
		}
	case session.Error:
		if paneIndex := m.paneIndex(msg.SessionID); paneIndex >= 0 {
			m.recordBackendError(paneIndex, "backend_event")
			m.appendLog(fmt.Sprintf("Erreur terminal de %s: %v", m.panes[paneIndex].name, msg.Err))
		}
	case inputDeliveredMsg:
		m.writePending = false
		paneIndex := m.paneIndex(msg.SessionID)
		if paneIndex < 0 {
			break
		}
		outcome := audit.OutcomeSucceeded
		reason := "delivery_succeeded"
		if msg.Err != nil {
			outcome = audit.OutcomeFailed
			reason = "delivery_failed"
		}
		if !m.recordDelivery(
			paneIndex,
			msg.Event,
			audit.DecisionAsk,
			audit.DecisionByHuman,
			outcome,
			reason,
		) {
			// Delivery has already been attempted. Never retry after losing the
			// synchronized audit boundary, regardless of the transport result.
			break
		}
		if msg.Err != nil {
			m.panes[paneIndex].policyTag = "ASK REQUISE"
			m.appendLog(fmt.Sprintf("Échec de l'envoi à %s (réponse non acquittée)", m.panes[paneIndex].name))
			if !m.panes[paneIndex].exited && !m.panes[paneIndex].blocked {
				m.panes[paneIndex].blocked = true
				m.panes[paneIndex].prompt = msg.Event.Clone()
				m.prependPending(msg.SessionID)
			}
		} else {
			m.panes[paneIndex].policyTag = "ASK APPLIQUÉE"
			m.rememberResolved(msg.SessionID, msg.Event.ID)
		}
		commands = append(commands, m.activateNextPrompt())
	case automaticDecisionFinishedMsg:
		commands = append(commands, m.applyAutomaticDecisionResult(msg))
	case attachFinishedMsg:
		paneIndex := m.paneIndex(msg.SessionID)
		if paneIndex < 0 {
			if m.attachInProgress(msg.SessionID) {
				m.attachPending = ""
				m.attachFinishedAudited = false
				m.attachReturned = false
			}
			break
		}
		if !m.attachInProgress(msg.SessionID) || m.attachReturned {
			break
		}
		m.attachReturned = true
		if msg.Err != nil {
			// Even if this post-attach write fails, Resync remains mandatory: it
			// reconciles lifecycle/prompt state but cannot send a decision.
			m.recordAttach(paneIndex, audit.KindAttachFinished, audit.OutcomeFailed, "attach_client_failed")
			m.attachFinishedAudited = true
			m.appendLog(fmt.Sprintf("Session tmux %s interrompue: %v", m.panes[paneIndex].name, msg.Err))
		} else {
			m.appendLog(fmt.Sprintf("Retour de la session tmux %s", m.panes[paneIndex].name))
		}
		attachable, ok := m.backend.(AttachableBackend)
		if !ok {
			if !m.attachFinishedAudited {
				m.recordAttach(paneIndex, audit.KindAttachFinished, audit.OutcomeFailed, "attach_resync_backend_missing")
				m.attachFinishedAudited = true
			}
			m.recordBackendError(paneIndex, "attach_resync_backend_missing")
			m.appendLog("Resynchronisation tmux impossible: backend incompatible")
			m.attachPending = ""
			m.attachFinishedAudited = false
			m.attachReturned = false
			commands = append(commands, m.freezeResyncFailure(paneIndex))
			break
		}
		commands = append(commands, resyncAttachedSession(
			m.backend.Context(),
			attachable,
			msg.SessionID,
			m.panes[paneIndex].viewport.Width,
			m.panes[paneIndex].viewport.Height,
		))
	case resyncFinishedMsg:
		paneIndex := m.paneIndex(msg.SessionID)
		if paneIndex < 0 {
			if m.attachInProgress(msg.SessionID) {
				m.attachPending = ""
				m.attachFinishedAudited = false
				m.attachReturned = false
			}
			break
		}
		if !m.attachInProgress(msg.SessionID) || !m.attachReturned {
			break
		}
		m.attachPending = ""
		m.refreshPaneOutput(paneIndex)
		if msg.Err != nil {
			if !m.attachFinishedAudited {
				m.recordAttach(paneIndex, audit.KindAttachFinished, audit.OutcomeFailed, "detach_resync_failed")
			}
			m.recordBackendError(paneIndex, "detach_resync_failed")
			m.appendLog(fmt.Sprintf("Resynchronisation de %s impossible: %v", m.panes[paneIndex].name, msg.Err))
			commands = append(commands, m.freezeResyncFailure(paneIndex))
		} else {
			if !m.attachFinishedAudited {
				m.recordAttach(paneIndex, audit.KindAttachFinished, audit.OutcomeSucceeded, "detach_resynced")
			}
			commands = append(commands, m.reconcileEvent(msg.SessionID, msg.Pending))
			m.appendLog(fmt.Sprintf("%s resynchronisé (sortie, état, prompts et taille)", m.panes[paneIndex].name))
		}
		m.attachFinishedAudited = false
		m.attachReturned = false
	case resizeFinishedMsg:
		m.resizeInFlight = false
		if m.backend.Context().Err() == nil {
			for _, failure := range msg.Failures {
				paneIndex := m.paneIndex(failure.SessionID)
				m.recordBackendError(paneIndex, "resize_failed")
				m.appendLog("Redimensionnement de " + failure.Name + " impossible: " + failure.Err.Error())
			}
		}
		if msg.Generation < m.resizeGeneration {
			if contextual, ok := m.backend.(ContextResizeBackend); ok {
				commands = append(commands, m.startResize(contextual))
			}
		}
	case backendStoppedMsg:
		return m, tea.Quit
	}

	return m, batchCommands(commands...)
}

func (m *Model) handleActionableEvent(observed adapters.Event) tea.Cmd {
	if m.auditUnavailable || !observed.Actionable() || m.eventResolved(observed.SessionID, observed.ID) {
		return nil
	}
	paneIndex := m.paneIndex(observed.SessionID)
	if paneIndex < 0 || m.panes[paneIndex].exited {
		return nil
	}
	if m.panes[paneIndex].policyFrozen {
		m.refreshPaneOutput(paneIndex)
		return nil
	}
	if m.attachInProgress(observed.SessionID) {
		// Events emitted before or during an interactive tmux attachment can be
		// stale because the human may answer directly in tmux. Only the
		// authoritative snapshot read after Resync may drive policy evaluation.
		m.refreshPaneOutput(paneIndex)
		return nil
	}

	// PendingEvent is an in-memory authoritative snapshot for both PTY and
	// tmux. Consulting it prevents a delayed channel event from resurrecting an
	// occurrence after the bounded resolved-ID cache evicts it.
	snapshotKnown := false
	if snapshots, ok := m.backend.(EventSnapshotBackend); ok {
		current, err := snapshots.PendingEvent(m.backend.Context(), observed.SessionID)
		if err == nil {
			snapshotKnown = true
			if current == nil {
				m.rememberResolved(observed.SessionID, observed.ID)
				return nil
			}
			observed = current.Clone()
			observed.SessionID = m.panes[paneIndex].sessionID
			if m.eventResolved(observed.SessionID, observed.ID) {
				return nil
			}
		} else if !m.recordBackendError(paneIndex, "pending_snapshot_failed") {
			return nil
		}
	}

	sessionKey := semanticEventKey(observed.SessionID, observed.ID).sessionID
	if active, exists := m.automaticBySession[sessionKey]; exists {
		if active == semanticEventKey(observed.SessionID, observed.ID) {
			return nil
		}
		m.deferredEvents[sessionKey] = observed.Clone()
		return nil
	}
	if m.panes[paneIndex].blocked && m.panes[paneIndex].prompt.ID == observed.ID {
		return nil
	}

	// Only the canonical occurrence from PendingEvent reaches the audit. This
	// keeps delayed channel duplicates and superseded prompts out of the trail.
	if !m.recordEventDetected(paneIndex, observed) {
		return nil
	}
	evaluation := m.policy.Evaluate(observed.Clone())
	if requiresHumanSafety(observed) {
		evaluation.Action = policy.ActionAsk
		evaluation.Automatic = false
		evaluation.Reason = policy.ReasonSensitive
	}
	if m.policyConfig.DryRun || evaluation.DryRun {
		evaluation.Action = policy.ActionAsk
		evaluation.Automatic = false
		evaluation.DryRun = true
		if evaluation.Reason != policy.ReasonSensitive && evaluation.Reason != policy.ReasonRisk {
			evaluation.Reason = policy.ReasonDryRun
		}
	}
	snapshotUnavailable := !snapshotKnown && m.backendSupportsSnapshots()
	if snapshotUnavailable {
		// Without the authoritative occurrence snapshot, a configured allow or
		// deny is not an effective automatic decision.
		evaluation.Action = policy.ActionAsk
		evaluation.Automatic = false
	}
	if !m.recordPolicyEvaluation(paneIndex, observed, evaluation) {
		return nil
	}

	decision, automatic := automaticDecision(evaluation)
	if !automatic || snapshotUnavailable {
		status := "ask"
		if evaluation.DryRun {
			status = "dry_run"
		} else if snapshotUnavailable {
			status = "snapshot_unavailable"
		}
		return m.queueHumanEvent(observed, evaluation, status)
	}

	if !m.recordDecision(paneIndex, observed, decisionForAdapter(decision), audit.DecisionByPolicy) {
		return nil
	}
	key := semanticEventKey(observed.SessionID, observed.ID)
	m.automaticInFlight[key] = automaticAttempt{event: observed.Clone(), evaluation: evaluation}
	m.automaticBySession[sessionKey] = key
	m.panes[paneIndex].policyTag = "AUTO EN COURS"
	m.appendPolicyLog(paneIndex, observed, evaluation, "in_flight")
	return deliverAutomaticDecision(m.backend, observed, evaluation, decision, m.auditGate)
}

func (m *Model) backendSupportsSnapshots() bool {
	_, ok := m.backend.(EventSnapshotBackend)
	return ok
}

func automaticDecision(evaluation policy.Evaluation) (adapters.Decision, bool) {
	if !evaluation.Automatic {
		return "", false
	}
	switch evaluation.Action {
	case policy.ActionAllow:
		return adapters.DecisionAllow, true
	case policy.ActionDeny:
		return adapters.DecisionDeny, true
	default:
		return "", false
	}
}

func (m *Model) queueHumanEvent(event adapters.Event, evaluation policy.Evaluation, status string) tea.Cmd {
	if m.auditUnavailable {
		return nil
	}
	paneIndex := m.paneIndex(event.SessionID)
	if paneIndex < 0 || m.panes[paneIndex].exited || m.eventResolved(event.SessionID, event.ID) {
		return nil
	}
	target := &m.panes[paneIndex]
	if target.blocked && target.prompt.ID == event.ID {
		return nil
	}
	if target.blocked && target.prompt.ID != "" {
		m.rememberResolved(event.SessionID, target.prompt.ID)
		if m.inputTarget == event.SessionID {
			m.inputTarget = ""
			m.input.Reset()
			m.input.Blur()
			setInputInterceptionStyle(&m.input, false)
		}
	}

	m.refreshPaneOutput(paneIndex)
	target.blocked = true
	target.prompt = event.Clone()
	target.policyTag = humanPolicyTag(evaluation, status)
	m.removePending(event.SessionID)
	m.pending = append(m.pending, event.SessionID)
	m.appendPolicyLog(paneIndex, event, evaluation, status)
	reason := "confirmation requise"
	if requiresSecretHandling(event) {
		reason = "saisie sensible requise"
	}
	m.appendLog(fmt.Sprintf("%s attend une intervention humaine (%s)", target.name, reason))
	if m.inputTarget == "" && !m.writePending {
		return m.activateNextPrompt()
	}
	return nil
}

func humanPolicyTag(evaluation policy.Evaluation, status string) string {
	if evaluation.DryRun || status == "dry_run" {
		return "DRY RUN • ASK"
	}
	if strings.HasPrefix(status, "fallback_") || status == "snapshot_unavailable" {
		return "AUTO → ASK"
	}
	return "ASK"
}

func (m *Model) applyAutomaticDecisionResult(message automaticDecisionFinishedMsg) tea.Cmd {
	key := semanticEventKey(message.SessionID, message.Event.ID)
	attempt, exists := m.automaticInFlight[key]
	if !exists {
		return nil
	}
	sessionKey := key.sessionID
	if active, activeExists := m.automaticBySession[sessionKey]; !activeExists || active != key {
		return nil
	}
	decision, _ := automaticDecision(attempt.evaluation)
	outcome := audit.OutcomeSucceeded
	reason := "delivery_succeeded"
	if message.Err != nil {
		outcome, reason = automaticDeliveryAudit(message.Err)
	}
	if !m.recordDelivery(
		m.paneIndex(message.SessionID),
		attempt.event,
		decisionForAdapter(decision),
		audit.DecisionByPolicy,
		outcome,
		reason,
	) {
		// The backend call has already returned. Losing audit durability here
		// makes any retry unsafe, even when the error looks recoverable.
		return nil
	}
	delete(m.automaticInFlight, key)
	delete(m.automaticBySession, sessionKey)

	paneIndex := m.paneIndex(message.SessionID)
	if paneIndex < 0 || m.panes[paneIndex].exited {
		delete(m.deferredEvents, sessionKey)
		return nil
	}
	if message.Err == nil {
		m.rememberResolved(message.SessionID, message.Event.ID)
		m.panes[paneIndex].policyTag = "AUTO APPLIQUÉE"
		m.appendPolicyLog(paneIndex, attempt.event, attempt.evaluation, "applied")
		if deferred, ok := m.deferredEvents[sessionKey]; ok {
			delete(m.deferredEvents, sessionKey)
			return m.handleActionableEvent(deferred)
		}
		return nil
	}

	status := automaticFailureStatus(message.Err)
	m.panes[paneIndex].policyTag = "AUTO → ASK"
	m.appendPolicyLog(paneIndex, attempt.event, attempt.evaluation, status)
	delete(m.deferredEvents, sessionKey)
	if status == "delivery_uncertain" {
		candidate := attempt.event.Clone()
		if message.Pending != nil {
			candidate = message.Pending.Clone()
			candidate.SessionID = message.SessionID
		}
		return m.freezePolicyDelivery(paneIndex, candidate)
	}
	if message.PendingKnown && message.Pending == nil {
		m.rememberResolved(message.SessionID, message.Event.ID)
		m.panes[paneIndex].policyTag = "AUTO NON APPLIQUÉE"
		return nil
	}
	candidate := attempt.event.Clone()
	if message.Pending != nil {
		candidate = message.Pending.Clone()
		candidate.SessionID = message.SessionID
	}
	fallback := attempt.evaluation
	if candidate.ID != attempt.event.ID {
		fallback = m.policy.Evaluate(candidate.Clone())
	}
	fallback.Action = policy.ActionAsk
	fallback.Automatic = false
	if candidate.ID != attempt.event.ID {
		if !m.recordEventDetected(paneIndex, candidate) ||
			!m.recordPolicyEvaluation(paneIndex, candidate, fallback) {
			return nil
		}
	}
	if status == "stale" && candidate.ID == attempt.event.ID {
		return m.freezePolicyDelivery(paneIndex, candidate)
	}
	return m.queueHumanEvent(candidate, fallback, "fallback_"+status)
}

func (m *Model) freezePolicyDelivery(paneIndex int, event adapters.Event) tea.Cmd {
	if paneIndex < 0 || paneIndex >= len(m.panes) || m.panes[paneIndex].exited {
		return nil
	}
	target := &m.panes[paneIndex]
	target.policyFrozen = true
	target.policyTag = "LIVRAISON INCERTAINE"
	target.blocked = true
	target.prompt = event.Clone()
	m.removePending(target.sessionID)
	if m.inputTarget == target.sessionID {
		m.inputTarget = ""
		m.input.Reset()
		m.input.Blur()
		setInputInterceptionStyle(&m.input, false)
	}
	m.appendLog(fmt.Sprintf(
		"Politique • agent=%s • status=delivery_uncertain • aucune nouvelle réponse envoyée • arrêt ou reprise contrôlée requis",
		safePolicyField(target.name),
	))
	if m.inputTarget == "" && !m.writePending {
		return m.activateNextPrompt()
	}
	return nil
}

func (m *Model) freezeResyncFailure(paneIndex int) tea.Cmd {
	if m.auditUnavailable {
		return nil
	}
	if paneIndex < 0 || paneIndex >= len(m.panes) || m.panes[paneIndex].exited {
		return nil
	}
	target := &m.panes[paneIndex]
	target.policyFrozen = true
	target.policyTag = "ÉTAT TMUX INCERTAIN"
	target.blocked = true
	target.prompt = adapters.Event{}
	m.removePending(target.sessionID)
	if m.inputTarget == target.sessionID {
		m.inputTarget = ""
		m.input.Reset()
		m.input.Blur()
		setInputInterceptionStyle(&m.input, false)
	}
	m.appendLog(fmt.Sprintf(
		"Politique • agent=%s • status=resync_failed • aucune décision envoyée • arrêt requis",
		safePolicyField(target.name),
	))
	if m.inputTarget == "" && !m.writePending {
		return m.activateNextPrompt()
	}
	return nil
}

func automaticFailureStatus(err error) string {
	switch {
	case errors.Is(err, adapters.ErrDecisionUnsupported), errors.Is(err, errAutomaticDecisionBackendUnavailable):
		return "unsupported"
	case errors.Is(err, adapters.ErrEventMismatch):
		return "stale"
	default:
		// Once delivery has been attempted, an arbitrary transport error cannot
		// prove whether the target consumed the bytes. Reconcile the exact
		// pending occurrence and ask instead of retrying automatically.
		return "delivery_uncertain"
	}
}

func (m *Model) clearAutomaticState(sessionID string) {
	sessionKey := semanticEventKey(sessionID, "unused").sessionID
	if key, exists := m.automaticBySession[sessionKey]; exists {
		delete(m.automaticInFlight, key)
		delete(m.automaticBySession, sessionKey)
	}
	delete(m.deferredEvents, sessionKey)
}

func requiresHumanSafety(event adapters.Event) bool {
	return event.Sensitive || event.Type == adapters.EventCredential
}

func requiresSecretHandling(event adapters.Event) bool {
	return requiresHumanSafety(event) || event.Risk == adapters.RiskHigh
}

func (m *Model) appendPolicyLog(
	paneIndex int,
	event adapters.Event,
	evaluation policy.Evaluation,
	status string,
) {
	mode := "enforce"
	if m.policyConfig.DryRun || evaluation.DryRun {
		mode = "dry_run"
	}
	rule := evaluation.RuleName
	if strings.TrimSpace(rule) == "" {
		rule = "default"
	}
	risk := event.Risk
	if risk == "" {
		risk = adapters.RiskUnknown
	}
	m.appendLog(fmt.Sprintf(
		"Politique • agent=%s • adapter=%s • type=%s • risk=%s • summary=%s • rule=%s • proposed=%s • effective=%s • automatic=%t • mode=%s • status=%s • reason=%s",
		safePolicyField(m.panes[paneIndex].name),
		safePolicyField(m.panes[paneIndex].adapter),
		safeEventType(event.Type),
		safeRisk(risk),
		safeEventSummary(event),
		safePolicyField(rule),
		safeAction(evaluation.ProposedAction),
		safeAction(evaluation.Action),
		evaluation.Automatic,
		mode,
		safePolicyField(status),
		safeReason(evaluation.Reason),
	))
}

func safeEventType(value adapters.EventType) string {
	switch value {
	case adapters.EventConfirmation, adapters.EventCredential, adapters.EventProcessExit:
		return string(value)
	default:
		return "unknown"
	}
}

func safeRisk(value adapters.RiskLevel) string {
	switch value {
	case adapters.RiskLow, adapters.RiskUnknown, adapters.RiskHigh:
		return string(value)
	default:
		return string(adapters.RiskUnknown)
	}
}

func safeAction(value policy.Action) string {
	switch value {
	case policy.ActionAllow, policy.ActionAsk, policy.ActionDeny:
		return string(value)
	default:
		return "unknown"
	}
}

func safeReason(value string) string {
	switch value {
	case policy.ReasonDefault,
		policy.ReasonRule,
		policy.ReasonInvalidEvent,
		policy.ReasonNonActionable,
		policy.ReasonSensitive,
		policy.ReasonRisk,
		policy.ReasonDryRun,
		policy.ReasonNoEngine:
		return value
	default:
		return "unknown"
	}
}

func safeEventSummary(event adapters.Event) string {
	if requiresSecretHandling(event) {
		return "sensitive_event"
	}
	value := strings.Map(func(character rune) rune {
		if unicode.IsControl(character) {
			return ' '
		}
		return character
	}, event.Summary)
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		return "-"
	}
	characters := []rune(value)
	const maximumLength = 80
	if len(characters) > maximumLength {
		characters = characters[:maximumLength]
	}
	return string(characters)
}

func safePolicyField(value string) string {
	value = strings.Map(func(character rune) rune {
		if unicode.IsControl(character) {
			return ' '
		}
		return character
	}, value)
	value = strings.Join(strings.Fields(value), "_")
	if value == "" {
		return "-"
	}
	characters := []rune(value)
	const maximumLength = 64
	if len(characters) > maximumLength {
		characters = characters[:maximumLength]
	}
	return string(characters)
}

func (m *Model) reconcileEvent(sessionID string, pending *adapters.Event) tea.Cmd {
	paneIndex := m.paneIndex(sessionID)
	if paneIndex < 0 {
		return nil
	}
	if m.panes[paneIndex].policyFrozen {
		// A cached pending snapshot cannot prove whether a prior transport write
		// was consumed. Keep the pane frozen until exit or restart.
		return nil
	}
	wasInputTarget := m.inputTarget == sessionID
	previousID := m.panes[paneIndex].prompt.ID
	m.removePending(sessionID)
	m.panes[paneIndex].blocked = false
	m.panes[paneIndex].policyTag = ""
	m.panes[paneIndex].prompt = adapters.Event{}
	if wasInputTarget {
		m.inputTarget = ""
		m.input.Reset()
		m.input.Blur()
		setInputInterceptionStyle(&m.input, false)
	}
	if pending != nil && !m.panes[paneIndex].exited && !m.eventResolved(sessionID, pending.ID) {
		current := pending.Clone()
		current.SessionID = sessionID
		return m.handleActionableEvent(current)
	} else {
		m.rememberResolved(sessionID, previousID)
	}
	if m.inputTarget == "" && !m.writePending {
		return m.activateNextPrompt()
	}
	return nil
}

func (m *Model) submitInput() tea.Cmd {
	if m.auditUnavailable {
		m.input.Reset()
		m.input.Blur()
		return nil
	}
	paneIndex := m.paneIndex(m.inputTarget)
	if paneIndex < 0 {
		return nil
	}
	targetID := m.inputTarget
	event := m.panes[paneIndex].prompt.Clone()
	if !m.recordDecision(paneIndex, event, audit.DecisionAsk, audit.DecisionByHuman) {
		return nil
	}
	value := m.input.Value()

	// Clear the old waiting state before the asynchronous write. This lets an
	// immediate second prompt from this session enter the queue.
	m.panes[paneIndex].blocked = false
	m.panes[paneIndex].prompt = adapters.Event{}
	m.removePending(targetID)
	m.inputTarget = ""
	m.input.Reset()
	m.input.Blur()
	setInputInterceptionStyle(&m.input, false)
	m.writePending = true
	m.panes[paneIndex].policyTag = "ASK EN COURS"
	m.appendLog(fmt.Sprintf("Réponse transmise à %s", m.panes[paneIndex].name))
	return deliverInput(m.backend, targetID, value, event, m.auditGate)
}

func (m *Model) applyProcessExit(event adapters.Event) tea.Cmd {
	paneIndex := m.paneIndex(event.SessionID)
	if paneIndex < 0 || m.panes[paneIndex].exited {
		return nil
	}
	// The canonical adapter event is the only source of a session_finished
	// fact. Legacy Exited messages only reduce UI state and never duplicate it.
	m.recordProcessExit(paneIndex, event)
	m.clearAutomaticState(event.SessionID)
	m.refreshPaneOutput(paneIndex)
	m.panes[paneIndex].exited = true
	if event.Metadata["failed"] == "true" {
		m.panes[paneIndex].exitErr = fmt.Errorf("processus terminé avec erreur")
	}
	m.rememberResolved(event.SessionID, m.panes[paneIndex].prompt.ID)
	m.removePending(event.SessionID)
	m.panes[paneIndex].blocked = false
	m.panes[paneIndex].policyFrozen = false
	m.panes[paneIndex].policyTag = ""
	m.panes[paneIndex].prompt = adapters.Event{}
	if m.inputTarget == event.SessionID {
		m.inputTarget = ""
		m.input.Reset()
		m.input.Blur()
		setInputInterceptionStyle(&m.input, false)
	}
	summary := event.Summary
	if summary == "" {
		summary = "processus terminé"
	}
	m.appendLog(fmt.Sprintf("%s: %s", m.panes[paneIndex].name, summary))
	if m.inputTarget == "" && !m.writePending {
		return m.activateNextPrompt()
	}
	return nil
}

func (m *Model) beginAttach(paneIndex int) tea.Cmd {
	pane := &m.panes[paneIndex]
	if pane.backend != "tmux" {
		return nil
	}
	if m.auditUnavailable {
		return nil
	}
	if pane.policyFrozen {
		m.appendLog(fmt.Sprintf("Attachement de %s refusé: livraison de politique incertaine; arrêt requis", pane.name))
		return nil
	}
	if m.writePending {
		m.appendLog(fmt.Sprintf("Attachement de %s différé: réponse manuelle en cours", pane.name))
		return nil
	}
	sessionKey := semanticEventKey(pane.sessionID, "unused").sessionID
	if _, automatic := m.automaticBySession[sessionKey]; automatic {
		m.appendLog(fmt.Sprintf("Attachement de %s différé: décision automatique en cours", pane.name))
		return nil
	}
	if m.attachPending != "" {
		return nil
	}
	attachable, ok := m.backend.(AttachableBackend)
	if !ok {
		m.recordBackendError(paneIndex, "attach_backend_incompatible")
		m.appendLog(fmt.Sprintf("Attachement de %s impossible: backend tmux incompatible", pane.name))
		return nil
	}
	if _, ok := m.backend.(EventSnapshotBackend); !ok {
		m.recordBackendError(paneIndex, "attach_snapshot_unavailable")
		m.appendLog(fmt.Sprintf("Attachement de %s impossible: snapshot d'événement indisponible", pane.name))
		return nil
	}
	if !m.recordAttach(paneIndex, audit.KindAttachStarted, audit.OutcomeStarted, "attach_requested") {
		return nil
	}
	command, err := attachable.AttachCommand(m.backend.Context(), pane.sessionID)
	if err != nil {
		m.recordAttachFailure(paneIndex, "attach_command_failed")
		m.appendLog(fmt.Sprintf("Attachement de %s impossible: %v", pane.name, err))
		return nil
	}
	if command == nil {
		m.recordAttachFailure(paneIndex, "attach_command_empty")
		m.appendLog(fmt.Sprintf("Attachement de %s impossible: commande vide", pane.name))
		return nil
	}
	sessionID := pane.sessionID
	m.attachPending = sessionID
	m.attachFinishedAudited = false
	m.attachReturned = false
	m.appendLog(fmt.Sprintf("Ouverture interactive de %s via tmux", pane.name))
	executor := m.execProcess
	if executor == nil {
		executor = tea.ExecProcess
	}
	execute := executor(command, func(err error) tea.Msg {
		return attachFinishedMsg{SessionID: sessionID, Err: err}
	})
	return func() tea.Msg {
		if !m.auditGate.beginOperation() {
			return attachFinishedMsg{SessionID: sessionID, Err: errAuditUnavailable}
		}
		defer m.auditGate.endOperation()
		return execute()
	}
}

func (m *Model) attachInProgress(sessionID string) bool {
	if m.attachPending == "" {
		return false
	}
	want := semanticEventKey(sessionID, "unused").sessionID
	active := semanticEventKey(m.attachPending, "unused").sessionID
	return want != "" && want == active
}

func isViewportNavigationKey(message tea.KeyMsg) bool {
	switch message.Type {
	case tea.KeyUp, tea.KeyDown, tea.KeyPgUp, tea.KeyPgDown:
		return true
	default:
		return false
	}
}

func (m *Model) moveFocus(delta int) {
	position := len(m.panes)
	if paneIndex := m.focusedPaneIndex(); paneIndex >= 0 {
		position = paneIndex
	}
	position = (position + delta) % (len(m.panes) + 1)
	if position < 0 {
		position += len(m.panes) + 1
	}
	if position == len(m.panes) {
		m.focus = FocusTarget{Kind: FocusSupervisor}
		return
	}
	m.focus = FocusTarget{Kind: FocusAgent, AgentID: m.panes[position].sessionID}
	m.setPage(position / maxAgentsPerPage)
}

func (m *Model) movePage(delta int) {
	newPage := clampInt(m.page+delta, 0, pageCount(len(m.panes))-1)
	if newPage == m.page {
		return
	}
	m.setPage(newPage)
	if m.focus.Kind == FocusAgent && len(m.layout.Cells) > 0 {
		index := m.layout.Cells[0].AgentIndex
		m.focus.AgentID = m.panes[index].sessionID
	}
}

// handleMouse routes wheel events using the same cells that View renders and
// lets a left click select an agent or the supervisor explicitly.
func (m *Model) handleMouse(message tea.MouseMsg) tea.Cmd {
	event := tea.MouseEvent(message)
	if event.X < 0 || event.X >= m.width || event.Y < 0 || event.Y >= m.height {
		return nil
	}

	if event.Action == tea.MouseActionPress && event.Button == tea.MouseButtonLeft {
		for _, cell := range m.layout.Cells {
			if cell.Outer.Contains(event.X, event.Y) {
				m.focus = FocusTarget{Kind: FocusAgent, AgentID: m.panes[cell.AgentIndex].sessionID}
				m.input.Blur()
				return nil
			}
		}
		if m.layout.Supervisor.Contains(event.X, event.Y) {
			m.focus = FocusTarget{Kind: FocusSupervisor}
			return m.syncFocus()
		}
		return nil
	}

	if event.Action != tea.MouseActionPress ||
		(event.Button != tea.MouseButtonWheelUp && event.Button != tea.MouseButtonWheelDown) {
		return nil
	}
	for _, cell := range m.layout.Cells {
		if cell.Outer.Contains(event.X, event.Y) {
			var command tea.Cmd
			index := cell.AgentIndex
			m.panes[index].viewport, command = m.panes[index].viewport.Update(message)
			return command
		}
	}
	if m.layout.Supervisor.Contains(event.X, event.Y) {
		var command tea.Cmd
		m.supervisor, command = m.supervisor.Update(message)
		return command
	}
	return nil
}
