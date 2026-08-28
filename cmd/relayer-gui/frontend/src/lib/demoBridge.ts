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
        description: "Assistant de développement en ligne de commande.",
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
        description: "Agent de code piloté depuis le terminal.",
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
        description: "CLI MiMo Code avec interception générique.",
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
        description: "CLI locale; run et le modèle restent explicites.",
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
        description: "Commande locale sous forme d’arguments exacts.",
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

function demoEvent(runID: string, sessionID: string, sensitive: boolean): SupervisionEvent {
  return {
    runID,
    id: `${sessionID}-prompt-1`,
    sessionID,
    agentID: sessionID,
    adapter: "generic",
    type: sensitive ? "credential" : "confirmation",
    summary: sensitive ? "Saisie confidentielle requise" : "Overwrite generated file? [Y/n]",
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
      throw new Error("Le run de démonstration est devenu obsolète.");
    }
  };

  const saveProfiles = (request: SaveAgentProfilesRequest): AgentProfilesView => {
    if (request.expectedRevision !== profiles.revision) {
      throw new Error("Configuration de démonstration obsolète.");
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
        executableLabel: input.argv[0] || "commande personnalisée",
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

  const agentsFromProfiles = (): AgentState[] => profiles.profiles.map((profile) => ({
    sessionID: profile.id,
    agentID: profile.id,
    name: profile.name,
    displayCommand: profile.executableLabel || "commande configurée",
    backend: profile.backend === "auto" ? "pty" : profile.backend,
    adapter: profile.adapter,
    status: "running",
    output: "Relayer demo — agent initialisé\n",
    revision: 1,
    running: true,
    attached: false,
    inputFrozen: false,
  }));

  window.setInterval(() => {
    if (state.runStatus !== "running") return;
    const eventRunID = state.runID;
    for (const agent of state.agents) {
      if (agent.status !== "running") continue;
      const next = (lineCounts.get(agent.sessionID) ?? 0) + 1;
      lineCounts.set(agent.sessionID, next);
      agent.output += `Génération · étape ${next.toString().padStart(2, "0")}\n`;
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
        const event = demoEvent(eventRunID, agent.sessionID, agent.sessionID === "demo-b");
        state.pendingEvents.push(event);
        agent.status = "waiting";
        agent.output += agent.sessionID === "demo-a"
          ? "Overwrite generated file? [Y/n]\n"
          : "Credential required:\n";
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
            summary: "La configuration enregistrée est valide.",
          },
          {
            id: "policy",
            scope: "policy",
            status: "pass",
            summary: "Le moteur de politiques est prêt.",
          },
          {
            id: "audit",
            scope: "audit",
            status: "pass",
            summary: "Le journal local peut être ouvert en mode sécurisé.",
          },
          {
            id: "backend",
            scope: "backend",
            status: "pass",
            summary: "Le backend configuré est disponible.",
          },
          {
            id: "adapter-compatibility",
            scope: "adapter",
            status: "warning",
            summary: "Un adapter expérimental demande une surveillance humaine.",
            remediation: "Conservez la politique par défaut sur ask.",
          },
        ],
      };
    },
    async submitDecision(runID, sessionID, eventID, _value) {
      requireRun(runID, true);
      const eventIndex = state.pendingEvents.findIndex(
        (event) => event.runID === runID && event.id === eventID && event.sessionID === sessionID,
      );
      if (eventIndex < 0) throw new Error("Événement de démonstration devenu obsolète.");
      state.pendingEvents.splice(eventIndex, 1);
      const agent = state.agents.find((candidate) => candidate.sessionID === sessionID);
      if (!agent) throw new Error("Session de démonstration introuvable.");
      agent.output += "Décision reçue et transmise.\nTâche terminée.\n";
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
    },
    async submitLine(runID, sessionID, _line) {
      requireRun(runID, true);
      const agent = state.agents.find((candidate) => candidate.sessionID === sessionID);
      if (!agent || !agent.running || agent.attached || agent.inputFrozen) {
        throw new Error("La session de démonstration n'accepte pas cette ligne.");
      }
      if (state.pendingEvents.some((event) => event.sessionID === sessionID)) {
        throw new Error("Une demande de supervision est en attente.");
      }
      agent.output += "Ligne locale transmise (contenu non conservé).\n";
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
      if (columns < 1 || rows < 1) throw new Error("Dimensions de terminal invalides.");
    },
    async stopSession(runID, sessionID) {
      requireRun(runID, true);
      const agent = state.agents.find((candidate) => candidate.sessionID === sessionID);
      if (!agent) throw new Error("Session de démonstration introuvable.");
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
        throw new Error("Un changement de run est déjà en cours.");
      }
      return saveProfiles(request);
    },
    async saveAgentProfilesAndRestart(request) {
      requireRun(request.expectedRunID);
      const restarting = state.runStatus === "running";
      if (!restarting && state.runStatus !== "idle" && state.runStatus !== "failed") {
        throw new Error("Un changement de run est déjà en cours.");
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
        throw new Error("Le run ne peut pas être arrêté dans cet état.");
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
