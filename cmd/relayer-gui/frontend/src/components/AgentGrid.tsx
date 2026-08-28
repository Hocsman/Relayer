import { AgentCard } from "./AgentCard";
import type { AgentState, SupervisionEvent } from "../types/relayer";

interface AgentGridProps {
  runID: string;
  agents: AgentState[];
  events: SupervisionEvent[];
  onResize(runID: string, sessionID: string, columns: number, rows: number): Promise<void>;
  onStop(runID: string, sessionID: string): Promise<void>;
  onOpenEvent(runID: string, sessionID: string, eventID: string): void;
  onSubmitLine(runID: string, sessionID: string, line: string): Promise<void>;
}

export function AgentGrid({ runID, agents, events, onResize, onStop, onOpenEvent, onSubmitLine }: AgentGridProps) {
  if (agents.length === 0) {
    return (
      <section className="empty-agents">
        <span className="empty-agents__orb" aria-hidden="true" />
        <h2>No active agent</h2>
        <p>Add an agent in the configuration, then restart Relayer.</p>
      </section>
    );
  }
  return (
    <section className="agent-grid" aria-label="Supervised agents">
      {agents.map((agent) => (
        <AgentCard
          key={`${runID}\u0000${agent.sessionID}`}
          runID={runID}
          agent={agent}
          event={events.find(
            (event) => event.runID === runID && event.sessionID === agent.sessionID,
          )}
          onResize={onResize}
          onStop={onStop}
          onOpenEvent={onOpenEvent}
          onSubmitLine={onSubmitLine}
        />
      ))}
    </section>
  );
}
