import type {
  AppState,
  AgentProfilesView,
  BridgeEventMap,
  BridgeEventName,
  LifecycleResult,
  RelayerBridge,
  SaveAgentProfilesRequest,
  SaveAgentProfilesAndRestartRequest,
} from "../types/relayer";

type NativeMethod<TArgs extends unknown[], TResult> = (...args: TArgs) => Promise<TResult>;

interface NativeBindings {
  GetState: NativeMethod<[], AppState>;
  SubmitDecision: NativeMethod<[string, string, string, string], void>;
  ResizeSession: NativeMethod<[string, string, number, number], void>;
  StopSession: NativeMethod<[string, string], void>;
  GetAgentProfiles: NativeMethod<[], AgentProfilesView>;
  SaveAgentProfiles: NativeMethod<[string, SaveAgentProfilesRequest], AgentProfilesView>;
  SaveAgentProfilesAndRestart: NativeMethod<
    [SaveAgentProfilesAndRestartRequest],
    LifecycleResult
  >;
  StopRun: NativeMethod<[string], AppState>;
}

interface WailsRuntime {
  EventsOn(event: string, callback: (payload: unknown) => void): (() => void) | void;
  EventsOff?(event: string): void;
}

declare global {
  interface Window {
    relayerBridge?: NativeBindings;
    go?: {
      main?: {
        App?: NativeBindings;
      };
    };
    runtime?: WailsRuntime;
  }
}

function resolveBindings(): NativeBindings {
  const candidate = window.relayerBridge ?? window.go?.main?.App;
  if (
    !candidate ||
    typeof candidate.GetState !== "function" ||
    typeof candidate.SubmitDecision !== "function" ||
    typeof candidate.ResizeSession !== "function" ||
    typeof candidate.StopSession !== "function" ||
    typeof candidate.GetAgentProfiles !== "function" ||
    typeof candidate.SaveAgentProfiles !== "function" ||
    typeof candidate.SaveAgentProfilesAndRestart !== "function" ||
    typeof candidate.StopRun !== "function"
  ) {
    throw new Error("Le bridge natif Relayer n'est pas disponible.");
  }
  return candidate;
}

function resolveRuntime(): WailsRuntime {
  if (!window.runtime || typeof window.runtime.EventsOn !== "function") {
    throw new Error("Le bus d'événements Wails n'est pas disponible.");
  }
  return window.runtime;
}

// createWailsBridge is intentionally strict: a packaged build without native
// bindings fails visibly and never switches itself to simulated data.
export function createWailsBridge(): RelayerBridge {
  const bindings = resolveBindings();
  const runtime = resolveRuntime();

  return {
    getState: () => bindings.GetState(),
    submitDecision: (runID, sessionID, eventID, value) =>
      bindings.SubmitDecision(runID, sessionID, eventID, value),
    resizeSession: (runID, sessionID, columns, rows) =>
      bindings.ResizeSession(runID, sessionID, columns, rows),
    stopSession: (runID, sessionID) => bindings.StopSession(runID, sessionID),
    getAgentProfiles: () => bindings.GetAgentProfiles(),
    saveAgentProfiles: (runID, request) => bindings.SaveAgentProfiles(runID, request),
    saveAgentProfilesAndRestart: (request) =>
      bindings.SaveAgentProfilesAndRestart(request),
    stopRun: (runID) => bindings.StopRun(runID),
    on<K extends BridgeEventName>(
      event: K,
      listener: (payload: BridgeEventMap[K]) => void,
    ) {
      const callback = (payload: unknown) => listener(payload as BridgeEventMap[K]);
      const dispose = runtime.EventsOn(event, callback);
      if (typeof dispose === "function") {
        return dispose;
      }
      return () => runtime.EventsOff?.(event);
    },
  };
}
