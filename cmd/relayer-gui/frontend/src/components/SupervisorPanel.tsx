import { safeEventSummary } from "../lib/safety";
import { supervisionEventKey } from "../lib/eventKey";
import type { AppState, SafeErrorEvent, SupervisionEvent } from "../types/relayer";

interface SupervisorPanelProps {
  state: AppState;
  errors: SafeErrorEvent[];
  selectedEventKey?: string;
  onSelectEvent(sessionID: string, eventID: string): void;
}

const actionLabels = { allow: "Autoriser", ask: "Demander", deny: "Refuser" } as const;
const riskLabels = { low: "Faible", unknown: "À vérifier", high: "Élevé" } as const;

export function SupervisorPanel({
  state,
  errors,
  selectedEventKey,
  onSelectEvent,
}: SupervisorPanelProps) {
  return (
    <aside className="supervisor" aria-label="Superviseur">
      <header className="supervisor__header">
        <div>
          <span className="eyebrow">Superviseur</span>
          <h2>File d’intervention</h2>
        </div>
        <span className={`queue-count${state.pendingEvents.length ? " queue-count--active" : ""}`}>
          {state.pendingEvents.length}
        </span>
      </header>

      <div className="supervisor__body">
        {state.pendingEvents.length === 0 ? (
          <div className="all-clear">
            <span className="all-clear__icon" aria-hidden="true">✓</span>
            <strong>Tout est sous contrôle</strong>
            <p>Aucune validation humaine en attente.</p>
          </div>
        ) : (
          <div className="event-list">
            {state.pendingEvents.map((event) => (
              <EventItem
                key={event.id}
                event={event}
                agentName={state.agents.find((agent) => agent.sessionID === event.sessionID)?.name}
                selected={supervisionEventKey(event.sessionID, event.id) === selectedEventKey}
                onSelect={() => onSelectEvent(event.sessionID, event.id)}
              />
            ))}
          </div>
        )}

        <section className="supervisor__section">
          <h3>Protection</h3>
          <dl className="protection-grid">
            <div>
              <dt>Audit</dt>
              <dd className={`health health--${state.audit.status}`}>{state.audit.status}</dd>
            </div>
            <div>
              <dt>Mode</dt>
              <dd>{state.audit.mode}</dd>
            </div>
            <div>
              <dt>Politique</dt>
              <dd>{state.policy.defaultAction}</dd>
            </div>
            <div>
              <dt>Décisions</dt>
              <dd>{state.policy.dryRun ? "simulation" : "actives"}</dd>
            </div>
          </dl>
        </section>

        {errors.length > 0 && (
          <section className="supervisor__section">
            <h3>Incidents récents</h3>
            <div className="error-list">
              {errors.slice(0, 4).map((error, index) => (
                <div className="safe-error" key={`${error.timestamp}-${error.code}-${index}`}>
                  <strong>{error.code}</strong>
                  <p>{error.message}</p>
                </div>
              ))}
            </div>
          </section>
        )}
      </div>
    </aside>
  );
}

function EventItem({
  event,
  agentName,
  selected,
  onSelect,
}: {
  event: SupervisionEvent;
  agentName?: string;
  selected: boolean;
  onSelect(): void;
}) {
  return (
    <button
      type="button"
      className={`event-item${selected ? " event-item--selected" : ""}`}
      onClick={onSelect}
    >
      <span className={`risk-dot risk-dot--${event.risk}`} aria-hidden="true" />
      <span className="event-item__content">
        <strong>{safeEventSummary(event)}</strong>
        <span>{agentName || event.agentID} · {event.adapter}</span>
        <span className="event-item__policy">
          {riskLabels[event.risk]} · {event.evaluation.ruleName || "règle par défaut"} · {actionLabels[event.evaluation.action]}
        </span>
      </span>
      <span className="event-item__arrow" aria-hidden="true">›</span>
    </button>
  );
}
