import type {
  AppState,
  AgentProfilesView,
  BridgeEventMap,
  BridgeEventName,
  RelayerBridge,
  SaveAgentProfilesRequest,
} from "../types/relayer";

type NativeMethod<TArgs extends unknown[], TResult> = (...args: TArgs) => Promise<TResult>;

interface NativeBindings {
  GetState: NativeMethod<[], AppState>;
  SubmitDecision: NativeMethod<[string, string, string], void>;
  ResizeSession: NativeMethod<[string, number, number], void>;
  StopSession: NativeMethod<[string], void>;
  GetAgentProfiles: NativeMethod<[], AgentProfilesView>;
  SaveAgentProfiles: NativeMethod<[SaveAgentProfilesRequest], AgentProfilesView>;
  Shutdown: NativeMethod<[], void>;
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
    typeof candidate.Shutdown !== "function"
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
    submitDecision: (sessionID, eventID, value) =>
      bindings.SubmitDecision(sessionID, eventID, value),
    resizeSession: (sessionID, columns, rows) =>
      bindings.ResizeSession(sessionID, columns, rows),
    stopSession: (sessionID) => bindings.StopSession(sessionID),
    getAgentProfiles: () => bindings.GetAgentProfiles(),
    saveAgentProfiles: (request) => bindings.SaveAgentProfiles(request),
    shutdown: () => bindings.Shutdown(),
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
