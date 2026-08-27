import { useCallback, useEffect, useReducer } from "react";
import { safeError } from "../lib/safety";
import {
  initialRelayerState,
  relayerReducer,
} from "../state/relayerState";
import type { RelayerBridge, SafeErrorEvent } from "../types/relayer";

function localError(code: string, message: string, sessionID?: string): SafeErrorEvent {
  return {
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
      dispatch({ type: "loadFailed", message: safeError(error, "Impossible de charger Relayer.") });
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
    async (sessionID: string, eventID: string, value: string) => {
      dispatch({ type: "delivery", sessionID, eventID, status: "delivering" });
      try {
        await bridge.submitDecision(sessionID, eventID, value);
        dispatch({ type: "delivery", sessionID, eventID, status: "delivered" });
        await refresh();
        return true;
      } catch {
        // Once the native bridge has accepted the call, an error cannot prove
        // that zero bytes reached the PTY/tmux pane. Keep the occurrence
        // locked until the native snapshot removes or reconciles it.
        dispatch({ type: "delivery", sessionID, eventID, status: "uncertain" });
        dispatch({
          type: "error",
          error: localError(
            "decision_delivery_uncertain",
            "La livraison est indéterminée. Arrêtez ou resynchronisez la session avant toute nouvelle saisie.",
            sessionID,
          ),
        });
        return false;
      }
    },
    [bridge, refresh],
  );

  const resizeSession = useCallback(
    async (sessionID: string, columns: number, rows: number) => {
      try {
        await bridge.resizeSession(sessionID, columns, rows);
      } catch {
        dispatch({
          type: "error",
          error: localError("resize_failed", "Le redimensionnement de la session a échoué.", sessionID),
        });
      }
    },
    [bridge],
  );

  const stopSession = useCallback(
    async (sessionID: string) => {
      try {
        await bridge.stopSession(sessionID);
      } catch {
        dispatch({
          type: "error",
          error: localError("stop_failed", "L’arrêt de la session a échoué.", sessionID),
        });
      }
    },
    [bridge],
  );

  const shutdown = useCallback(async () => {
    try {
      await bridge.shutdown();
    } catch {
      dispatch({
        type: "error",
        error: localError("shutdown_failed", "L’arrêt de Relayer a échoué."),
      });
    }
  }, [bridge]);

  return { state, refresh, submitDecision, resizeSession, stopSession, shutdown };
}
