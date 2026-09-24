import { FormEvent, useEffect, useLayoutEffect, useRef, useState } from "react";
import { ToolCallBadge } from "./ToolCallBadge";
import { useDialogKeyboard } from "../hooks/useDialogKeyboard";
import { reasonText } from "../lib/reason";
import { promptContextLines, safeEventSummary } from "../lib/safety";
import { answerLocked, deliveryRequiresResync, policyDecisionInProgress, typedAtTerminal } from "../lib/delivery";
import type { AgentState, SemanticDecision, SupervisionEvent } from "../types/relayer";

interface DecisionModalProps {
  event?: SupervisionEvent;
  agent?: AgentState;
  queueSize: number;
  readOnly?: boolean;
  onClose(): void;
  onSubmit(runID: string, sessionID: string, eventID: string, value: string): Promise<boolean>;
  onDecide(
    runID: string,
    sessionID: string,
    eventID: string,
    decision: SemanticDecision,
  ): Promise<boolean>;
}

const decisionLabels: Record<SemanticDecision, string> = {
  allow: "Allow",
  deny: "Deny",
};

const decisionShortcuts: Partial<Record<SemanticDecision, string>> = {
  allow: "Ctrl+↵",
  deny: "Esc",
};

export function DecisionModal({ event, agent, queueSize, readOnly, onClose, onSubmit, onDecide }: DecisionModalProps) {
  const inputRef = useRef<HTMLInputElement>(null);
  const transcriptRef = useRef<HTMLPreElement>(null);
  const dialogRef = useRef<HTMLElement>(null);
  const [busy, setBusy] = useState(false);

  // The prompt is the last thing the pane wrote, so the tail must open at its
  // end. Opening at the top shows the operator the output before the question.
  useLayoutEffect(() => {
    const transcript = transcriptRef.current;
    if (transcript) transcript.scrollTop = transcript.scrollHeight;
  });

  // Resizing the window shrinks this box without re-rendering React, and the
  // browser keeps scrollTop where it was — so the prompt silently drifts out of
  // view on the one element that exists to show it.
  useEffect(() => {
    const transcript = transcriptRef.current;
    if (!transcript || typeof ResizeObserver === "undefined") return;
    const observer = new ResizeObserver(() => {
      transcript.scrollTop = transcript.scrollHeight;
    });
    observer.observe(transcript);
    return () => observer.disconnect();
  }, [event?.runID, event?.sessionID, event?.id]);

  useEffect(() => {
    if (!event) return;
    if (inputRef.current) {
      inputRef.current.value = "";
      inputRef.current.focus();
    }
  }, [event?.runID, event?.sessionID, event?.id]);

  const indeterminateDelivery = event ? deliveryRequiresResync(event) : false;
  // Every path that sends something checks `locked`: the buttons, the form,
  // Ctrl+Enter and Esc. A prompt the server is delivering, or one the policy is
  // about to answer, is not this operator's to answer, whoever started it.
  const policyDeciding = event ? policyDecisionInProgress(event) : false;
  const locked = event ? answerLocked(event) : false;
  const reason = event ? reasonText(event.evaluation.reason) : undefined;
  const context = event?.sensitive ? [] : promptContextLines(agent?.output ?? "");
  const offered = (event?.decisions ?? []).filter(
    (decision): decision is SemanticDecision => decision === "allow" || decision === "deny",
  );

  const decide = async (decision: SemanticDecision) => {
    if (busy || locked || readOnly || !event) return;
    setBusy(true);
    try {
      const delivered = await onDecide(event.runID, event.sessionID, event.id, decision);
      if (delivered) onClose();
    } finally {
      setBusy(false);
    }
  };

  const submitDirect = async (value: string) => {
    if (!inputRef.current || value.length === 0 || busy || locked || readOnly || !event) return;
    inputRef.current.value = "";
    setBusy(true);
    try {
      const delivered = await onSubmit(event.runID, event.sessionID, event.id, value);
      if (delivered) onClose();
    } finally {
      if (inputRef.current) inputRef.current.value = "";
      setBusy(false);
    }
  };

  const submit = async (formEvent: FormEvent) => {
    formEvent.preventDefault();
    const input = inputRef.current;
    if (!input || input.value.length === 0 || readOnly) return;
    await submitDirect(input.value);
  };

  // Esc is the Deny shortcut, and Deny is an answer. On a prompt nobody may
  // answer from here it falls back to what Esc means everywhere else, which is
  // to minimize, and sends nothing.
  const handleEscape = () => {
    if (readOnly || locked) {
      onClose();
      return;
    }
    if (offered.includes("deny")) {
      void decide("deny");
    } else {
      onClose();
    }
  };

  useDialogKeyboard(dialogRef, {
    onClose,
    onEscape: handleEscape,
    closable: !busy,
    active: Boolean(event),
  });

  useEffect(() => {
    if (!event || busy || locked || readOnly) return;

    const handleKeyDown = (e: KeyboardEvent) => {
      if ((e.ctrlKey || e.metaKey) && e.key === "Enter") {
        e.preventDefault();
        e.stopPropagation();
        const inputVal = inputRef.current?.value.trim() ?? "";
        if (inputVal.length > 0) {
          void submitDirect(inputVal);
        } else if (offered.includes("allow")) {
          void decide("allow");
        }
      }
    };

    window.addEventListener("keydown", handleKeyDown, true);
    return () => window.removeEventListener("keydown", handleKeyDown, true);
  }, [event, busy, locked, offered, readOnly]);

  if (!event) return null;

  return (
    <div className="modal-layer" role="presentation">
      <section
        ref={dialogRef}
        className={`decision-modal${event.sensitive ? " decision-modal--sensitive" : ""}`}
        role="dialog"
        aria-modal="true"
        aria-labelledby="decision-title"
      >
        <div className="decision-modal__glow" aria-hidden="true" />
        <header className="decision-modal__header">
          <div className="decision-modal__signal" aria-hidden="true">!</div>
          <div>
            <span className="eyebrow">{policyDeciding ? "Policy decision in progress" : "Human action required"}</span>
            <h2 id="decision-title">{safeEventSummary(event)}</h2>
          </div>
          <button className="icon-button" type="button" onClick={onClose} disabled={busy} aria-label="Minimize">
            ×
          </button>
        </header>

        <div className="decision-modal__body">
        <div className="decision-context">
          <div>
            <span>Agent</span>
            <strong>{agent?.name || event.agentID}</strong>
            {agent?.simulated && (
              <em
                className="simulated-tag"
                title="Demo Bash script substituted for a real agent."
              >
                Simulated
              </em>
            )}
          </div>
          <div><span>Adapter</span><strong>{event.adapter}</strong></div>
          <div><span>Risk</span><strong className={`risk-text risk-text--${event.risk}`}>{event.risk}</strong></div>
          <div><span>Rule</span><strong>{event.evaluation.ruleName || "Safe default"}</strong></div>
          <div><span>Action</span><strong>{event.evaluation.action}</strong></div>
          <div><span>Delivery</span><strong>{event.deliveryStatus}</strong></div>
        </div>

        {reason && <p className="decision-reason">{reason}</p>}

        {/* Above the transcript: the question is what this tool call will do,
            so it should be readable without scrolling the output. Suppressed on
            a sensitive event for the same reason the transcript is. */}
        {!event.sensitive && <ToolCallBadge toolCall={event.toolCall} />}

        {context.length > 0 && (
          <div className="decision-transcript">
            <span className="eyebrow">End of output · {agent?.name || event.agentID}</span>
            <pre ref={transcriptRef} tabIndex={0} aria-label="Terminal context">{context.join("\n")}</pre>
          </div>
        )}

        {event.evaluation.dryRun && (
          <p className="dry-run-notice">DRY RUN · The decision stays entirely manual.</p>
        )}

        {indeterminateDelivery && (
          <p className="delivery-lock" role="alert">
            Indeterminate state — stop or resynchronize the session. No new input will be sent.
          </p>
        )}

        {!indeterminateDelivery && policyDeciding && (
          <p className="delivery-progress" role="status">
            The policy answers this prompt itself ({event.evaluation.action}). Nothing can be sent from here meanwhile.
          </p>
        )}

        {!indeterminateDelivery && !policyDeciding && event.deliveryStatus === "delivering" && (
          <p className="delivery-progress" role="status">
            An answer to this prompt is being delivered. Nothing more can be sent until it is confirmed.
          </p>
        )}

        {!indeterminateDelivery && typedAtTerminal(event) && (
          <p className="delivery-progress" role="status">
            Keys were typed at this terminal while the prompt was shown, and may have answered it. Answer it at the terminal: nothing can be sent from here.
          </p>
        )}

        {readOnly && (
          <p className="decision-modal__viewer-notice" role="alert">
            Mode Lecture Seule — En attente d'un arbitrage par un opérateur.
          </p>
        )}

        {offered.length > 0 && (
          <div className="decision-actions">
            {offered.map((decision) => (
              <button
                key={decision}
                type="button"
                className={`button button--decision button--decision-${decision}`}
                disabled={busy || locked || readOnly}
                onClick={() => void decide(decision)}
              >
                <span>{decisionLabels[decision]}</span>
                {decisionShortcuts[decision] && (
                  <kbd className="button__shortcut" title={`Shortcut: ${decisionShortcuts[decision]}`}>
                    {decisionShortcuts[decision]}
                  </kbd>
                )}
              </button>
            ))}
            <span>Answer encoded by the {event.adapter} adapter.</span>
          </div>
        )}

        <form className="decision-form" onSubmit={(formEvent) => void submit(formEvent)}>
          <label htmlFor="manual-decision">
            {event.sensitive
              ? "Confidential value"
              : offered.length > 0
                ? "Or answer manually"
                : "Answer to submit"}
          </label>
          <div className="decision-input-row">
            <input
              ref={inputRef}
              id="manual-decision"
              name="relayer-manual-decision"
              type={event.sensitive ? "password" : "text"}
              autoComplete={event.sensitive ? "new-password" : "off"}
              autoCapitalize="off"
              autoCorrect="off"
              spellCheck={false}
              data-1p-ignore
              placeholder={readOnly ? "Read-only (viewer)" : event.sensitive ? "••••••••" : "Type your answer…"}
              disabled={busy || locked || readOnly}
            />
            <button
              className={`button button--${offered.length > 0 ? "ghost" : "primary"}`}
              type="submit"
              disabled={busy || locked || readOnly}
            >
              {busy ? "Submitting…" : "Submit"}
            </button>
          </div>
          <p>
            {event.sensitive
              ? "The value is masked, submitted directly and never added to the interface logs."
              : "The answer is sent to this exact prompt occurrence."}
          </p>
        </form>

        </div>

        <footer className="decision-modal__footer">
          <span>{event.sensitive ? "Sensitive event" : `Event ${event.id}`}</span>
          {queueSize > 1 && <span>{queueSize - 1} other{queueSize > 2 ? "s" : ""} pending</span>}
        </footer>
      </section>
    </div>
  );
}
