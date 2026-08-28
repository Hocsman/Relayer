import { useEffect, useRef, useState } from "react";
import { AgentGrid } from "./components/AgentGrid";
import { AgentSettingsPanel } from "./components/AgentSettingsPanel";
import { DecisionModal } from "./components/DecisionModal";
import { PreflightPanel } from "./components/PreflightPanel";
import { SupervisorPanel } from "./components/SupervisorPanel";
import { TopBar } from "./components/TopBar";
import { useRelayer } from "./hooks/useRelayer";
import { supervisionEventKey } from "./lib/eventKey";
import type { AppState, RelayerBridge, RunStatus } from "./types/relayer";

export function App({ bridge }: { bridge: RelayerBridge }) {
  const {
    state,
    submitDecision,
    submitAutomaticDecision,
    submitLine,
    resizeSession,
    stopSession,
    saveAgentProfiles,
    saveAgentProfilesAndRestart,
    stopRun,
  } = useRelayer(bridge);
  const [selectedEventKey, setSelectedEventKey] = useState<string>();
  const [modalOpen, setModalOpen] = useState(false);
  const [agentsOpen, setAgentsOpen] = useState(false);
  const [preflightOpen, setPreflightOpen] = useState(false);
  const [stopConfirmation, setStopConfirmation] = useState(false);
  const seenEvents = useRef(new Set<string>());

  useEffect(() => {
    seenEvents.current.clear();
    setSelectedEventKey(undefined);
    setModalOpen(false);
    setStopConfirmation(false);
  }, [state.app?.runID]);

  useEffect(() => {
    const pending = state.app?.pendingEvents ?? [];
    if (agentsOpen || preflightOpen || state.app?.runStatus !== "running") {
      setModalOpen(false);
      return;
    }
    if (pending.length === 0) {
      setSelectedEventKey(undefined);
      setModalOpen(false);
      return;
    }

    if (
      !selectedEventKey ||
      !pending.some(
        (event) =>
          supervisionEventKey(event.runID, event.sessionID, event.id) === selectedEventKey,
      )
    ) {
      setSelectedEventKey(
        supervisionEventKey(pending[0].runID, pending[0].sessionID, pending[0].id),
      );
      setModalOpen(true);
    }
    const unseen = pending.find(
      (event) =>
        !seenEvents.current.has(
          supervisionEventKey(event.runID, event.sessionID, event.id),
        ),
    );
    if (unseen) {
      const key = supervisionEventKey(unseen.runID, unseen.sessionID, unseen.id);
      seenEvents.current.add(key);
      setSelectedEventKey(key);
      setModalOpen(true);
    }
  }, [agentsOpen, preflightOpen, state.app?.pendingEvents, state.app?.runStatus, selectedEventKey]);

  if (state.connection === "loading" || !state.app) {
    if (state.connection === "failed") {
      return <StartupFailure message={state.fatalError || "The Relayer engine is not responding."} />;
    }
    return <StartupScreen />;
  }

  const selectedEvent = state.app.pendingEvents.find(
    (event) =>
      supervisionEventKey(event.runID, event.sessionID, event.id) === selectedEventKey,
  );
  const selectedAgent = state.app.agents.find(
    (agent) => agent.sessionID === selectedEvent?.sessionID,
  );

  const openEvent = (runID: string, sessionID: string, eventID: string) => {
    if (state.app?.runStatus !== "running" || runID !== state.app.runID) return;
    setSelectedEventKey(supervisionEventKey(runID, sessionID, eventID));
    setModalOpen(true);
  };

  const transitioning = isTransitioning(state.app.runStatus);
  const hasDashboard = state.app.runStatus === "running";

  return (
    <div className="application-shell">
      <TopBar
        state={state.app}
        onOpenAgents={() => {
          setPreflightOpen(false);
          setAgentsOpen(true);
        }}
        onOpenPreflight={() => {
          setAgentsOpen(false);
          setPreflightOpen(true);
        }}
        onRequestStop={() => setStopConfirmation(true)}
      />
      {hasDashboard ? (
        <main className="workspace">
          <AgentGrid
            runID={state.app.runID}
            agents={state.app.agents}
            events={state.app.pendingEvents}
            onResize={resizeSession}
            onStop={stopSession}
            onOpenEvent={openEvent}
            onSubmitLine={submitLine}
          />
          <SupervisorPanel
            state={state.app}
            errors={state.errors}
            selectedEventKey={selectedEventKey}
            onSelectEvent={openEvent}
          />
        </main>
      ) : (
        <RunWorkspace
          status={state.app.runStatus}
          errors={state.errors.map((error) => error.message)}
          onConfigure={() => setAgentsOpen(true)}
          onOpenPreflight={() => setPreflightOpen(true)}
        />
      )}
      <DecisionModal
        event={!agentsOpen && !preflightOpen && !transitioning && modalOpen ? selectedEvent : undefined}
        agent={selectedAgent}
        queueSize={state.app.pendingEvents.length}
        onClose={() => setModalOpen(false)}
        onSubmit={submitDecision}
        onDecide={submitAutomaticDecision}
      />
      {agentsOpen && (
        <AgentSettingsPanel
          bridge={bridge}
          runID={state.app.runID}
          runStatus={state.app.runStatus}
          pendingEvents={state.app.pendingEvents}
          onSave={saveAgentProfiles}
          onSaveAndRestart={saveAgentProfilesAndRestart}
          onClose={() => setAgentsOpen(false)}
        />
      )}
      {preflightOpen && (
        <PreflightPanel bridge={bridge} onClose={() => setPreflightOpen(false)} />
      )}
      {stopConfirmation && (
        <StopRunConfirmation
          state={state.app}
          onCancel={() => setStopConfirmation(false)}
          onConfirm={() => {
            const runID = state.app?.runID;
            setStopConfirmation(false);
            if (runID) void stopRun(runID);
          }}
        />
      )}
    </div>
  );
}

