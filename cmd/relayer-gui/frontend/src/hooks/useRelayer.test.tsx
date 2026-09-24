/** @vitest-environment jsdom */
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { useRelayer } from "./useRelayer";
import type {
  AppState,
  BridgeEventMap,
  BridgeEventName,
  RelayerBridge,
  SupervisionEvent,
} from "../types/relayer";

declare global {
  // eslint-disable-next-line no-var
  var IS_REACT_ACT_ENVIRONMENT: boolean;
}

let container: HTMLDivElement;
let root: Root;

beforeEach(() => {
  globalThis.IS_REACT_ACT_ENVIRONMENT = true;
  container = document.createElement("div");
  document.body.appendChild(container);
  root = createRoot(container);
});

afterEach(() => {
  act(() => root.unmount());
  container.remove();
});

function prompt(deliveryStatus: SupervisionEvent["deliveryStatus"] = "pending"): SupervisionEvent {
  return {
    runID: "run-1",
    id: "prompt-1",
    sessionID: "agent-a",
    agentID: "agent-a",
    adapter: "codex",
    type: "permission",
    summary: "Run the build command?",
    sensitive: false,
    risk: "unknown",
    timestamp: "2026-01-01T00:00:00Z",
    evaluation: {
      action: "ask",
      proposedAction: "ask",
      reason: "default_action",
      automatic: false,
      dryRun: false,
    },
    deliveryStatus,
    decisions: ["allow", "deny"],
  };
}

function appState(pendingEvents: SupervisionEvent[], runID = "run-1"): AppState {
  return {
    runID,
    runStatus: "running",
    policy: { defaultAction: "ask", dryRun: false },
    audit: { enabled: true, mode: "metadata", status: "ready" },
    agents: [{
      sessionID: "agent-a",
      agentID: "agent-a",
      name: "Agent A",
      displayCommand: "fixture-agent",
      backend: "pty",
      adapter: "codex",
      status: "waiting",
      output: "",
      revision: 1,
      running: true,
      attached: false,
    }],
    pendingEvents,
  };
}

interface FakeBridge extends RelayerBridge {
  emit<K extends BridgeEventName>(event: K, payload: BridgeEventMap[K]): void;
  reconnect(): void;
}

// states are what successive getState calls return; the last one repeats.
function fakeBridge(states: Array<AppState | Error>, options: { reconnects?: boolean } = {}): FakeBridge {
  const listeners = new Map<string, Set<(payload: unknown) => void>>();
  const reconnectListeners = new Set<() => void>();
  let calls = 0;
  const bridge = {
    getState: vi.fn(async () => {
      const next = states[Math.min(calls, states.length - 1)];
      calls += 1;
      if (next instanceof Error) throw next;
      return next;
    }),
    submitDecision: vi.fn(async () => {}),
    submitAutomaticDecision: vi.fn(async () => {}),
    on<K extends BridgeEventName>(event: K, listener: (payload: BridgeEventMap[K]) => void) {
      let set = listeners.get(event);
      if (!set) {
        set = new Set();
        listeners.set(event, set);
      }
      set.add(listener as (payload: unknown) => void);
      return () => set?.delete(listener as (payload: unknown) => void);
    },
    emit<K extends BridgeEventName>(event: K, payload: BridgeEventMap[K]) {
      listeners.get(event)?.forEach((listener) => listener(payload));
    },
    reconnect() {
      reconnectListeners.forEach((listener) => listener());
    },
    ...(options.reconnects
      ? {
          onReconnect(listener: () => void) {
            reconnectListeners.add(listener);
            return () => reconnectListeners.delete(listener);
          },
        }
      : {}),
  };
  return bridge as unknown as FakeBridge;
}

let hook: ReturnType<typeof useRelayer>;

function Probe({ bridge }: { bridge: RelayerBridge }) {
  hook = useRelayer(bridge);
  return null;
}

async function mount(bridge: RelayerBridge) {
  await act(async () => {
    root.render(<Probe bridge={bridge} />);
    await Promise.resolve();
  });
  await settle();
}

async function settle() {
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
}

function onlyPrompt() {
  return hook.state.app?.pendingEvents[0];
}

