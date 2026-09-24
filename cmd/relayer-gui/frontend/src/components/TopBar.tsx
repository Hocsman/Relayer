import { awaitsPerson } from "../lib/delivery";
import type { AppState, UserInfo } from "../types/relayer";

interface TopBarProps {
  state: AppState;
  userInfo?: UserInfo;
  onOpenAgents(): void;
  onOpenPreflight(): void;
  onOpenAudit(): void;
  onOpenObservability(): void;
  onOpenRecordings(): void;
  onRequestStop(): void;
}

const runLabels: Record<AppState["runStatus"], string> = {
  idle: "Ready to start",
  starting: "Initializing",
  running: "Running",
  restarting: "Restarting",
  rollback: "Rolling back",
  stopping: "Stopping",
  stopped: "Stopped",
  failed: "Error",
};

export function TopBar({ state, userInfo, onOpenAgents, onOpenPreflight, onOpenAudit, onOpenObservability, onOpenRecordings, onRequestStop }: TopBarProps) {
  const running = state.agents.filter((agent) => agent.running).length;
  // Requests waiting for a person, not every pending prompt: the policy's own
  // answers in progress are listed in the supervisor panel instead.
  const waiting = state.pendingEvents.filter(awaitsPerson).length;
  const transitioning = ["starting", "restarting", "rollback", "stopping"].includes(
    state.runStatus,
  );
  const hasRun = state.runStatus !== "idle";
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

      <div className="topbar__metrics" role="group" aria-label="Run state">
        <span className={`run-state run-state--${state.runStatus}`}>
          <i aria-hidden="true" />
          {runLabels[state.runStatus]}
        </span>
        {hasRun && (
          <>
            <span className="topbar__metric"><strong>{running}</strong> active</span>
            <span className={`topbar__metric${waiting ? " topbar__metric--attention" : ""}`}>
              <strong>{waiting}</strong> pending
            </span>
          </>
        )}
      </div>

      <div className="topbar__actions">
        {userInfo?.readOnly && (
          <span
            className="badge badge--viewer"
            title={`Signed in as ${userInfo.identity} (read-only)`}
          >
            VIEWER (READ-ONLY)
          </span>
        )}
        {state.policy.dryRun && <span className="mode-pill">DRY RUN</span>}
        <span className="mode-pill mode-pill--quiet">POLICY {state.policy.defaultAction.toUpperCase()}</span>
        <button
          className="button button--health"
          type="button"
          onClick={onOpenPreflight}
          disabled={transitioning}
        >
          <span aria-hidden="true">＋</span> Health
        </button>
        <button
          className="button button--agents"
          type="button"
          onClick={onOpenAgents}
          disabled={transitioning}
        >
          <span aria-hidden="true">◇</span> Agents
        </button>
        <button
          className="button button--audit"
          type="button"
          onClick={onOpenAudit}
          disabled={transitioning}
        >
          <span aria-hidden="true">📜</span> Audit
        </button>
        <button
          className="button button--metrics"
          type="button"
          onClick={onOpenObservability}
          disabled={transitioning}
        >
          <span aria-hidden="true">📊</span> Metrics
        </button>
        <button
          className="button button--recordings"
          type="button"
          onClick={onOpenRecordings}
          disabled={transitioning}
        >
          <span aria-hidden="true">⏺</span> Recordings
        </button>
        {state.runID && state.runStatus !== "idle" && !userInfo?.readOnly && (
          <button
            className="button button--ghost"
            type="button"
            disabled={transitioning}
            onClick={onRequestStop}
          >
            {state.runStatus === "failed" ? "Retry the stop" : "Stop the run"}
          </button>
        )}
      </div>
    </header>
  );
}
