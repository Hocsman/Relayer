import type {
  AgentProfile,
  AgentProfilesView,
  AgentState,
  AppState,
  AuditEntryView,
  AuditFilterInput,
  AuditSummaryView,
  AuditVerificationView,
  BridgeEventMap,
  BridgeEventName,
  RelayerBridge,
  PreflightReport,
  SaveAgentProfilesRequest,
  SupervisionEvent,
  TelemetrySnapshotView,
} from "../types/relayer";

type Listener = (payload: never) => void;

function initialState(): AppState {
  return {
    runID: "",
    runStatus: "idle",
    policy: { defaultAction: "ask", dryRun: false },
    audit: { enabled: false, mode: "off", status: "disabled" },
    agents: [],
    pendingEvents: [],
  };
}

function initialProfiles(): AgentProfilesView {
  return {
    configPath: "/tmp/relayer-demo/config.yaml",
    revision: "demo-1",
    minProfiles: 1,
    maxProfiles: 8,
    restartRequired: false,
    editable: true,
    catalog: [
      {
        id: "claude-code",
        name: "Claude Code",
        description: "Command-line development assistant.",
        installStatus: "installed",
        installed: true,
        adapter: "claude",
        adapterStatus: "experimental",
        defaultArgv: ["claude"],
        requiresCustomArgv: false,
        minimumArguments: 0,
        argumentPrefix: [],
      },
      {
        id: "codex-cli",
        name: "Codex CLI",
        description: "Coding agent driven from the terminal.",
        installStatus: "installed",
        installed: true,
        adapter: "codex",
        adapterStatus: "experimental",
        defaultArgv: ["codex"],
        requiresCustomArgv: false,
        minimumArguments: 0,
        argumentPrefix: [],
      },
      {
        id: "aider",
        name: "Aider",
        description: "Aider coding assistant; interactive pair programming with terminal prompts.",
        installStatus: "installed",
        installed: true,
        adapter: "aider",
        adapterStatus: "experimental",
        defaultArgv: ["aider"],
        requiresCustomArgv: false,
        minimumArguments: 0,
        argumentPrefix: [],
      },
      {
        id: "goose",
        name: "Goose CLI",
        description: "Goose CLI; developer AI agent with automated tool and command execution prompts.",
        installStatus: "installed",
        installed: true,
        adapter: "goose",
        adapterStatus: "experimental",
        defaultArgv: ["goose"],
        requiresCustomArgv: false,
        minimumArguments: 0,
        argumentPrefix: [],
      },
      {
        id: "open-interpreter",
        name: "Open Interpreter",
        description: "Open Interpreter; local code and command execution with human approval prompts.",
        installStatus: "installed",
        installed: true,
        adapter: "interpreter",
        adapterStatus: "experimental",
        defaultArgv: ["interpreter"],
        requiresCustomArgv: false,
        minimumArguments: 0,
        argumentPrefix: [],
      },
      {
        id: "mimo-code",
        name: "MiMo Code",
        description: "MiMo Code CLI with generic interception.",
        installStatus: "not_installed",
        installed: false,
        adapter: "generic",
        adapterStatus: "stable",
        defaultArgv: ["mimo"],
        requiresCustomArgv: false,
        minimumArguments: 0,
        argumentPrefix: [],
      },
      {
        id: "ollama",
        name: "Ollama / DeepSeek",
        description: "Local CLI; run and the model stay explicit.",
        installStatus: "installed",
        installed: true,
        adapter: "generic",
        adapterStatus: "stable",
        defaultArgv: ["ollama", "run", ""],
        requiresCustomArgv: false,
        minimumArguments: 2,
        argumentPrefix: ["run"],
      },
      {
        id: "custom",
        name: "Custom",
        description: "Local command as exact arguments.",
        installStatus: "unknown",
        installed: true,
        adapter: "generic",
        adapterStatus: "stable",
        defaultArgv: [],
        requiresCustomArgv: true,
        minimumArguments: 0,
        argumentPrefix: [],
      },
    ],
    profiles: [
      {
        id: "demo-a",
        name: "Agent A · Claude",
        presetID: "claude-code",
        cwd: "",
        backend: "pty",
        adapter: "claude",
        executableLabel: "claude",
        argumentCount: 0,
        locked: false,
        preserveOnSave: true,
      },
      {
        id: "demo-b",
        name: "Agent B · Codex",
        presetID: "codex-cli",
        cwd: "",
        backend: "tmux",
        adapter: "codex",
        executableLabel: "codex",
        argumentCount: 0,
        locked: false,
        preserveOnSave: true,
      },
    ],
  };
}

