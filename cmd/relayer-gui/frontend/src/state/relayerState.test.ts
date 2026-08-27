import { describe, expect, it } from "vitest";
import {
  initialRelayerState,
  normalizeState,
  relayerReducer,
} from "./relayerState";
import type { AppState, SupervisionEvent } from "../types/relayer";

function pending(sessionID: string, id = "same-id"): SupervisionEvent {
  return {
    id,
    sessionID,
    agentID: sessionID,
    adapter: "generic",
    type: "confirmation",
    summary: "Confirm?",
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
    deliveryStatus: "pending",
  };
}

function appState(): AppState {
  return {
    runID: "run-1",
    runStatus: "running",
    policy: { defaultAction: "ask", dryRun: false },
    audit: { enabled: false, mode: "off", status: "disabled" },
    agents: ["a", "b"].map((sessionID) => ({
      sessionID,
      agentID: sessionID,
      name: sessionID,
      displayCommand: "mock",
      backend: "pty",
      adapter: "generic",
      status: "running",
      output: "old",
      revision: 4,
      running: true,
      attached: false,
    })),
    pendingEvents: [pending("a"), pending("b")],
  };
}

describe("relayerReducer", () => {
  it("deduplicates events by session and occurrence ID", () => {
    const normalized = normalizeState(appState());
    expect(normalized.pendingEvents).toHaveLength(2);

    const loaded = relayerReducer(initialRelayerState, { type: "loaded", state: normalized });
    const updated = relayerReducer(loaded, {
      type: "event",
      event: { ...pending("a"), deliveryStatus: "failed" },
    });
    expect(updated.app?.pendingEvents).toHaveLength(2);
    expect(updated.app?.pendingEvents.find((event) => event.sessionID === "a")?.deliveryStatus).toBe("failed");
    expect(updated.app?.pendingEvents.find((event) => event.sessionID === "b")?.deliveryStatus).toBe("pending");
  });

  it("replaces a complete output snapshot even when semantic revision is unchanged", () => {
    const loaded = relayerReducer(initialRelayerState, { type: "loaded", state: appState() });
    const updated = relayerReducer(loaded, {
      type: "snapshot",
      snapshot: {
        sessionID: "a",
        revision: 4,
        output: "new output at equal revision",
        status: "running",
        running: true,
        attached: false,
      },
    });
    expect(updated.app?.agents[0].output).toBe("new output at equal revision");
  });

  it("ignores a snapshot older than the current semantic revision", () => {
    const loaded = relayerReducer(initialRelayerState, { type: "loaded", state: appState() });
    const updated = relayerReducer(loaded, {
      type: "snapshot",
      snapshot: {
        sessionID: "a",
        revision: 3,
        output: "stale",
        status: "running",
        running: true,
        attached: false,
      },
    });
    expect(updated.app?.agents[0].output).toBe("old");
  });

  it("updates delivery only for the composite key", () => {
    const loaded = relayerReducer(initialRelayerState, { type: "loaded", state: appState() });
    const updated = relayerReducer(loaded, {
      type: "delivery",
      sessionID: "a",
      eventID: "same-id",
      status: "delivered",
    });
    expect(updated.app?.pendingEvents.map((event) => event.sessionID)).toEqual(["b"]);
  });
});
