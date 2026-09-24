import { useCallback, useEffect, useReducer } from "react";
import { decisionFailure } from "../lib/delivery";
import { safeError } from "../lib/safety";
import {
  initialRelayerState,
  relayerReducer,
} from "../state/relayerState";
import type {
  HandView,
  LifecycleResult,
  RelayerBridge,
  SafeErrorEvent,
  SaveAgentProfilesAndRestartRequest,
  SaveAgentProfilesRequest,
  SemanticDecision,
} from "../types/relayer";

function localError(
  runID: string,
  code: string,
  message: string,
  sessionID?: string,
): SafeErrorEvent {
  return {
    runID,
    code,
    message,
    sessionID,
    timestamp: new Date().toISOString(),
  };
}

export function useRelayer(bridge: RelayerBridge) {
  const [state, dispatch] = useReducer(relayerReducer, initialRelayerState);

  const refresh = useCallback(async () => {
    try {
      dispatch({ type: "loaded", state: await bridge.getState() });
    } catch (error) {
      dispatch({ type: "loadFailed", message: safeError(error, "Relayer could not be loaded.") });
    }
  }, [bridge]);

  useEffect(() => {
    const disposers = [
      bridge.on("relayer:snapshot", (snapshot) => dispatch({ type: "snapshot", snapshot })),
      bridge.on("relayer:event", (event) => dispatch({ type: "event", event })),
      bridge.on("relayer:status", (status) => {
        dispatch({ type: "status", status });
        // A run-scope status is a lifecycle step (a start, a restart, a
        // rollback, a stop), after which every prompt, agent and hand may be
        // different. The status itself carries none of that, so take it all.
        if (status.scope === "run") void refresh();
      }),
      bridge.on("relayer:error", (error) => dispatch({ type: "error", error })),
      bridge.on("relayer:presence", (presence) => dispatch({ type: "presence", presence })),
      bridge.on("relayer:hand", (hand) => dispatch({ type: "hand", hand })),
    ];
    // The gateway's socket can drop. Whatever it broadcast meanwhile is lost,
    // a "delivered" included, which would leave an answered prompt on screen.
    const stopReconnect = bridge.onReconnect?.(() => void refresh());
    void refresh();
    return () => {
      disposers.forEach((dispose) => dispose());
      stopReconnect?.();
    };
  }, [bridge, refresh]);

  // The server is the authority on every prompt: it delivers the policy's own
  // answers, marks each prompt delivering, delivered, failed or uncertain, and
  // refuses an answer with a typed error. So a failed answer is never turned
  // into a local verdict. Most refusals send nothing at all (another answer
  // was already in flight, the prompt is gone, the journal is down, the run
  // stopped), and locking those as "uncertain" froze a prompt the server was
  // happily delivering, with no way for the operator to clear it.
  //
  // Instead the operator reads a fixed message for the refusal, and the page
  // takes the server's state again. A delivery the server itself found
  // indeterminate comes back as "uncertain" and stays locked; one it never
  // received comes back answerable. If the state cannot be read either, the
  // prompt keeps the "delivering" it was given below, which is locked, until
  // a reconnection or the next frame says what became of it.
  const answerFailed = useCallback(
    async (runID: string, sessionID: string, error: unknown) => {
      const failure = decisionFailure(error);
      dispatch({
        type: "error",
        error: localError(runID, failure.code, failure.message, sessionID),
      });
      await refresh();
    },
    [refresh],
  );

  const submitDecision = useCallback(
    async (runID: string, sessionID: string, eventID: string, value: string) => {
      dispatch({ type: "delivery", runID, sessionID, eventID, status: "delivering" });
      try {
        await bridge.submitDecision(runID, sessionID, eventID, value);
      } catch (error) {
        await answerFailed(runID, sessionID, error);
        return false;
      }
      dispatch({ type: "delivery", runID, sessionID, eventID, status: "delivered" });
      await refresh();
      return true;
    },
    [bridge, refresh, answerFailed],
  );

  // The semantic path follows the same rule as the typed one.
  const submitAutomaticDecision = useCallback(
    async (
      runID: string,
      sessionID: string,
      eventID: string,
      decision: SemanticDecision,
    ) => {
      dispatch({ type: "delivery", runID, sessionID, eventID, status: "delivering" });
      try {
        await bridge.submitAutomaticDecision(runID, sessionID, eventID, decision);
      } catch (error) {
        await answerFailed(runID, sessionID, error);
        return false;
      }
      dispatch({ type: "delivery", runID, sessionID, eventID, status: "delivered" });
      await refresh();
      return true;
    },
    [bridge, refresh, answerFailed],
  );

  const resizeSession = useCallback(
    async (runID: string, sessionID: string, columns: number, rows: number) => {
      try {
        await bridge.resizeSession(runID, sessionID, columns, rows);
      } catch {
        dispatch({
          type: "error",
          error: localError(
            runID,
            "resize_failed",
            "Resizing the session failed.",
            sessionID,
          ),
        });
      }
    },
    [bridge],
  );

  const submitLine = useCallback(
    async (runID: string, sessionID: string, line: string) => {
      try {
        const pending = bridge.submitLine(runID, sessionID, line);
        line = "";
        await pending;
      } catch {
        line = "";
        // Native errors are deliberately not reflected: they could retain a
        // transport wrapper. The UI only creates this static, content-free
        // message and then reloads the authoritative frozen/prompt state.
        dispatch({
          type: "error",
          error: localError(
            runID,
            "line_delivery_rejected",
            "The line could not be confirmed. Check the supervisor and the session state.",
            sessionID,
          ),
        });
        await refresh();
        throw new Error("line_delivery_rejected");
      }
      line = "";
      await refresh();
    },
    [bridge, refresh],
  );

  const stopSession = useCallback(
    async (runID: string, sessionID: string) => {
      try {
        await bridge.stopSession(runID, sessionID);
      } catch {
        dispatch({
          type: "error",
          error: localError(runID, "stop_failed", "Stopping the session failed.", sessionID),
        });
      }
    },
    [bridge],
  );

  const startSession = useCallback(
    async (runID: string, sessionID: string) => {
      try {
        await bridge.startSession(runID, sessionID);
      } catch {
        dispatch({
          type: "error",
          error: localError(runID, "start_failed", "Starting the session failed.", sessionID),
        });
      }
    },
    [bridge],
  );

  const restartSession = useCallback(
    async (runID: string, sessionID: string) => {
      try {
        await bridge.restartSession(runID, sessionID);
      } catch {
        dispatch({
          type: "error",
          error: localError(runID, "restart_failed", "Restarting the session failed.", sessionID),
        });
      }
    },
    [bridge],
  );

  const saveAgentProfiles = useCallback(
    (runID: string, request: SaveAgentProfilesRequest) =>
      bridge.saveAgentProfiles(runID, request),
    [bridge],
  );

  const saveAgentProfilesAndRestart = useCallback(
    async (request: SaveAgentProfilesAndRestartRequest): Promise<LifecycleResult> => {
      try {
        const result = await bridge.saveAgentProfilesAndRestart(request);
        dispatch({ type: "loaded", state: result.state });
        return result;
      } catch (error) {
        dispatch({
          type: "error",
          error: localError(
            request.expectedRunID,
            "lifecycle_failed",
            safeError(error, "The run change failed."),
          ),
        });
        await refresh();
        throw error;
      }
    },
    [bridge, refresh],
  );

  const stopRun = useCallback(async (runID: string) => {
    try {
      const stopped = await bridge.stopRun(runID);
      dispatch({ type: "loaded", state: stopped });
      return true;
    } catch {
      dispatch({
        type: "error",
        error: localError(
          runID,
          "stop_run_failed",
          "Stopping the run failed. No new run will start while the state remains uncertain.",
        ),
      });
      await refresh();
      return false;
    }
  }, [bridge, refresh]);

  const sendTerminalInput = useCallback(
    async (runID: string, sessionID: string, data: string | Uint8Array) => {
      if (bridge.sendTerminalInput) {
        await bridge.sendTerminalInput(runID, sessionID, data);
      }
    },
    [bridge],
  );

  const setInteractiveSession = useCallback(
    async (runID: string, sessionID: string, active: boolean) => {
      if (!bridge.setInteractiveSession) return true;
      try {
        await bridge.setInteractiveSession(runID, sessionID, active);
        return true;
      } catch (error) {
        // Taking a terminal another operator holds is refused, not queued. Say
        // which colleague has it rather than leaving a button that does nothing.
        dispatch({
          type: "error",
          error: localError(
            runID,
            active ? "control_take_failed" : "control_release_failed",
            safeError(error, "The terminal could not be taken."),
            sessionID,
          ),
        });
        return false;
      }
    },
    [bridge],
  );

  const observeSession = useCallback(
    async (sessionID: string, observing: boolean) => {
      if (!bridge.observeSession) return;
      try {
        const presence = await bridge.observeSession(sessionID, observing);
        dispatch({ type: "presence", presence });
      } catch {
        // Presence is an indicator, not a control. A roster that could not be
        // joined must never block watching the terminal itself.
      }
    },
    [bridge],
  );

  // The four control verbs share one shape: call, fold the returned snapshot
  // into state, and surface a bounded error rather than a raw one.
  const controlVerb = useCallback(
    async (
      runID: string,
      sessionID: string,
      code: string,
      fallback: string,
      call?: () => Promise<import("../types/relayer").HandView>,
    ) => {
      if (!call) return false;
      try {
        dispatch({ type: "hand", hand: await call() });
        return true;
      } catch (error) {
        dispatch({
          type: "error",
          error: localError(runID, code, safeError(error, fallback), sessionID),
        });
        return false;
      }
    },
    [],
  );

  const requestControl = useCallback(
    (runID: string, sessionID: string) =>
      controlVerb(
        runID,
        sessionID,
        "control_request_failed",
        "The request for the terminal could not be sent.",
        bridge.requestControl && (() => bridge.requestControl!(sessionID)),
      ),
    [bridge, controlVerb],
  );

  const grantControl = useCallback(
    (runID: string, sessionID: string, toConnID: string) =>
      controlVerb(
        runID,
        sessionID,
        "control_grant_failed",
        "The terminal could not be handed over.",
        bridge.grantControl && (() => bridge.grantControl!(sessionID, toConnID)),
      ),
    [bridge, controlVerb],
  );

  const declineControl = useCallback(
    (runID: string, sessionID: string, toConnID: string) =>
      controlVerb(
        runID,
        sessionID,
        "control_decline_failed",
        "The request could not be declined.",
        bridge.declineControl && (() => bridge.declineControl!(sessionID, toConnID)),
      ),
    [bridge, controlVerb],
  );

  const releaseControl = useCallback(
    (runID: string, sessionID: string) =>
      controlVerb(
        runID,
        sessionID,
        "control_release_failed",
        "The terminal could not be released.",
        bridge.releaseControl && (() => bridge.releaseControl!(sessionID)),
      ),
    [bridge, controlVerb],
  );

  return {
    state,
    refresh,
    submitDecision,
    submitAutomaticDecision,
    submitLine,
    resizeSession,
    stopSession,
    startSession,
    restartSession,
    saveAgentProfiles,
    saveAgentProfilesAndRestart,
    stopRun,
    sendTerminalInput,
    setInteractiveSession,
    observeSession,
    requestControl,
    grantControl,
    declineControl,
    releaseControl,
  };
}
