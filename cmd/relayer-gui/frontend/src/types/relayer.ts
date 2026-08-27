export type RunStatus = "starting" | "running" | "stopping" | "stopped" | "failed";

export type SessionStatus =
  | "starting"
  | "running"
  | "detached"
  | "attached"
  | "waiting"
  | "exited"
  | "failed";

export type RiskLevel = "low" | "unknown" | "high";
export type EventType = "confirmation" | "credential" | "process_exit";
export type PolicyAction = "allow" | "ask" | "deny";
export type DeliveryStatus = "pending" | "delivering" | "delivered" | "failed" | "uncertain";

export interface PolicyState {
  defaultAction: PolicyAction;
  dryRun: boolean;
}

export interface AuditState {
  enabled: boolean;
  mode: "off" | "metadata" | "detailed";
  status: "ready" | "disabled" | "failed";
  path?: string;
}

export interface AgentState {
  sessionID: string;
  agentID: string;
  name: string;
  displayCommand: string;
  backend: "pty" | "tmux" | string;
  adapter: string;
  status: SessionStatus;
  output: string;
  revision: number;
  running: boolean;
  attached: boolean;
  exitCode?: number;
}

export interface PolicyEvaluation {
  action: PolicyAction;
  proposedAction: PolicyAction;
  ruleName?: string;
  reason: string;
  automatic: boolean;
  dryRun: boolean;
}

// Metadata and raw matches deliberately do not cross the GUI bridge. The Go
// core remains the authority for redaction and audit safety.
export interface SupervisionEvent {
  id: string;
  sessionID: string;
  agentID: string;
  adapter: string;
  type: EventType;
  summary: string;
  sensitive: boolean;
  risk: RiskLevel;
  timestamp: string;
  evaluation: PolicyEvaluation;
  deliveryStatus: DeliveryStatus;
}

export interface AppState {
  runID: string;
  runStatus: RunStatus;
  startedAt?: string;
  policy: PolicyState;
  audit: AuditState;
  agents: AgentState[];
  pendingEvents: SupervisionEvent[];
}

export interface SnapshotEvent {
  sessionID: string;
  revision: number;
  output: string;
  status: SessionStatus;
  running: boolean;
  attached: boolean;
  exitCode?: number;
}

export interface StatusEvent {
  scope: "run" | "session" | "audit";
  status: RunStatus | SessionStatus | AuditState["status"];
  sessionID?: string;
}

export interface SafeErrorEvent {
  code: string;
  message: string;
  sessionID?: string;
  timestamp: string;
}

export type BridgeEventMap = {
  "relayer:snapshot": SnapshotEvent;
  "relayer:event": SupervisionEvent;
  "relayer:status": StatusEvent;
  "relayer:error": SafeErrorEvent;
};

export type BridgeEventName = keyof BridgeEventMap;

export interface RelayerBridge {
  getState(): Promise<AppState>;
  submitDecision(sessionID: string, eventID: string, value: string): Promise<void>;
  resizeSession(sessionID: string, columns: number, rows: number): Promise<void>;
  stopSession(sessionID: string): Promise<void>;
  shutdown(): Promise<void>;
  on<K extends BridgeEventName>(event: K, listener: (payload: BridgeEventMap[K]) => void): () => void;
}