// A refused answer is not an uncertain delivery. The server decides what the
// prompt is now, and says so through getState: it may be the policy's answer
// in flight, one another operator sent, a prompt that is gone, or one still
// waiting. Locking it locally as "uncertain" froze a prompt the server was
// happily delivering, and left a ghost the operator could not clear.
describe("useRelayer after an answer the server did not confirm", () => {
  it("takes the server's view instead of locking the prompt as uncertain", async () => {
    const bridge = fakeBridge([appState([prompt()]), appState([prompt("delivering")])]);
    bridge.submitAutomaticDecision = vi.fn(async () => {
      throw new Error("a decision is already in progress for this agent");
    });
    await mount(bridge);

    let delivered: boolean | undefined;
    await act(async () => {
      delivered = await hook.submitAutomaticDecision("run-1", "agent-a", "prompt-1", "allow");
    });

    expect(delivered).toBe(false);
    expect(bridge.getState).toHaveBeenCalledTimes(2);
    expect(onlyPrompt()?.deliveryStatus).toBe("delivering");
    expect(hook.state.errors[0]).toMatchObject({ code: "decision_in_flight", sessionID: "agent-a" });
    expect(hook.state.errors[0].message).toMatch(/already being delivered/);
  });

  it("leaves the prompt answerable when the server says it is still waiting", async () => {
    const bridge = fakeBridge([appState([prompt()])]);
    bridge.submitDecision = vi.fn(async () => {
      throw new Error("RPC request submitDecision (id=7) timed out after 15000ms");
    });
    await mount(bridge);

    await act(async () => {
      await hook.submitDecision("run-1", "agent-a", "prompt-1", "y");
    });

    expect(onlyPrompt()?.deliveryStatus).toBe("pending");
    expect(hook.state.errors[0].code).toBe("decision_unconfirmed");
    expect(hook.state.errors[0].message).toMatch(/server's state/);
    // The raw transport text is not what the operator reads.
    expect(hook.state.errors[0].message).not.toMatch(/id=7/);
  });

  it("drops a prompt the server no longer has", async () => {
    const bridge = fakeBridge([appState([prompt()]), appState([])]);
    bridge.submitAutomaticDecision = vi.fn(async () => {
      throw new Error("request is no longer the awaited event");
    });
    await mount(bridge);

    await act(async () => {
      await hook.submitAutomaticDecision("run-1", "agent-a", "prompt-1", "deny");
    });

    expect(hook.state.app?.pendingEvents).toEqual([]);
    expect(hook.state.errors[0].code).toBe("decision_stale");
  });

  it("still says so plainly when the server reports the delivery indeterminate", async () => {
    const bridge = fakeBridge([appState([prompt()]), appState([prompt("uncertain")])]);
    bridge.submitAutomaticDecision = vi.fn(async () => {
      throw new Error("delivery state is indeterminate, stop the session before further input");
    });
    await mount(bridge);

    await act(async () => {
      await hook.submitAutomaticDecision("run-1", "agent-a", "prompt-1", "allow");
    });

    expect(onlyPrompt()?.deliveryStatus).toBe("uncertain");
    expect(hook.state.errors[0].code).toBe("decision_delivery_uncertain");
    expect(hook.state.errors[0].message).toMatch(/Stop or resynchronize the session/);
  });

  // With no server view at all, the prompt stays as it was while the answer
  // was on its way, which is locked: a reconnect or the next frame settles it.
  it("keeps the prompt locked when the server cannot be asked either", async () => {
    const bridge = fakeBridge([appState([prompt()]), new Error("socket closed")]);
    bridge.submitAutomaticDecision = vi.fn(async () => {
      throw new Error("socket closed");
    });
    await mount(bridge);

    await act(async () => {
      await hook.submitAutomaticDecision("run-1", "agent-a", "prompt-1", "allow");
    });

    expect(onlyPrompt()?.deliveryStatus).toBe("delivering");
  });
});

// Frames sent while the gateway's socket was down are lost for good, and the
// gateway drops frames for a client whose buffer is full. A "delivered" among
// them leaves a prompt on screen that the server has already answered.
describe("useRelayer resynchronization", () => {
  it("takes the server's state again after the gateway's socket reconnects", async () => {
    const bridge = fakeBridge([appState([prompt()]), appState([])], { reconnects: true });
    await mount(bridge);
    expect(hook.state.app?.pendingEvents).toHaveLength(1);

    await act(async () => {
      bridge.reconnect();
    });
    await settle();

    expect(bridge.getState).toHaveBeenCalledTimes(2);
    expect(hook.state.app?.pendingEvents).toEqual([]);
  });

  it("takes the server's state again when the run's status changes", async () => {
    const bridge = fakeBridge([appState([prompt()]), appState([], "run-2")]);
    await mount(bridge);

    await act(async () => {
      bridge.emit("relayer:status", { runID: "run-1", scope: "run", status: "running" });
    });
    await settle();

    expect(bridge.getState).toHaveBeenCalledTimes(2);
    expect(hook.state.app?.runID).toBe("run-2");
  });

  it("does not refetch on a session's own status", async () => {
    const bridge = fakeBridge([appState([prompt()])]);
    await mount(bridge);

    await act(async () => {
      bridge.emit("relayer:status", { runID: "run-1", scope: "session", sessionID: "agent-a", status: "running" });
    });
    await settle();

    expect(bridge.getState).toHaveBeenCalledTimes(1);
  });

  it("stops listening for reconnections once unmounted", async () => {
    const bridge = fakeBridge([appState([prompt()])], { reconnects: true });
    await mount(bridge);
    act(() => root.unmount());
    root = createRoot(container);

    bridge.reconnect();
    await settle();

    expect(bridge.getState).toHaveBeenCalledTimes(1);
  });
});
