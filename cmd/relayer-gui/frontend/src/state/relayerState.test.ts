import { describe, expect, it } from "vitest";
import {
  initialRelayerState,
  normalizeState,
  relayerReducer,
} from "./relayerState";
import type { AppState, SupervisionEvent } from "../types/relayer";

function pending(sessionID: string, id = "same-id"): SupervisionEvent {
  return {
    runID: "run-1",
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

  it("keeps permission prompts in the supervisor queue", () => {
    const state = appState();
    state.pendingEvents = [{ ...pending("a", "permission-id"), type: "permission" }];
    const normalized = normalizeState(state);
    expect(normalized.pendingEvents).toHaveLength(1);
    expect(normalized.pendingEvents[0].type).toBe("permission");
  });

  it("replaces a complete output snapshot even when semantic revision is unchanged", () => {
    const loaded = relayerReducer(initialRelayerState, { type: "loaded", state: appState() });
    const updated = relayerReducer(loaded, {
      type: "snapshot",
      snapshot: {
        runID: "run-1",
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
        runID: "run-1",
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
      runID: "run-1",
      sessionID: "a",
      eventID: "same-id",
      status: "delivered",
    });
    expect(updated.app?.pendingEvents.map((event) => event.sessionID)).toEqual(["b"]);
  });

  it("ignores every late event from a previous run", () => {
    const loaded = relayerReducer(initialRelayerState, { type: "loaded", state: appState() });

    const staleSnapshot = relayerReducer(loaded, {
      type: "snapshot",
      snapshot: {
        runID: "run-old",
        sessionID: "a",
        revision: 99,
        output: "stale output",
        status: "failed",
        running: false,
        attached: false,
      },
    });
    const staleEvent = relayerReducer(staleSnapshot, {
      type: "event",
      event: { ...pending("a", "late"), runID: "run-old" },
    });
    const staleStatus = relayerReducer(staleEvent, {
      type: "status",
      status: { runID: "run-old", scope: "run", status: "failed" },
    });
    const staleError = relayerReducer(staleStatus, {
      type: "error",
      error: {
        runID: "run-old",
        code: "late_error",
        message: "late",
        timestamp: "2026-01-01T00:00:01Z",
      },
    });
    const staleDelivery = relayerReducer(staleError, {
      type: "delivery",
      runID: "run-old",
      sessionID: "a",
      eventID: "same-id",
      status: "delivered",
    });

    expect(staleDelivery).toEqual(loaded);
  });

  it("drops pending events whose runID does not match the loaded state", () => {
    const normalized = normalizeState({
      ...appState(),
      pendingEvents: [pending("a"), { ...pending("b", "old"), runID: "run-old" }],
    });
    expect(normalized.pendingEvents).toEqual([pending("a")]);
  });
});
