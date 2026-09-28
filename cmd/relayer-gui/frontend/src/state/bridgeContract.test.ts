import { describe, expect, it } from "vitest";
import {
  initialRelayerState,
  normalizeState,
  relayerReducer,
} from "./relayerState";
import type { AppState } from "../types/relayer";

// The exact bytes the Go bridge emits for a freshly launched, idle
// application, pinned by TestBridgeStateContract on the Go side. The empty
// window of v0.8.9..v0.8.11 shipped because each side was tested against its
// own fixtures: this file is the one fixture both sides share. If Go changes
// what it emits, its test fails until the file is regenerated; if this page
// stops being able to read what Go emits, this test fails.
import idleBridgeState from "./testdata/bridge-state.idle.golden.json";
// The same for a run in progress, pinned by TestRunningBridgeStateContract:
// one agent waiting on a prompt, one running, one exited with a code.
import runningBridgeState from "./testdata/bridge-state.running.golden.json";

describe("the Go-to-interface bridge contract", () => {
  // The state an operator sees on every launch: no run, no agents. A null
  // where a list is expected threw before the first render and drew nothing
  // but the window's background.
  it("loads the real idle payload before the first render", () => {
    expect(idleBridgeState.runStatus).toBe("idle");
    expect(Array.isArray(idleBridgeState.agents)).toBe(true);
    expect(Array.isArray(idleBridgeState.pendingEvents)).toBe(true);

    const state = idleBridgeState as unknown as AppState;
    expect(() => normalizeState(state)).not.toThrow();
    const loaded = relayerReducer(initialRelayerState, { type: "loaded", state });
    expect(loaded.connection).toBe("ready");
    expect(loaded.app?.agents).toEqual([]);
    expect(loaded.app?.pendingEvents).toEqual([]);
  });

  it("keeps every agent and the waiting prompt of a run in progress", () => {
    const state = runningBridgeState as unknown as AppState;
    const loaded = relayerReducer(initialRelayerState, { type: "loaded", state });
    expect(loaded.connection).toBe("ready");
    expect(loaded.app?.runStatus).toBe("running");
    expect(loaded.app?.agents.map((agent) => [agent.sessionID, agent.status, agent.running])).toEqual([
      ["Agent-A", "waiting", true],
      ["Agent-B", "running", true],
      ["Agent-C", "failed", false],
    ]);
    expect(loaded.app?.agents[2].exitCode).toBe(3);
    // A prompt is kept only when it belongs to the run the page shows; a
    // payload whose prompts named another run would silently show none.
    expect(loaded.app?.pendingEvents.map((event) => [event.sessionID, event.id, event.deliveryStatus])).toEqual([
      ["Agent-A", "prompt-1", "pending"],
    ]);
  });
});
