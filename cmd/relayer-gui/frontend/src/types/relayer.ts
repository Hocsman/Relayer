export type RunStatus =
  | "idle"
  | "starting"
  | "running"
  | "restarting"
  | "rollback"
  | "stopping"
  | "stopped"
  | "failed";

export type SessionStatus =
  | "starting"
  | "running"
  | "detached"
  | "attached"
  | "waiting"
  | "stopping"
  | "exited"
  | "failed";

export type RiskLevel = "low" | "unknown" | "high";
export type EventType = "confirmation" | "permission" | "credential" | "process_exit";
export type PolicyAction = "allow" | "ask" | "deny";
export type DeliveryStatus = "pending" | "delivering" | "delivered" | "failed" | "uncertain";
export type AgentPresetID =
  | "claude-code"
  | "codex-cli"
  | "mimo-code"
  | "ollama"
  | "custom";
export type AgentBackend = "auto" | "pty" | "tmux";
export type AdapterStatus = "stable" | "experimental";
export type InstallStatus = "unknown" | "installed" | "not_installed";
export type ProfileReadOnlyReason =
  | "advanced_shell"
  | "advanced_environment"
  | "advanced_adapter"
  | "invalid_command"
  | "legacy_profile_fields"
  | "sensitive_arguments";

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
  inputFrozen?: boolean;
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
  runID: string;
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
  runID: string;
  sessionID: string;
  revision: number;
  output: string;
  status: SessionStatus;
  running: boolean;
  attached: boolean;
  inputFrozen?: boolean;
  exitCode?: number;
}

export interface StatusEvent {
  runID: string;
  scope: "run" | "session" | "audit";
  status: RunStatus | SessionStatus | AuditState["status"];
  sessionID?: string;
}

export interface SafeErrorEvent {
  runID: string;
  code: string;
  message: string;
  sessionID?: string;
  timestamp: string;
}

// Agent settings deliberately exclude environment variables, provider
// credentials and shell snippets. Commands remain exact argv; a user-entered
// model identifier is an ordinary argument and is persisted in local YAML.
export interface AgentCatalogEntry {
  id: AgentPresetID;
  name: string;
  description: string;
  installStatus: InstallStatus;
  installed: boolean;
  adapter: string;
  adapterStatus: AdapterStatus;
  defaultArgv: string[];
  requiresCustomArgv: boolean;
  minimumArguments: number;
  argumentPrefix: string[];
}

export interface AgentProfile {
  id: string;
  name: string;
  presetID: AgentPresetID;
  cwd: string;
  backend: AgentBackend;
  adapter: string;
  argv?: string[];
  executableLabel?: string;
  argumentCount?: number;
  locked: boolean;
  readOnlyReason?: ProfileReadOnlyReason;
  preserveOnSave?: boolean;
}

export interface AgentProfilesView {
  configPath: string;
  revision: string;
  catalog: AgentCatalogEntry[];
  profiles: AgentProfile[];
  minProfiles: number;
  maxProfiles: number;
  restartRequired: boolean;
  editable: boolean;
  readOnlyReason?: "legacy_config";
}

export interface SaveAgentProfilesRequest {
  expectedRevision: string;
  profiles: AgentProfileInput[];
}

export interface SaveAgentProfilesAndRestartRequest extends SaveAgentProfilesRequest {
  expectedRunID: string;
}

export type LifecycleOutcome = "started" | "restarted" | "rolled_back";

export interface LifecycleResult {
  outcome: LifecycleOutcome;
  state: AppState;
  profiles: AgentProfilesView;
}

export type PreflightStatus = "ready" | "warning" | "blocked";
export type PreflightCheckStatus = "pass" | "warning" | "block";
export type PreflightScope =
  | "configuration"
  | "platform"
  | "policy"
  | "audit"
  | "tool"
  | "agent"
  | "adapter"
  | "backend";

// Preflight data is deliberately display-only. Paths, argv, environment
// values and native errors never cross the Wails bridge.
export interface PreflightCheck {
  id: string;
  scope: PreflightScope;
  status: PreflightCheckStatus;
  summary: string;
  remediation?: string;
}

export interface PreflightReport {
  schemaVersion: number;
  status: PreflightStatus;
  platform: {
    os: string;
    arch: string;
    supported: boolean;
  };
  configuration: {
    version: number;
    legacy: boolean;
    agentCount: number;
    policyRuleCount: number;
  };
  audit: {
    enabled: boolean;
    mode: AuditState["mode"];
    location: "disabled" | "default" | "custom";
    maxFileSizeMB: number;
    maxFiles: number;
  };
  tools: Array<{
    profileID: AgentPresetID;
    installation: InstallStatus;
  }>;
  agents: Array<{
    ordinal: number;
    source: "configured" | "demo";
    command: "direct" | "shell";
    installation: InstallStatus;
    adapter?: string;
    adapterMaturity?: AdapterStatus;
    backend?: string;
  }>;
  checks: PreflightCheck[];
}

export interface AgentProfileInput {
  id: string;
  name: string;
  presetID: AgentPresetID;
  cwd: string;
  backend: AgentBackend;
  adapter: string;
  argv: string[];
  preserve: boolean;
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
  runPreflight(): Promise<PreflightReport>;
  submitDecision(runID: string, sessionID: string, eventID: string, value: string): Promise<void>;
  submitLine(runID: string, sessionID: string, line: string): Promise<void>;
  resizeSession(runID: string, sessionID: string, columns: number, rows: number): Promise<void>;
  stopSession(runID: string, sessionID: string): Promise<void>;
  getAgentProfiles(): Promise<AgentProfilesView>;
  saveAgentProfiles(runID: string, request: SaveAgentProfilesRequest): Promise<AgentProfilesView>;
  saveAgentProfilesAndRestart(request: SaveAgentProfilesAndRestartRequest): Promise<LifecycleResult>;
  stopRun(runID: string): Promise<AppState>;
  on<K extends BridgeEventName>(event: K, listener: (payload: BridgeEventMap[K]) => void): () => void;
}
