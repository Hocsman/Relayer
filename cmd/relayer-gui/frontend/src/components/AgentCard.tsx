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
  onResize(runID: string, sessionID: string, columns: number, rows: number): Promise<void>;
  onStop(runID: string, sessionID: string): Promise<void>;
  onOpenEvent(runID: string, sessionID: string, eventID: string): void;
  onSubmitLine(runID: string, sessionID: string, line: string): Promise<void>;
}

export function AgentCard({ runID, agent, event, onResize, onStop, onOpenEvent, onSubmitLine }: AgentCardProps) {
  const waiting = Boolean(event) || agent.status === "waiting";
  const inputRef = useRef<HTMLInputElement>(null);
  const submittingRef = useRef(false);
  const [submitting, setSubmitting] = useState(false);
  const inputDisabled = lineInputDisabled(agent, waiting, submitting);
  const inputIdentity = `${runID}\u0000${agent.sessionID}`;
  const previousInputIdentity = useRef<string>();

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
    if (!input || inputDisabled || submittingRef.current) return;
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
      className={`agent-card${waiting ? " agent-card--waiting" : ""}${agent.simulated ? " agent-card--simulated" : ""}`}
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

      <TerminalSnapshotView
        runID={runID}
        sessionID={agent.sessionID}
        label={`Output from ${agent.name}`}
        output={agent.output}
        revision={agent.revision}
        onResize={onResize}
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
          placeholder={waiting ? "Handle the pending request" : agent.inputFrozen ? "Session frozen" : "Text is never recorded"}
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
          {agent.running && (
            <button
              className="button button--ghost button--small"
              type="button"
              disabled={agent.status === "stopping"}
              onClick={() => void onStop(runID, agent.sessionID)}
            >
              Stop
            </button>
          )}
        </div>
      </footer>
    </article>
  );
}
