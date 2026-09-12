import type {
  AppState,
  AgentProfilesView,
  AuditEntryView,
  AuditFilterInput,
  AuditSummaryView,
  AuditVerificationView,
  BridgeEventMap,
  BridgeEventName,
  FullSettingsView,
  LifecycleResult,
  PreflightReport,
  RelayerBridge,
  SaveAgentProfilesRequest,
  SaveAgentProfilesAndRestartRequest,
  SaveFullSettingsRequest,
  SemanticDecision,
  TelemetrySnapshotView,
} from "../types/relayer";

interface PendingRpc {
  resolve: (result: unknown) => void;
  reject: (error: Error) => void;
  timeoutId: ReturnType<typeof setTimeout>;
}

export interface WebBridgeOptions {
  url?: string;
  token?: string;
  rpcTimeoutMs?: number;
}

export function createWebBridge(options: WebBridgeOptions = {}): RelayerBridge {
  let ws: WebSocket | null = null;
  let nextRpcId = 1;
  const pendingRpcs = new Map<string, PendingRpc>();
  const sendQueue: string[] = [];
  const listeners = new Map<string, Set<(payload: unknown) => void>>();
  let isClosed = false;
  let reconnectTimer: ReturnType<typeof setTimeout> | null = null;

  // Resolve token from options, query parameters, hash, or localStorage
  let token = options.token;
  if (!token && typeof window !== "undefined" && window.location) {
    const urlParams = new URLSearchParams(window.location.search);
    token = urlParams.get("token") || undefined;
    if (!token && window.location.hash.startsWith("#token=")) {
      token = window.location.hash.slice(7);
    }
    if (token) {
      try {
        localStorage.setItem("relayer_token", token);
      } catch {
        // Ignore localStorage errors (e.g. private browsing)
      }
    } else {
      try {
        token = localStorage.getItem("relayer_token") || undefined;
      } catch {
        // Ignore localStorage errors
      }
    }
  }

  function resolveWsUrl(): string {
    if (options.url) {
      return options.url;
    }
    const loc = window.location;
    const protocol = loc.protocol === "https:" ? "wss:" : "ws:";
    let wsUrl = `${protocol}//${loc.host}/api/ws`;
    if (token) {
      wsUrl += `?token=${encodeURIComponent(token)}`;
    }
    return wsUrl;
  }

  function connect(): void {
    if (isClosed) return;

    try {
      const url = resolveWsUrl();
      ws = new WebSocket(url);

      ws.onopen = () => {
        // Flush queue
        while (sendQueue.length > 0 && ws && ws.readyState === WebSocket.OPEN) {
          const item = sendQueue.shift();
          if (item) ws.send(item);
        }
      };

      ws.onmessage = (evt: MessageEvent) => {
        try {
          const data = JSON.parse(evt.data);

          // Check if this is an RPC response
          if (data && typeof data.id === "string" && pendingRpcs.has(data.id)) {
            const pending = pendingRpcs.get(data.id)!;
            pendingRpcs.delete(data.id);
            clearTimeout(pending.timeoutId);

            if (data.error) {
              pending.reject(new Error(data.error));
            } else {
              pending.resolve(data.result);
            }
            return;
          }

          // Check if this is a broadcast event
          if (data && typeof data.event === "string") {
            const set = listeners.get(data.event);
            if (set) {
              set.forEach((fn) => {
                try {
                  fn(data.payload);
                } catch (err) {
                  console.error("Error in bridge event listener:", err);
                }
              });
            }
          }
        } catch (err) {
          console.error("Failed to parse incoming WebSocket message:", err);
        }
      };

      ws.onclose = () => {
        ws = null;
        if (!isClosed) {
          scheduleReconnect();
        }
      };

      ws.onerror = () => {
        // Handled by close event
      };
    } catch {
      scheduleReconnect();
    }
  }

  function scheduleReconnect(): void {
    if (reconnectTimer || isClosed) return;
    reconnectTimer = setTimeout(() => {
      reconnectTimer = null;
      connect();
    }, 1500);
  }

  connect();

  function callRpc<T>(method: string, params: unknown = {}): Promise<T> {
    return new Promise<T>((resolve, reject) => {
      const id = String(nextRpcId++);
      const timeoutMs = options.rpcTimeoutMs ?? 15000;

      const timeoutId = setTimeout(() => {
        pendingRpcs.delete(id);
        reject(new Error(`RPC request ${method} (id=${id}) timed out after ${timeoutMs}ms`));
      }, timeoutMs);

      pendingRpcs.set(id, {
        resolve: resolve as (res: unknown) => void,
        reject,
        timeoutId,
      });

      const message = JSON.stringify({ id, method, params });
      if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(message);
      } else {
        sendQueue.push(message);
      }
    });
  }

  return {
    getState: () => callRpc<AppState>("getState"),
    runPreflight: () => callRpc<PreflightReport>("runPreflight"),
    submitDecision: (runID, sessionID, eventID, value) =>
      callRpc<void>("submitDecision", { runID, sessionID, eventID, value }),
    submitAutomaticDecision: (runID, sessionID, eventID, decision) =>
      callRpc<void>("submitAutomaticDecision", { runID, sessionID, eventID, decision }),
    submitLine: (runID, sessionID, line) =>
      callRpc<void>("submitLine", { runID, sessionID, line }),
    resizeSession: (runID, sessionID, columns, rows) =>
      callRpc<void>("resizeSession", { runID, sessionID, columns, rows }),
    stopSession: (runID, sessionID) =>
      callRpc<void>("stopSession", { runID, sessionID }),
    startSession: (runID, sessionID) =>
      callRpc<void>("startSession", { runID, sessionID }),
    restartSession: (runID, sessionID) =>
      callRpc<void>("restartSession", { runID, sessionID }),
    getAgentProfiles: () =>
      callRpc<AgentProfilesView>("getAgentProfiles"),
    saveAgentProfiles: (runID, request) =>
      callRpc<AgentProfilesView>("saveAgentProfiles", { runID, request }),
    saveAgentProfilesAndRestart: (request) =>
      callRpc<LifecycleResult>("saveAgentProfilesAndRestart", request),
    getFullSettings: () =>
      callRpc<FullSettingsView>("getFullSettings"),
    saveFullSettings: (runID, request) =>
      callRpc<FullSettingsView>("saveFullSettings", { runID, request }),
    stopRun: (runID) =>
      callRpc<AppState>("stopRun", { runID }),
    getAuditSummary: () =>
      callRpc<AuditSummaryView>("getAuditSummary"),
    getAuditEntries: (filter) =>
      callRpc<AuditEntryView[]>("getAuditEntries", filter ?? {}),
    verifyAuditJournal: () =>
      callRpc<AuditVerificationView>("verifyAuditJournal"),
    exportAuditReport: (format) =>
      callRpc<string>("exportAuditReport", { format }),
    getTelemetrySnapshot: () =>
      callRpc<TelemetrySnapshotView>("getTelemetrySnapshot"),

    on<K extends BridgeEventName>(
      event: K,
      listener: (payload: BridgeEventMap[K]) => void,
    ) {
      let set = listeners.get(event);
      if (!set) {
        set = new Set();
        listeners.set(event, set);
      }
      const genericListener = listener as (payload: unknown) => void;
      set.add(genericListener);

      return () => {
        const currentSet = listeners.get(event);
        if (currentSet) {
          currentSet.delete(genericListener);
          if (currentSet.size === 0) {
            listeners.delete(event);
          }
        }
      };
    },
  };
}
