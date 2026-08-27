import type { AppState } from "../types/relayer";

interface TopBarProps {
  state: AppState;
  onOpenAgents(): void;
  onShutdown(): Promise<void>;
}

const runLabels: Record<AppState["runStatus"], string> = {
  starting: "Initialisation",
  running: "En cours",
  stopping: "Arrêt en cours",
  stopped: "Arrêté",
  failed: "Erreur",
};

export function TopBar({ state, onOpenAgents, onShutdown }: TopBarProps) {
  const running = state.agents.filter((agent) => agent.running).length;
  const waiting = state.pendingEvents.length;
  return (
    <header className="topbar">
      <div className="brand">
        <span className="brand__mark" aria-hidden="true">
          <span />
          <span />
        </span>
        <div>
          <strong>Relayer</strong>
          <span>Human control plane</span>
        </div>
      </div>

      <div className="topbar__metrics" aria-label="État du run">
        <span className={`run-state run-state--${state.runStatus}`}>
          <i aria-hidden="true" />
          {runLabels[state.runStatus]}
        </span>
        <span className="topbar__metric"><strong>{running}</strong> actif{running > 1 ? "s" : ""}</span>
        <span className={`topbar__metric${waiting ? " topbar__metric--attention" : ""}`}>
          <strong>{waiting}</strong> en attente
        </span>
      </div>

      <div className="topbar__actions">
        {state.policy.dryRun && <span className="mode-pill">DRY RUN</span>}
        <span className="mode-pill mode-pill--quiet">POLICY {state.policy.defaultAction.toUpperCase()}</span>
        <button className="button button--agents" type="button" onClick={onOpenAgents}>
          <span aria-hidden="true">◇</span> Agents
        </button>
        <button className="button button--ghost" type="button" onClick={() => void onShutdown()}>
          Arrêter
        </button>
      </div>
    </header>
  );
}
