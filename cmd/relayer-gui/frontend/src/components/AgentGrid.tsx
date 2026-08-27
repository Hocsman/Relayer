import { AgentCard } from "./AgentCard";
import type { AgentState, SupervisionEvent } from "../types/relayer";

interface AgentGridProps {
  agents: AgentState[];
  events: SupervisionEvent[];
  onResize(sessionID: string, columns: number, rows: number): Promise<void>;
  onStop(sessionID: string): Promise<void>;
  onOpenEvent(sessionID: string, eventID: string): void;
}

export function AgentGrid({ agents, events, onResize, onStop, onOpenEvent }: AgentGridProps) {
  if (agents.length === 0) {
    return (
      <section className="empty-agents">
        <span className="empty-agents__orb" aria-hidden="true" />
        <h2>Aucun agent actif</h2>
        <p>Ajoutez un agent dans la configuration puis redémarrez Relayer.</p>
      </section>
    );
  }
  return (
    <section className="agent-grid" aria-label="Agents supervisés">
      {agents.map((agent) => (
        <AgentCard
          key={agent.sessionID}
          agent={agent}
          event={events.find((event) => event.sessionID === agent.sessionID)}
          onResize={onResize}
          onStop={onStop}
          onOpenEvent={onOpenEvent}
        />
      ))}
    </section>
  );
}
