import { FormEvent, useEffect, useLayoutEffect, useRef, useState } from "react";
import { useDialogKeyboard } from "../hooks/useDialogKeyboard";
import { promptContextLines, safeEventSummary } from "../lib/safety";
import { deliveryRequiresResync } from "../lib/delivery";
import type { AgentState, SemanticDecision, SupervisionEvent } from "../types/relayer";

interface DecisionModalProps {
  event?: SupervisionEvent;
  agent?: AgentState;
  queueSize: number;
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

export function DecisionModal({ event, agent, queueSize, onClose, onSubmit, onDecide }: DecisionModalProps) {
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

  useDialogKeyboard(dialogRef, { onClose, closable: !busy, active: Boolean(event) });

  if (!event) return null;

  const indeterminateDelivery = deliveryRequiresResync(event);
  // Only what the adapter reported for this exact occurrence. An unknown value
  // arriving from a stale bridge is dropped rather than rendered as a button.
  // What the pane was showing when it stopped. A decision made without it is
  // made on a one-line summary.
  //
  // A sensitive prompt is excluded: safeEventSummary already refuses to repeat
  // its text here, and reprinting the pane tail underneath would undo that.
  const context = event.sensitive ? [] : promptContextLines(agent?.output ?? "");
  const offered = (event.decisions ?? []).filter(
    (decision): decision is SemanticDecision => decision === "allow" || decision === "deny",
  );

  const decide = async (decision: SemanticDecision) => {
    if (busy || indeterminateDelivery) return;
    setBusy(true);
    try {
      const delivered = await onDecide(event.runID, event.sessionID, event.id, decision);
      if (delivered) onClose();
    } finally {
      setBusy(false);
    }
  };

  const submit = async (formEvent: FormEvent) => {
    formEvent.preventDefault();
    const input = inputRef.current;
    if (!input || input.value.length === 0 || busy || indeterminateDelivery) return;
    const value = input.value;
    input.value = "";
    setBusy(true);
    try {
      const delivered = await onSubmit(event.runID, event.sessionID, event.id, value);
      if (delivered) onClose();
    } finally {
      // The manual value is never put in component state, notifications or logs.
      input.value = "";
      setBusy(false);
    }
  };

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
            <span className="eyebrow">Human action required</span>
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

        {context.length > 0 && (
          <div className="decision-transcript">
            <span className="eyebrow">End of output · {agent?.name || event.agentID}</span>
            <pre ref={transcriptRef} aria-label="Terminal context">{context.join("\n")}</pre>
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

        {offered.length > 0 && (
          <div className="decision-actions">
            {offered.map((decision) => (
              <button
                key={decision}
                type="button"
                className={`button button--decision button--decision-${decision}`}
                disabled={busy || indeterminateDelivery}
                onClick={() => void decide(decision)}
              >
                {decisionLabels[decision]}
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
              placeholder={event.sensitive ? "••••••••" : "Type your answer…"}
              disabled={busy || indeterminateDelivery}
            />
            <button
              className={`button button--${offered.length > 0 ? "ghost" : "primary"}`}
              type="submit"
              disabled={busy || indeterminateDelivery}
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
