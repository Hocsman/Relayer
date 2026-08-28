import type {
  AgentProfile,
  AgentProfilesView,
  AgentState,
  AppState,
  BridgeEventMap,
  BridgeEventName,
  RelayerBridge,
  PreflightReport,
  SaveAgentProfilesRequest,
  SupervisionEvent,
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
    async saveAgentProfiles(runID, request) {
      requireRun(runID);
      if (["starting", "restarting", "rollback", "stopping"].includes(state.runStatus)) {
        throw new Error("A run change is already in progress.");
      }
      return saveProfiles(request);
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
