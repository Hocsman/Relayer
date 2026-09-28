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
});