function demoEvent(
  runID: string,
  sessionID: string,
  sensitive: boolean,
  adapter: string,
): SupervisionEvent {
  return {
    runID,
    id: `${sessionID}-prompt-1`,
    sessionID,
    agentID: sessionID,
    adapter,
    type: sensitive ? "credential" : "confirmation",
    summary: sensitive
      ? "Confidential input required"
      : adapter === "codex"
        ? "Allow command execution?"
        : "Overwrite generated file? [Y/n]",
    sensitive,
    risk: sensitive ? "high" : "unknown",
    timestamp: new Date().toISOString(),
    evaluation: {
      action: "ask",
      proposedAction: "ask",
      reason: sensitive ? "sensitive_event" : "default_action",
      automatic: false,
      dryRun: false,
    },
    deliveryStatus: "pending",
    // Mirrors the real probe: only the Codex adapter has verified bytes for
    // accepting or refusing a command approval. A generic prompt is answered by
    // typing whatever it asked for.
    decisions: adapter === "codex" && !sensitive ? ["allow", "deny"] : [],
  };
}

function delay(milliseconds: number): Promise<void> {
  return new Promise((resolve) => window.setTimeout(resolve, milliseconds));
}

// The demo is only constructed when VITE_RELAYER_DEMO=true. Production never
// calls this function as a fallback when the native Wails bridge is missing.
export function createDemoBridge(): RelayerBridge {
  let state = initialState();
  let profiles = initialProfiles();
  let runSequence = 0;
  const listeners = new Map<BridgeEventName, Set<Listener>>();
  const lineCounts = new Map<string, number>();

  const emit = <K extends BridgeEventName>(event: K, payload: BridgeEventMap[K]) => {
    listeners.get(event)?.forEach((listener) => listener(payload as never));
  };

  const requireRun = (runID: string, runningOnly = false) => {
    if (runID !== state.runID || (runningOnly && state.runStatus !== "running")) {
      throw new Error("The demo run is stale.");
    }
  };

  const saveProfiles = (request: SaveAgentProfilesRequest): AgentProfilesView => {
    if (request.expectedRevision !== profiles.revision) {
      throw new Error("The demo configuration is stale.");
    }
    const revision = Number.parseInt(profiles.revision.replace("demo-", ""), 10) + 1;
    const previousProfiles = profiles.profiles;
    const nextProfiles: AgentProfile[] = request.profiles.map((input) => {
      if (input.preserve) {
        const existing = previousProfiles.find(
          (profile) => profile.id.toLocaleLowerCase() === input.id.toLocaleLowerCase(),
        );
        if (existing) {
          return {
            ...structuredClone(existing),
            name: input.name,
            cwd: input.cwd,
            backend: input.backend,
          };
        }
      }
      return {
        id: input.id,
        name: input.name,
        presetID: input.presetID,
        cwd: input.cwd,
        backend: input.backend,
        adapter: input.adapter,
        executableLabel: input.argv[0] || "custom command",
        argumentCount: Math.max(0, input.argv.length - 1),
        locked: false,
        preserveOnSave: true,
      };
    });
    profiles = {
      ...profiles,
      revision: `demo-${revision}`,
      profiles: nextProfiles,
      restartRequired: state.runStatus === "running",
    };
    return structuredClone(profiles);
  };

  const resolveDemoEvent = (runID: string, sessionID: string, eventID: string) => {
    requireRun(runID, true);
    const eventIndex = state.pendingEvents.findIndex(
      (event) => event.runID === runID && event.id === eventID && event.sessionID === sessionID,
    );
    if (eventIndex < 0) throw new Error("The demo event is stale.");
    state.pendingEvents.splice(eventIndex, 1);
    const agent = state.agents.find((candidate) => candidate.sessionID === sessionID);
    if (!agent) throw new Error("The demo session was not found.");
    agent.output += "Decision received and submitted.\nTask complete.\n";
    agent.revision += 1;
    agent.status = "exited";
    agent.running = false;
    agent.exitCode = 0;
    emit("relayer:snapshot", {
      runID,
      sessionID,
      revision: agent.revision,
      output: agent.output,
      status: agent.status,
      running: false,
      attached: false,
      inputFrozen: false,
      exitCode: 0,
    });
  };

  const demoNotices = (): string[] => [
    "Demo mode: no real agent is started and nothing is recorded",
    `${profiles.profiles.length} agent(s) simulated by browser scripts`,
  ];

  const agentsFromProfiles = (): AgentState[] => profiles.profiles.map((profile) => ({
    sessionID: profile.id,
    agentID: profile.id,
    name: profile.name,
    displayCommand: profile.executableLabel || "configured command",
    backend: profile.backend === "auto" ? "pty" : profile.backend,
    adapter: profile.adapter,
    status: "running",
    output: "Relayer demo — agent initialized\n",
    revision: 1,
    running: true,
    attached: false,
    inputFrozen: false,
    // Every agent in the demo bridge is a script; none supervises a real tool.
    // The interface says so rather than letting the demo pass for a session.
    simulated: true,
  }));

  window.setInterval(() => {
    if (state.runStatus !== "running") return;
    const eventRunID = state.runID;
    for (const agent of state.agents) {
      if (agent.status !== "running") continue;
      const next = (lineCounts.get(agent.sessionID) ?? 0) + 1;
      lineCounts.set(agent.sessionID, next);
      agent.output += `Generating · step ${next.toString().padStart(2, "0")}\n`;
      agent.revision += 1;
      emit("relayer:snapshot", {
        runID: eventRunID,
        sessionID: agent.sessionID,
        revision: agent.revision,
        output: agent.output,
        status: agent.status,
        running: agent.running,
        attached: agent.attached,
        inputFrozen: Boolean(agent.inputFrozen),
      });

      const threshold = agent.sessionID === "demo-a" ? 8 : 12;
      if (next === threshold) {
        const event = demoEvent(
          eventRunID,
          agent.sessionID,
          agent.sessionID === "demo-a",
          agent.adapter,
        );
        state.pendingEvents.push(event);
        agent.status = "waiting";
        // Derived from the event so the pane and the queue can never describe
        // two different prompts.
        agent.output += event.sensitive
          ? "Credential required:\n"
          : event.adapter === "codex"
            ? "Allow command execution? [y/esc]\n"
            : "Overwrite generated file? [Y/n]\n";
        agent.revision += 1;
        emit("relayer:snapshot", {
          runID: eventRunID,
          sessionID: agent.sessionID,
          revision: agent.revision,
          output: agent.output,
          status: agent.status,
          running: true,
          attached: false,
          inputFrozen: false,
        });
        emit("relayer:event", event);
      }
    }
  }, 360);

  return {
    async getState() {
      return structuredClone(state);
    },
    async runPreflight(): Promise<PreflightReport> {
      await delay(180);
      return {
        schemaVersion: 1,
        status: "warning",
        platform: { os: "darwin", arch: "arm64", supported: true },
        configuration: {
          version: 1,
          legacy: false,
          agentCount: 2,
          policyRuleCount: 1,
        },
        audit: {
          enabled: true,
          mode: "metadata",
          location: "default",
          maxFileSizeMB: 10,
          maxFiles: 5,
        },
        tools: [
          { profileID: "claude-code", installation: "installed" },
          { profileID: "codex-cli", installation: "installed" },
        ],
        agents: [
          {
            ordinal: 1,
            source: "configured",
            command: "direct",
            installation: "installed",
            adapter: "claude",
            adapterMaturity: "experimental",
            backend: "pty",
          },
          {
            ordinal: 2,
            source: "configured",
            command: "direct",
            installation: "installed",
            adapter: "codex",
            adapterMaturity: "experimental",
            backend: "tmux",
          },
        ],
        checks: [
          {
            id: "configuration",
            scope: "configuration",
            status: "pass",
            summary: "The saved configuration is valid.",
          },
          {
            id: "policy",
            scope: "policy",
            status: "pass",
            summary: "The policy engine is ready.",
          },
          {
            id: "audit",
            scope: "audit",
            status: "pass",
            summary: "The local journal can be opened in secure mode.",
          },
          {
            id: "backend",
            scope: "backend",
            status: "pass",
            summary: "The configured backend is available.",
          },
          {
            id: "adapter-compatibility",
            scope: "adapter",
            status: "warning",
            summary: "An experimental adapter requires human supervision.",
            remediation: "Keep the default policy on ask.",
          },
        ],
      };
    },
    async submitAutomaticDecision(runID, sessionID, eventID, decision) {
      const offered = state.pendingEvents.find(
        (event) => event.runID === runID && event.id === eventID && event.sessionID === sessionID,
      )?.decisions;
      // The demo refuses what the real core refuses: a decision the adapter for
      // this occurrence cannot encode, whatever the screen was showing.
      if (!offered?.includes(decision)) {
        throw new Error("This decision cannot be encoded for this request.");
      }
      return resolveDemoEvent(runID, sessionID, eventID);
    },
    async submitDecision(runID, sessionID, eventID, _value) {
      return resolveDemoEvent(runID, sessionID, eventID);
    },
    async submitLine(runID, sessionID, _line) {
      requireRun(runID, true);
      const agent = state.agents.find((candidate) => candidate.sessionID === sessionID);
      if (!agent || !agent.running || agent.attached || agent.inputFrozen) {
        throw new Error("The demo session does not accept this line.");
      }
      if (state.pendingEvents.some((event) => event.sessionID === sessionID)) {
        throw new Error("A supervision request is pending.");
      }
      agent.output += "Local line submitted (content not kept).\n";
      agent.revision += 1;
      emit("relayer:snapshot", {
        runID,
        sessionID,
        revision: agent.revision,
        output: agent.output,
        status: agent.status,
        running: true,
        attached: false,
        inputFrozen: false,
      });
    },
    async resizeSession(runID, _sessionID, columns, rows) {
      requireRun(runID, true);
      if (columns < 1 || rows < 1) throw new Error("Invalid terminal dimensions.");
    },
    async stopSession(runID, sessionID) {
      requireRun(runID, true);
      const agent = state.agents.find((candidate) => candidate.sessionID === sessionID);
      if (!agent) throw new Error("The demo session was not found.");
      agent.status = "exited";
      agent.running = false;
      agent.exitCode = 130;
      state.pendingEvents = state.pendingEvents.filter((event) => event.sessionID !== sessionID);
      emit("relayer:status", { runID, scope: "session", sessionID, status: "exited" });
    },
    async getAgentProfiles() {
      return structuredClone(profiles);
    },
    async getFullSettings() {
      return {
        ...structuredClone(profiles),
        security: {
          profile: "developer-friendly",
          defaultAction: "ask",
          dryRun: false,
          blockDestructive: true,
          blockExfiltration: true,
          blockSensitivePaths: true,
          blockOutsideWorkspace: false,
          workspaceRoot: "/workspace",
          rateLimitPerMinute: 30,
          maxConsecutiveAutoDecisions: 10,
        },
        notifications: {
          enabled: true,
          bell: true,
          desktop: true,
          minSeverity: "info",
          webhooks: [
            {
              name: "Slack Ops",
              url: "https://hooks.slack.com/services/demo",
              format: "slack",
              minSeverity: "warning",
              timeout: "5s",
            },
          ],
        },
      };
    },
    async saveAgentProfiles(runID, request) {
      requireRun(runID);
      if (["starting", "restarting", "rollback", "stopping"].includes(state.runStatus)) {
        throw new Error("A run change is already in progress.");
      }
      return saveProfiles(request);
    },
    async saveFullSettings(runID, request) {
      if (request.profiles) {
        saveProfiles({ expectedRevision: request.expectedRevision, profiles: request.profiles });
      }
      return this.getFullSettings();
    },
    async saveAgentProfilesAndRestart(request) {
      requireRun(request.expectedRunID);
      const restarting = state.runStatus === "running";
      if (!restarting && state.runStatus !== "idle" && state.runStatus !== "failed") {
        throw new Error("A run change is already in progress.");
      }
      const transitionRunID = state.runID;
      state = {
        ...state,
        runStatus: restarting ? "restarting" : "starting",
        pendingEvents: [],
      };
      emit("relayer:status", {
        runID: transitionRunID,
        scope: "run",
        status: state.runStatus,
      });
      saveProfiles(request);
      await delay(500);

      runSequence += 1;
      const runID = `demo-run-${runSequence}`;
      lineCounts.clear();
      state = {
        runID,
        runStatus: "running",
        startedAt: new Date().toISOString(),
        policy: { defaultAction: "ask", dryRun: false },
        audit: { enabled: true, mode: "metadata", status: "ready" },
        agents: agentsFromProfiles(),
        pendingEvents: [],
        notices: demoNotices(),
      };
      profiles = { ...profiles, restartRequired: false };
      emit("relayer:status", { runID, scope: "run", status: "running" });
      return {
        outcome: restarting ? "restarted" : "started",
        state: structuredClone(state),
        profiles: structuredClone(profiles),
      };
    },
    async stopRun(runID) {
      requireRun(runID);
      if (state.runStatus !== "running" && state.runStatus !== "failed") {
        throw new Error("The run cannot be stopped in this state.");
      }
      state = { ...state, runStatus: "stopping", pendingEvents: [] };
      emit("relayer:status", { runID, scope: "run", status: "stopping" });
      await delay(350);
      state = initialState();
      emit("relayer:status", { runID, scope: "run", status: "idle" });
      return structuredClone(state);
    },
    async getAuditSummary() {
      await delay(120);
      const entries = demoAuditEntries();
      const agentCounts: Record<string, number> = {};
      const kindCounts: Record<string, number> = {};
      const decisionsCount: Record<string, number> = {};
      const actorsCount: Record<string, number> = {};
      const outcomesCount: Record<string, number> = {};
      let sensitiveCount = 0;
      for (const e of entries) {
        if (e.agentID) agentCounts[e.agentID] = (agentCounts[e.agentID] ?? 0) + 1;
        if (e.kind) kindCounts[e.kind] = (kindCounts[e.kind] ?? 0) + 1;
        if (e.decision) decisionsCount[e.decision] = (decisionsCount[e.decision] ?? 0) + 1;
        if (e.decisionBy) actorsCount[e.decisionBy] = (actorsCount[e.decisionBy] ?? 0) + 1;
        if (e.outcome) outcomesCount[e.outcome] = (outcomesCount[e.outcome] ?? 0) + 1;
        if (e.sensitive) sensitiveCount += 1;
      }
      return {
        path: "/tmp/relayer-demo/audit.jsonl",
        totalEntries: entries.length,
        runsCount: 1,
        sessionsCount: 2,
        agentCounts,
        kindCounts,
        decisionsCount,
        actorsCount,
        outcomesCount,
        sensitiveCount,
        firstTimestamp: entries[0]?.timestamp,
        lastTimestamp: entries[entries.length - 1]?.timestamp,
      };
    },
    async getAuditEntries(filter?: AuditFilterInput) {
      await delay(150);
      let entries = demoAuditEntries();
      if (filter?.agentID) {
        entries = entries.filter((e) => e.agentID?.toLowerCase() === filter.agentID?.toLowerCase());
      }
      if (filter?.kind) {
        entries = entries.filter((e) => e.kind === filter.kind);
      }
      if (filter?.limit && filter.limit > 0 && entries.length > filter.limit) {
        entries = entries.slice(entries.length - filter.limit);
      }
      return entries;
    },
    async verifyAuditJournal() {
      await delay(200);
      const entries = demoAuditEntries();
      return {
        path: "/tmp/relayer-demo/audit.jsonl",
        totalLines: entries.length,
        totalRuns: 1,
        validLines: entries.length,
        issues: [],
        passed: true,
      };
    },
    async exportAuditReport(format: "json" | "csv") {
      await delay(100);
      const entries = demoAuditEntries();
      if (format === "json") {
        return JSON.stringify(entries, null, 2);
      }
      const header = "sequence,timestamp,entry_id,run_id,agent_id,session_id,kind,event_type,decision,decision_by,outcome,risk,sensitive,rule,reason,summary\n";
      const rows = entries.map((e) =>
        [
          e.sequence,
          e.timestamp,
          e.entryID,
          e.runID,
          e.agentID ?? "",
          e.sessionID ?? "",
          e.kind,
          e.eventType ?? "",
          e.decision ?? "",
          e.decisionBy ?? "",
          e.outcome ?? "",
          e.risk ?? "",
          e.sensitive ? "true" : "false",
          e.rule ?? "",
          e.reason ?? "",
          `"${(e.summary ?? "").replace(/"/g, '""')}"`,
        ].join(",")
      );
      return header + rows.join("\n");
    },
    async getTelemetrySnapshot(): Promise<TelemetrySnapshotView> {
      await delay(100);
      return {
        timestamp: new Date().toISOString(),
        enabled: true,
        prometheusEnabled: true,
        prometheusAddress: "http://localhost:9090/metrics",
        otlpEnabled: true,
        otlpEndpoint: "http://localhost:4318/v1/metrics",
        sessionsActive: state.agents.filter((a) => a.running).length,
        eventsPending: state.pendingEvents.length,
        sessionsTotal: state.agents.length,
        eventsDetectedTotal: 18,
        eventsWithdrawnTotal: 1,
        decisionsTotal: 15,
        decisionsBreakdown: {
          allow: 9,
          deny: 3,
          autoAllow: 2,
          autoDeny: 1,
          custom: 0,
        },
        operatorInputsTotal: 4,
        guardrailsTotal: 3,
        guardrailsBreakdown: {
          destructive_command: 2,
          sensitive_paths: 1,
        },
        averageReactionTime: 1.84,
        decisionDurations: [
          { le: 0.5, label: "< 0.5s", count: 2 },
          { le: 1.0, label: "0.5s - 1s", count: 4 },
          { le: 2.0, label: "1s - 2s", count: 5 },
          { le: 5.0, label: "2s - 5s", count: 3 },
          { le: 10.0, label: "5s - 10s", count: 1 },
          { le: 30.0, label: "10s - 30s", count: 0 },
          { le: 60.0, label: "30s - 60s", count: 0 },
          { le: 300.0, label: "> 60s", count: 0 },
        ],
      };
    },
    on<K extends BridgeEventName>(
      event: K,
      listener: (payload: BridgeEventMap[K]) => void,
    ) {
      const set = listeners.get(event) ?? new Set<Listener>();
      set.add(listener as Listener);
      listeners.set(event, set);
      return () => set.delete(listener as Listener);
    },
  };
}

