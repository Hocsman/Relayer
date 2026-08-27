import { StatusBadge } from "./StatusBadge";
import { TerminalSnapshotView } from "./TerminalSnapshotView";
import type { AgentState, SupervisionEvent } from "../types/relayer";

interface AgentCardProps {
  runID: string;
  agent: AgentState;
  event?: SupervisionEvent;
  onResize(runID: string, sessionID: string, columns: number, rows: number): Promise<void>;
  onStop(runID: string, sessionID: string): Promise<void>;
  onOpenEvent(runID: string, sessionID: string, eventID: string): void;
}

export function AgentCard({ runID, agent, event, onResize, onStop, onOpenEvent }: AgentCardProps) {
  const waiting = Boolean(event) || agent.status === "waiting";
  return (
    <article className={`agent-card${waiting ? " agent-card--waiting" : ""}`}>
      <header className="agent-card__header">
        <div className="agent-card__identity">
          <span className="agent-card__avatar" aria-hidden="true">
            {agent.name.trim().charAt(0).toUpperCase() || "A"}
          </span>
          <div className="agent-card__title">
            <h2>{agent.name}</h2>
            <p title={agent.displayCommand}>{agent.displayCommand || agent.agentID}</p>
          </div>
        </div>
        <StatusBadge status={waiting ? "waiting" : agent.status} />
      </header>

      <div className="agent-card__meta" aria-label="Informations de session">
        <span>{agent.backend.toUpperCase()}</span>
        <span>{agent.adapter}</span>
        <span className="agent-card__session" title={agent.sessionID}>{agent.sessionID}</span>
        {typeof agent.exitCode === "number" && <span>exit {agent.exitCode}</span>}
      </div>

      <TerminalSnapshotView
        runID={runID}
        sessionID={agent.sessionID}
        label={`Sortie de ${agent.name}`}
        output={agent.output}
        revision={agent.revision}
        onResize={onResize}
      />

      <footer className="agent-card__footer">
        <span>{agent.attached ? "Session attachée" : "Supervision active"}</span>
        <div className="agent-card__actions">
          {event && (
            <button
              className="button button--attention button--small"
              type="button"
              onClick={() => onOpenEvent(event.runID, event.sessionID, event.id)}
            >
              Examiner
            </button>
          )}
          {agent.running && (
            <button
              className="button button--ghost button--small"
              type="button"
              onClick={() => void onStop(runID, agent.sessionID)}
            >
              Arrêter
            </button>
          )}
        </div>
      </footer>
    </article>
  );
}
