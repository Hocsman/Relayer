import type {
  AgentProfilesView,
  AppState,
  BridgeEventMap,
  BridgeEventName,
  RelayerBridge,
  SupervisionEvent,
} from "../types/relayer";

type Listener = (payload: never) => void;

const startedAt = new Date().toISOString();

function initialState(): AppState {
  return {
    runID: "demo-local",
    runStatus: "running",
    startedAt,
    policy: { defaultAction: "ask", dryRun: false },
    audit: { enabled: true, mode: "metadata", status: "ready" },
    agents: [
      {
        sessionID: "demo-a",
        agentID: "demo-a",
        name: "Agent A · Claude",
        displayCommand: "claude",
        backend: "pty",
        adapter: "generic",
        status: "running",
        output: "Relayer demo — agent initialisé\n",
        revision: 1,
        running: true,
        attached: false,
      },
      {
        sessionID: "demo-b",
        agentID: "demo-b",
        name: "Agent B · Codex",
        displayCommand: "codex",
        backend: "tmux",
        adapter: "generic",
        status: "running",
        output: "Relayer demo — agent initialisé\n",
        revision: 1,
        running: true,
        attached: false,
      },
    ],
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
        adapter: "generic",
        adapterStatus: "stable",
        defaultArgv: ["claude"],
        requiresCustomArgv: false,
      },
      {
        id: "codex-cli",
        name: "Codex CLI",
        description: "Agent de code piloté depuis le terminal.",
        installStatus: "installed",
        installed: true,
        adapter: "generic",
        adapterStatus: "stable",
        defaultArgv: ["codex"],
        requiresCustomArgv: false,
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
      },
    ],
    profiles: [
      {
        id: "demo-a",
        name: "Agent A · Claude",
        presetID: "claude-code",
        cwd: "",
        backend: "pty",
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
        executableLabel: "codex",
        argumentCount: 0,
        locked: false,
        preserveOnSave: true,
      },
    ],
  };
}

function demoEvent(sessionID: string, sensitive: boolean): SupervisionEvent {
  return {
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

// The demo is only constructed when VITE_RELAYER_DEMO=true. Production never
// calls this function as a fallback when the native Wails bridge is missing.
export function createDemoBridge(): RelayerBridge {
  let state = initialState();
  let profiles = initialProfiles();
  const listeners = new Map<BridgeEventName, Set<Listener>>();
  const lineCounts = new Map(state.agents.map((agent) => [agent.sessionID, 0]));

  const emit = <K extends BridgeEventName>(event: K, payload: BridgeEventMap[K]) => {
    listeners.get(event)?.forEach((listener) => listener(payload as never));
  };

  const timer = window.setInterval(() => {
    for (const agent of state.agents) {
      if (agent.status !== "running") continue;
      const next = (lineCounts.get(agent.sessionID) ?? 0) + 1;
      lineCounts.set(agent.sessionID, next);
      agent.output += `Génération · étape ${next.toString().padStart(2, "0")}\n`;
      agent.revision += 1;
      emit("relayer:snapshot", {
        sessionID: agent.sessionID,
        revision: agent.revision,
        output: agent.output,
        status: agent.status,
        running: agent.running,
        attached: agent.attached,
      });

      const threshold = agent.sessionID === "demo-a" ? 8 : 12;
      if (next === threshold) {
        const event = demoEvent(agent.sessionID, agent.sessionID === "demo-b");
        state.pendingEvents.push(event);
        agent.status = "waiting";
        agent.output += agent.sessionID === "demo-a"
          ? "Overwrite generated file? [Y/n]\n"
          : "Credential required:\n";
        agent.revision += 1;
        emit("relayer:snapshot", {
          sessionID: agent.sessionID,
          revision: agent.revision,
          output: agent.output,
          status: agent.status,
          running: true,
          attached: false,
        });
        emit("relayer:event", event);
      }
    }
  }, 360);

  return {
    async getState() {
      return structuredClone(state);
    },
    async submitDecision(sessionID, eventID, _value) {
      const eventIndex = state.pendingEvents.findIndex(
        (event) => event.id === eventID && event.sessionID === sessionID,
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
        sessionID,
        revision: agent.revision,
        output: agent.output,
        status: agent.status,
        running: false,
        attached: false,
        exitCode: 0,
      });
    },
    async resizeSession(_sessionID, columns, rows) {
      if (columns < 1 || rows < 1) throw new Error("Dimensions de terminal invalides.");
    },
    async stopSession(sessionID) {
      const agent = state.agents.find((candidate) => candidate.sessionID === sessionID);
      if (!agent) throw new Error("Session de démonstration introuvable.");
      agent.status = "exited";
      agent.running = false;
      agent.exitCode = 130;
      state.pendingEvents = state.pendingEvents.filter((event) => event.sessionID !== sessionID);
      emit("relayer:status", { scope: "session", sessionID, status: "exited" });
    },
    async getAgentProfiles() {
      return structuredClone(profiles);
    },
    async saveAgentProfiles(request) {
      if (request.expectedRevision !== profiles.revision) {
        throw new Error("Configuration de démonstration obsolète.");
      }
      const revision = Number.parseInt(profiles.revision.replace("demo-", ""), 10) + 1;
      profiles = {
        ...profiles,
        revision: `demo-${revision}`,
        profiles: request.profiles.map((input) => {
          if (input.preserve) {
            const existing = profiles.profiles.find(
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
            argv: [...input.argv],
            executableLabel: input.argv[0] || "",
            argumentCount: Math.max(0, input.argv.length - 1),
            locked: false,
            preserveOnSave: false,
          };
        }),
        restartRequired: true,
      };
      return structuredClone(profiles);
    },
    async shutdown() {
      window.clearInterval(timer);
      state = { ...state, runStatus: "stopped" };
      emit("relayer:status", { scope: "run", status: "stopped" });
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
