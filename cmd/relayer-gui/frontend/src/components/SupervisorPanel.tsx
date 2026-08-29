import { safeEventSummary } from "../lib/safety";
import { supervisionEventKey } from "../lib/eventKey";
import type { AppState, SafeErrorEvent, SupervisionEvent } from "../types/relayer";

interface SupervisorPanelProps {
  state: AppState;
  errors: SafeErrorEvent[];
  selectedEventKey?: string;
  onSelectEvent(runID: string, sessionID: string, eventID: string): void;
}

const actionLabels = { allow: "Allow", ask: "Ask", deny: "Deny" } as const;
const riskLabels = { low: "Low", unknown: "To check", high: "High" } as const;

export function SupervisorPanel({
  state,
  errors,
  selectedEventKey,
  onSelectEvent,
}: SupervisorPanelProps) {
  return (
    <aside className="supervisor" aria-label="Supervisor">
      <header className="supervisor__header">
        <div>
          <span className="eyebrow">Supervisor</span>
          <h2>Action queue</h2>
        </div>
        <span className={`queue-count${state.pendingEvents.length ? " queue-count--active" : ""}`}>
          {state.pendingEvents.length}
        </span>
        <span className="sr-only" aria-live="polite">
          {state.pendingEvents.length === 0
            ? "No supervision request is pending"
            : `${state.pendingEvents.length} supervision request${state.pendingEvents.length !== 1 ? "s" : ""} pending`}
        </span>
      </header>

      <div className="supervisor__body">
        {state.pendingEvents.length === 0 ? (
          <div className="all-clear">
            <span className="all-clear__icon" aria-hidden="true">✓</span>
            <strong>All clear</strong>
            <p>No human decision is pending.</p>
          </div>
        ) : (
          <div className="event-list">
            {state.pendingEvents.map((event) => (
              <EventItem
                key={supervisionEventKey(event.runID, event.sessionID, event.id)}
                event={event}
                agentName={state.agents.find((agent) => agent.sessionID === event.sessionID)?.name}
                selected={supervisionEventKey(event.runID, event.sessionID, event.id) === selectedEventKey}
                onSelect={() => onSelectEvent(event.runID, event.sessionID, event.id)}
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
              <dt>Policy</dt>
              <dd>{state.policy.defaultAction}</dd>
            </div>
            <div>
              <dt>Decisions</dt>
              <dd>{state.policy.dryRun ? "simulated" : "active"}</dd>
            </div>
          </dl>
        </section>

        {(state.notices?.length ?? 0) > 0 && (
          <section className="supervisor__section">
            <h3>Startup</h3>
            <ul className="notice-list">
              {state.notices?.map((notice, index) => (
                <li key={`${index}-${notice}`}>{notice}</li>
              ))}
            </ul>
          </section>
        )}

        {errors.length > 0 && (
          <section className="supervisor__section">
            <h3>Recent incidents</h3>
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
          {riskLabels[event.risk]} · {event.evaluation.ruleName || "default rule"} · {actionLabels[event.evaluation.action]}
        </span>
      </span>
      <span className="event-item__arrow" aria-hidden="true">›</span>
    </button>
  );
}