function demoAuditEntries(): AuditEntryView[] {
  const base = new Date(Date.now() - 3600000);
  return [
    {
      sequence: 1,
      timestamp: new Date(base.getTime() + 1000).toISOString(),
      entryID: "demo-ent-1",
      runID: "demo-run-1",
      kind: "run_started",
      decisionBy: "system",
      outcome: "started",
      summary: "Relayer supervision engine started",
      sensitive: false,
    },
    {
      sequence: 2,
      timestamp: new Date(base.getTime() + 2000).toISOString(),
      entryID: "demo-ent-2",
      runID: "demo-run-1",
      sessionID: "demo-sess-1",
      agentID: "claude-code",
      backend: "pty",
      adapter: "claude",
      kind: "session_started",
      decisionBy: "system",
      outcome: "started",
      summary: "Agent session initialized",
      sensitive: false,
    },
    {
      sequence: 3,
      timestamp: new Date(base.getTime() + 15000).toISOString(),
      entryID: "demo-ent-3",
      runID: "demo-run-1",
      sessionID: "demo-sess-1",
      agentID: "claude-code",
      backend: "pty",
      adapter: "claude",
      kind: "policy_evaluated",
      eventType: "permission",
      risk: "high",
      rule: "deny_sensitive_exec",
      decision: "deny",
      decisionBy: "policy",
      outcome: "in_flight",
      reason: "Command requires explicit operator authorization",
      summary: "curl execution intercepted",
      sensitive: true,
      metadata: { automatic: "false", mode: "enforce" },
    },
    {
      sequence: 4,
      timestamp: new Date(base.getTime() + 25000).toISOString(),
      entryID: "demo-ent-4",
      runID: "demo-run-1",
      sessionID: "demo-sess-1",
      agentID: "claude-code",
      backend: "pty",
      adapter: "claude",
      kind: "decision",
      eventType: "permission",
      decision: "allow",
      decisionBy: "human",
      outcome: "applied",
      reason: "decision_selected",
      sensitive: false,
    },
    {
      sequence: 5,
      timestamp: new Date(base.getTime() + 26000).toISOString(),
      entryID: "demo-ent-5",
      runID: "demo-run-1",
      sessionID: "demo-sess-1",
      agentID: "claude-code",
      backend: "pty",
      adapter: "claude",
      kind: "delivery",
      eventType: "permission",
      decision: "allow",
      decisionBy: "human",
      outcome: "succeeded",
      reason: "delivered_to_backend",
      sensitive: false,
    },
    {
      sequence: 6,
      timestamp: new Date(base.getTime() + 45000).toISOString(),
      entryID: "demo-ent-6",
      runID: "demo-run-1",
      sessionID: "demo-sess-2",
      agentID: "codex-cli",
      backend: "pty",
      adapter: "codex",
      kind: "policy_evaluated",
      eventType: "confirmation",
      risk: "low",
      rule: "auto_allow_git_status",
      decision: "allow",
      decisionBy: "policy",
      outcome: "applied",
      reason: "Matched automated read-only rule",
      summary: "git status auto-authorized",
      sensitive: false,
      metadata: { automatic: "true", mode: "enforce" },
    },
    {
      sequence: 7,
      timestamp: new Date(base.getTime() + 60000).toISOString(),
      entryID: "demo-ent-7",
      runID: "demo-run-1",
      sessionID: "demo-sess-2",
      agentID: "codex-cli",
      backend: "pty",
      adapter: "codex",
      kind: "operator_input",
      decisionBy: "human",
      outcome: "succeeded",
      reason: "line_submitted",
      sensitive: false,
    },
  ];
}
