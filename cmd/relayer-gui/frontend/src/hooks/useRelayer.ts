import { useCallback, useEffect, useReducer } from "react";
import { safeError } from "../lib/safety";
import {
  initialRelayerState,
  relayerReducer,
} from "../state/relayerState";
import type {
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
      bridge.on("relayer:status", (status) => dispatch({ type: "status", status })),
      bridge.on("relayer:error", (error) => dispatch({ type: "error", error })),
    ];
    void refresh();
    return () => disposers.forEach((dispose) => dispose());
  }, [bridge, refresh]);

  const submitDecision = useCallback(
    async (runID: string, sessionID: string, eventID: string, value: string) => {
      dispatch({ type: "delivery", runID, sessionID, eventID, status: "delivering" });
      try {
        await bridge.submitDecision(runID, sessionID, eventID, value);
        dispatch({ type: "delivery", runID, sessionID, eventID, status: "delivered" });
        await refresh();
        return true;
      } catch {
        // Once the native bridge has accepted the call, an error cannot prove
        // that zero bytes reached the PTY/tmux pane. Keep the occurrence
        // locked until the native snapshot removes or reconciles it.
        dispatch({ type: "delivery", runID, sessionID, eventID, status: "uncertain" });
        dispatch({
          type: "error",
          error: localError(
            runID,
            "decision_delivery_uncertain",
            "The delivery is indeterminate. Stop or resynchronize the session before any further input.",
            sessionID,
          ),
        });
        return false;
      }
    },
    [bridge, refresh],
  );

  // The semantic path shares the manual path's uncertainty rule: once the
  // native bridge has accepted the call, a failure cannot prove that zero bytes
  // reached the pane, so the occurrence stays locked until the native snapshot
  // reconciles it.
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
        dispatch({ type: "delivery", runID, sessionID, eventID, status: "delivered" });
        await refresh();
        return true;
      } catch {
        dispatch({ type: "delivery", runID, sessionID, eventID, status: "uncertain" });
        dispatch({
          type: "error",
          error: localError(
            runID,
            "decision_delivery_uncertain",
            "The delivery is indeterminate. Stop or resynchronize the session before any further input.",
            sessionID,
          ),
        });
        return false;
      }
    },
    [bridge, refresh],
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

  return {
    state,
    refresh,
    submitDecision,
    submitAutomaticDecision,
    submitLine,
    resizeSession,
    stopSession,
    saveAgentProfiles,
    saveAgentProfilesAndRestart,
    stopRun,
  };
}
