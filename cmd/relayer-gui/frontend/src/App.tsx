import { useEffect, useRef, useState } from "react";
import { AgentGrid } from "./components/AgentGrid";
import { DecisionModal } from "./components/DecisionModal";
import { SupervisorPanel } from "./components/SupervisorPanel";
import { TopBar } from "./components/TopBar";
import { useRelayer } from "./hooks/useRelayer";
import { supervisionEventKey } from "./lib/eventKey";
import type { RelayerBridge } from "./types/relayer";

export function App({ bridge }: { bridge: RelayerBridge }) {
  const { state, submitDecision, resizeSession, stopSession, shutdown } = useRelayer(bridge);
  const [selectedEventKey, setSelectedEventKey] = useState<string>();
  const [modalOpen, setModalOpen] = useState(false);
  const seenEvents = useRef(new Set<string>());

  useEffect(() => {
    const pending = state.app?.pendingEvents ?? [];
    if (pending.length === 0) {
      setSelectedEventKey(undefined);
      setModalOpen(false);
      return;
    }

    if (
      !selectedEventKey ||
      !pending.some(
        (event) => supervisionEventKey(event.sessionID, event.id) === selectedEventKey,
      )
    ) {
      setSelectedEventKey(supervisionEventKey(pending[0].sessionID, pending[0].id));
      setModalOpen(true);
    }
    const unseen = pending.find(
      (event) => !seenEvents.current.has(supervisionEventKey(event.sessionID, event.id)),
    );
    if (unseen) {
      const key = supervisionEventKey(unseen.sessionID, unseen.id);
      seenEvents.current.add(key);
      setSelectedEventKey(key);
      setModalOpen(true);
    }
  }, [state.app?.pendingEvents, selectedEventKey]);

  if (state.connection === "loading" || !state.app) {
    if (state.connection === "failed") {
      return <StartupFailure message={state.fatalError || "Le moteur Relayer ne répond pas."} />;
    }
    return <StartupScreen />;
  }

  const selectedEvent = state.app.pendingEvents.find(
    (event) => supervisionEventKey(event.sessionID, event.id) === selectedEventKey,
  );
  const selectedAgent = state.app.agents.find(
    (agent) => agent.sessionID === selectedEvent?.sessionID,
  );

  const openEvent = (sessionID: string, eventID: string) => {
    setSelectedEventKey(supervisionEventKey(sessionID, eventID));
    setModalOpen(true);
  };

  return (
    <div className="application-shell">
      <TopBar state={state.app} onShutdown={shutdown} />
      <main className="workspace">
        <AgentGrid
          agents={state.app.agents}
          events={state.app.pendingEvents}
          onResize={resizeSession}
          onStop={stopSession}
          onOpenEvent={openEvent}
        />
        <SupervisorPanel
          state={state.app}
          errors={state.errors}
          selectedEventKey={selectedEventKey}
          onSelectEvent={openEvent}
        />
      </main>
      <DecisionModal
        event={modalOpen ? selectedEvent : undefined}
        agent={selectedAgent}
        queueSize={state.app.pendingEvents.length}
        onClose={() => setModalOpen(false)}
        onSubmit={submitDecision}
      />
    </div>
  );
}

export function StartupFailure({ message }: { message: string }) {
  return (
    <main className="startup startup--failed">
      <span className="startup__mark">R</span>
      <h1>Connexion au moteur impossible</h1>
      <p>{message}</p>
      <small>Le mode démo n’est jamais activé automatiquement dans une application empaquetée.</small>
    </main>
  );
}

function StartupScreen() {
  return (
    <main className="startup">
      <span className="startup__mark startup__mark--pulse">R</span>
      <h1>Relayer</h1>
      <p>Initialisation du plan de contrôle…</p>
    </main>
  );
}