function isTransitioning(status: RunStatus): boolean {
  return status === "starting" || status === "restarting" || status === "rollback" || status === "stopping";
}

function RunWorkspace({
  status,
  errors,
  onConfigure,
  onOpenPreflight,
}: {
  status: RunStatus;
  errors: string[];
  onConfigure(): void;
  onOpenPreflight(): void;
}) {
  const transitionCopy: Partial<Record<RunStatus, { title: string; detail: string }>> = {
    starting: {
      title: "Starting the agents…",
      detail: "Relayer validates the configuration, the audit journal and each backend before opening the run.",
    },
    restarting: {
      title: "Secure restart…",
      detail: "Decisions are locked during the clean stop and the start of the new run.",
    },
    rollback: {
      title: "Restoring…",
      detail: "The new start failed. Relayer is trying to restore the previously active configuration.",
    },
    stopping: {
      title: "Stopping the run…",
      detail: "Accepted deliveries are drained before the backends and the audit journal close.",
    },
  };
  const transition = transitionCopy[status];
  if (transition) {
    return (
      <main className="run-workspace run-workspace--transition" aria-live="polite">
        <span className="settings-spinner" aria-hidden="true" />
        <span className="eyebrow">Lifecycle locked</span>
        <h1>{transition.title}</h1>
        <p>{transition.detail}</p>
      </main>
    );
  }

  const failed = status === "failed";
  return (
    <main className={`run-workspace${failed ? " run-workspace--failed" : ""}`}>
      <span className="run-workspace__mark" aria-hidden="true">{failed ? "!" : "◇"}</span>
      <span className="eyebrow">{failed ? "Run unavailable" : "No active run"}</span>
      <h1>{failed ? "The start or stop did not complete" : "Configure your agent team"}</h1>
      <p>
        {failed
          ? "Decisions stay blocked. Check the configuration before retrying; no simulation mode is enabled automatically."
          : "Save the profiles, then start the agents without closing Relayer."}
      </p>
      {failed && errors[0] && <strong className="run-workspace__error">{errors[0]}</strong>}
      <div className="run-workspace__actions">
        <button className="button button--primary" type="button" onClick={onConfigure}>
          {failed ? "Open the configuration" : "Configure the agents"}
        </button>
        <button className="button button--ghost" type="button" onClick={onOpenPreflight}>
          Check the installation
        </button>
      </div>
    </main>
  );
}

function StopRunConfirmation({
  state,
  onCancel,
  onConfirm,
}: {
  state: AppState;
  onCancel(): void;
  onConfirm(): void;
}) {
  const running = state.agents.filter((agent) => agent.running).length;
  const pending = state.pendingEvents.length;
  return (
    <div className="modal-layer" role="presentation">
      <section className="lifecycle-confirmation" role="alertdialog" aria-modal="true" aria-labelledby="stop-run-title">
        <span className="eyebrow">Action on the current run</span>
        <h2 id="stop-run-title">Stop the run?</h2>
        <p>
          {running} agent{running !== 1 ? "s" : ""} will be stopped outright. tmux persistence is ignored for this explicit stop.
          {pending > 0 ? ` ${pending} pending supervision request${pending > 1 ? "s" : ""} will be cancelled.` : ""}
        </p>
        <div className="lifecycle-confirmation__actions">
          <button className="button button--ghost" type="button" onClick={onCancel}>Cancel</button>
          <button className="button button--danger" type="button" onClick={onConfirm}>Stop the agents</button>
        </div>
      </section>
    </div>
  );
}

export function StartupFailure({ message }: { message: string }) {
  return (
    <main className="startup startup--failed">
      <span className="startup__mark">R</span>
      <h1>Cannot connect to the engine</h1>
      <p>{message}</p>
      <small>Demo mode is never enabled automatically in a packaged application.</small>
    </main>
  );
}

function StartupScreen() {
  return (
    <main className="startup">
      <span className="startup__mark startup__mark--pulse">R</span>
      <h1>Relayer</h1>
      <p>Initializing the control plane…</p>
    </main>
  );
}
