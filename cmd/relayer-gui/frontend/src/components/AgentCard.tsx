import { useLayoutEffect, useRef, useState, type FormEvent } from "react";
import { StatusBadge } from "./StatusBadge";
import { TerminalSnapshotView } from "./TerminalSnapshotView";
import {
  discardUnavailableLine,
  lineInputDisabled,
  submitUncontrolledLine,
} from "../lib/lineInput";
import type { AgentState, SupervisionEvent } from "../types/relayer";

interface AgentCardProps {
  runID: string;
  agent: AgentState;
  event?: SupervisionEvent;
  readOnly?: boolean;
  onResize(runID: string, sessionID: string, columns: number, rows: number): Promise<void>;
  onStop(runID: string, sessionID: string): Promise<void>;
  onStart(runID: string, sessionID: string): Promise<void>;
  onRestart(runID: string, sessionID: string): Promise<void>;
  onOpenEvent(runID: string, sessionID: string, eventID: string): void;
  onSubmitLine(runID: string, sessionID: string, line: string): Promise<void>;
  onTerminalInput?(runID: string, sessionID: string, data: string): Promise<void> | void;
  onToggleInteractive?(runID: string, sessionID: string, active: boolean): Promise<void> | void;
}

export function AgentCard({
  runID,
  agent,
  event,
  readOnly,
  onResize,
  onStop,
  onStart,
  onRestart,
  onOpenEvent,
  onSubmitLine,
  onTerminalInput,
  onToggleInteractive,
}: AgentCardProps) {
  const waiting = Boolean(event) || agent.status === "waiting";
  const inputRef = useRef<HTMLInputElement>(null);
  const submittingRef = useRef(false);
  const [submitting, setSubmitting] = useState(false);
  const [localInteractive, setLocalInteractive] = useState(false);
  const isInteractive = (localInteractive || agent.attached) && agent.running && !readOnly;
  const inputDisabled = Boolean(readOnly) || isInteractive || lineInputDisabled(agent, waiting, submitting);
  const inputIdentity = `${runID}\u0000${agent.sessionID}`;
  const previousInputIdentity = useRef<string>();

  const handleToggleInteractive = async () => {
    const next = !isInteractive;
    setLocalInteractive(next);
    await onToggleInteractive?.(runID, agent.sessionID, next);
  };

  // Clear before paint whenever this DOM node becomes unusable or is rebound
  // to another run/session. A draft can therefore never survive a prompt,
  // freeze, attach, exit, stop or identity transition and be sent later.
  useLayoutEffect(() => {
    const identityChanged = previousInputIdentity.current !== inputIdentity;
    previousInputIdentity.current = inputIdentity;
    discardUnavailableLine(inputRef.current, inputDisabled, identityChanged);
  }, [inputDisabled, inputIdentity]);

  const submitLine = async (formEvent: FormEvent<HTMLFormElement>) => {
    formEvent.preventDefault();
    const input = inputRef.current;
    if (!input || inputDisabled || submittingRef.current || readOnly) return;
    submittingRef.current = true;
    setSubmitting(true);
    try {
      await submitUncontrolledLine(input, (line) => onSubmitLine(runID, agent.sessionID, line));
    } catch {
      // The hook has already emitted a static safe error and refreshed native
      // state. Never copy the rejected line or native error into this card.
    } finally {
      submittingRef.current = false;
      setSubmitting(false);
    }
  };
  return (
    <article
      className={`agent-card${waiting ? " agent-card--waiting" : ""}${agent.simulated ? " agent-card--simulated" : ""}${isInteractive ? " agent-card--interactive" : ""}`}
    >
      <header className="agent-card__header">
        <div className="agent-card__identity">
          <span className="agent-card__avatar" aria-hidden="true">
            {agent.name.trim().charAt(0).toUpperCase() || "A"}
          </span>
          <div className="agent-card__title">
            <h2>
              {agent.name}
              {agent.simulated && (
                <span
                  className="simulated-tag"
                  title="Demo Bash script substituted for a real agent. This panel supervises no coding tool."
                >
                  Simulated
                </span>
              )}
            </h2>
            <p title={agent.displayCommand}>{agent.displayCommand || agent.agentID}</p>
          </div>
        </div>
        <StatusBadge status={waiting ? "waiting" : agent.status} />
      </header>

      <div className="agent-card__meta" aria-label="Session information">
        <span>{agent.backend.toUpperCase()}</span>
        <span>{agent.adapter}</span>
        <span className="agent-card__session" title={agent.sessionID}>{agent.sessionID}</span>
        {typeof agent.exitCode === "number" && <span>exit {agent.exitCode}</span>}
      </div>

      {isInteractive && (
        <div className="agent-card__interactive-banner" role="status">
          <span className="agent-card__interactive-pulse" aria-hidden="true" />
          <span className="agent-card__interactive-text">
            Session interactive (PTY direct) — Saisie directe au clavier, touches fléchées et Ctrl+C
          </span>
          <button
            type="button"
            className="button button--ghost button--tiny"
            onClick={() => void handleToggleInteractive()}
            title="Rendre la main et quitter la session interactive"
          >
            Rendre la main
          </button>
        </div>
      )}

      <TerminalSnapshotView
        runID={runID}
        sessionID={agent.sessionID}
        label={`Output from ${agent.name}`}
        output={agent.output}
        revision={agent.revision}
        onResize={onResize}
        interactive={isInteractive}
        onTerminalInput={(data) => onTerminalInput?.(runID, agent.sessionID, data)}
      />

      <form className="agent-card__line-input" onSubmit={(event) => void submitLine(event)}>
        <label className="sr-only" htmlFor={`line-${runID}-${agent.sessionID}`}>
          Send a line to {agent.name}
        </label>
        <input
          ref={inputRef}
          id={`line-${runID}-${agent.sessionID}`}
          type="text"
          autoComplete="off"
          spellCheck={false}
          disabled={inputDisabled}
          placeholder={readOnly ? "Mode lecture seule (Viewer)" : isInteractive ? "Terminal interactif actif (tapez directement ci-dessus)" : waiting ? "Handle the pending request" : agent.inputFrozen ? "Session frozen" : "Text is never recorded"}
          title="One UTF-8 line, 4096 bytes maximum, no control character"
          aria-label={`Line for ${agent.name}`}
        />
        <button className="button button--ghost button--small" type="submit" disabled={inputDisabled}>
          {submitting ? "Sending…" : "Send"}
        </button>
      </form>

      <footer className="agent-card__footer">
        <span>
          {!agent.running
            ? agent.status === "failed"
              ? "Session failed"
              : "Session finished"
            : agent.simulated
              ? "Demo agent"
              : isInteractive
                ? "Session interactive directe"
                : agent.attached
                  ? "Session attached"
                  : "Supervision active"}
        </span>
        <div className="agent-card__actions">
          {event && (
            <button
              className="button button--attention button--small"
              type="button"
              onClick={() => onOpenEvent(event.runID, event.sessionID, event.id)}
            >
              Review
            </button>
          )}
          {!readOnly && agent.running && (
            <>
              <button
                className={`button button--small ${isInteractive ? "button--attention" : "button--ghost"}`}
                type="button"
                disabled={agent.status === "stopping"}
                title={isInteractive ? "Rendre la main (quitter le contrôle direct PTY)" : "Prendre la main sur le terminal (contrôle direct PTY)"}
                onClick={() => void handleToggleInteractive()}
              >
                {isInteractive ? "🔌 Rendre la main" : "⌨️ Prendre la main"}
              </button>
              <button
                className="button button--ghost button--small"
                type="button"
                disabled={agent.status === "stopping"}
                title="Stop this agent without interrupting the other agents"
                onClick={() => void onStop(runID, agent.sessionID)}
              >
                Stop
              </button>
              <button
                className="button button--ghost button--small"
                type="button"
                disabled={agent.status === "stopping" || Boolean(event)}
                title="Restart this agent in place with a fresh terminal; its pending request must be answered first"
                onClick={() => void onRestart(runID, agent.sessionID)}
              >
                Restart
              </button>
            </>
          )}
          {!readOnly && !agent.running && agent.status !== "stopping" && agent.status !== "starting" && (
            <button
              className="button button--ghost button--small"
              type="button"
              title="Start this agent again under the same configuration, without interrupting the other agents"
              onClick={() => void onStart(runID, agent.sessionID)}
            >
              Start
            </button>
          )}
        </div>
      </footer>
    </article>
  );
}
