import { FormEvent, useEffect, useRef, useState } from "react";
import { safeEventSummary } from "../lib/safety";
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
  allow: "Autoriser",
  deny: "Refuser",
};

export function DecisionModal({ event, agent, queueSize, onClose, onSubmit, onDecide }: DecisionModalProps) {
  const inputRef = useRef<HTMLInputElement>(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    if (!event) return;
    if (inputRef.current) {
      inputRef.current.value = "";
      inputRef.current.focus();
    }
  }, [event?.runID, event?.sessionID, event?.id]);

  useEffect(() => {
    if (!event) return;
    const onKeyDown = (keyboardEvent: KeyboardEvent) => {
      if (keyboardEvent.key === "Escape" && !busy) onClose();
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [event, busy, onClose]);

  if (!event) return null;

  const indeterminateDelivery = deliveryRequiresResync(event);
  // Only what the adapter reported for this exact occurrence. An unknown value
  // arriving from a stale bridge is dropped rather than rendered as a button.
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
        className={`decision-modal${event.sensitive ? " decision-modal--sensitive" : ""}`}
        role="dialog"
        aria-modal="true"
        aria-labelledby="decision-title"
      >
        <div className="decision-modal__glow" aria-hidden="true" />
        <header className="decision-modal__header">
          <div className="decision-modal__signal" aria-hidden="true">!</div>
          <div>
            <span className="eyebrow">Action humaine requise</span>
            <h2 id="decision-title">{safeEventSummary(event)}</h2>
          </div>
          <button className="icon-button" type="button" onClick={onClose} disabled={busy} aria-label="Réduire">
            ×
          </button>
        </header>

        <div className="decision-context">
          <div>
            <span>Agent</span>
            <strong>{agent?.name || event.agentID}</strong>
            {agent?.simulated && (
              <em
                className="simulated-tag"
                title="Script Bash de démonstration substitué à un vrai agent."
              >
                Simulé
              </em>
            )}
          </div>
          <div><span>Adaptateur</span><strong>{event.adapter}</strong></div>
          <div><span>Risque</span><strong className={`risk-text risk-text--${event.risk}`}>{event.risk}</strong></div>
          <div><span>Règle</span><strong>{event.evaluation.ruleName || "Défaut sûr"}</strong></div>
          <div><span>Action</span><strong>{event.evaluation.action}</strong></div>
          <div><span>Livraison</span><strong>{event.deliveryStatus}</strong></div>
        </div>

        {event.evaluation.dryRun && (
          <p className="dry-run-notice">DRY RUN · La décision reste entièrement manuelle.</p>
        )}

        {indeterminateDelivery && (
          <p className="delivery-lock" role="alert">
            État indéterminé — arrêter ou resynchroniser la session. Aucune nouvelle saisie ne sera envoyée.
          </p>
        )}

        {offered.length > 0 && (
          <div className="decision-actions">
            {offered.map((decision) => (
              <button
                key={decision}
                type="button"
                className={`button button--${decision === "deny" ? "danger" : "primary"}`}
                disabled={busy || indeterminateDelivery}
                onClick={() => void decide(decision)}
              >
                {decisionLabels[decision]}
              </button>
            ))}
            <span>Réponse encodée par l’adaptateur {event.adapter}.</span>
          </div>
        )}

        <form className="decision-form" onSubmit={(formEvent) => void submit(formEvent)}>
          <label htmlFor="manual-decision">
            {event.sensitive
              ? "Valeur confidentielle"
              : offered.length > 0
                ? "Ou répondre manuellement"
                : "Réponse à transmettre"}
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
              placeholder={event.sensitive ? "••••••••" : "Saisissez votre réponse…"}
              disabled={busy || indeterminateDelivery}
            />
            <button className="button button--primary" type="submit" disabled={busy || indeterminateDelivery}>
              {busy ? "Transmission…" : "Transmettre"}
            </button>
          </div>
          <p>
            {event.sensitive
              ? "La valeur est masquée, transmise directement et jamais ajoutée aux logs de l’interface."
              : "La réponse est envoyée à cette occurrence exacte du prompt."}
          </p>
        </form>

        <footer className="decision-modal__footer">
          <span>{event.sensitive ? "Événement sensible" : `Événement ${event.id}`}</span>
          {queueSize > 1 && <span>{queueSize - 1} autre{queueSize > 2 ? "s" : ""} en attente</span>}
        </footer>
      </section>
    </div>
  );
}
