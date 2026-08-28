import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { AgentCard } from "./AgentCard";
import { submitUncontrolledLine } from "../lib/lineInput";
import { initialRelayerState, relayerReducer } from "../state/relayerState";
import type { AgentState, AppState } from "../types/relayer";

function agent(): AgentState {
  return {
    sessionID: "agent-a",
    agentID: "agent-a",
    name: "Agent A",
    displayCommand: "fixture-agent",
    backend: "pty",
    adapter: "generic",
    status: "running",
    output: "ready",
    revision: 1,
    running: true,
    attached: false,
  };
}

function appState(value: AgentState): AppState {
  return {
    runID: "run-1",
    runStatus: "running",
    policy: { defaultAction: "ask", dryRun: false },
    audit: { enabled: true, mode: "metadata", status: "ready" },
    agents: [value],
    pendingEvents: [],
  };
}

describe("AgentCard safe line path", () => {
  it("clears a secret and renders a disabled composer immediately after uncertainty", async () => {
    const secret = "dom-line-secret-sentinel-91ca";
    const input = { value: secret };
    await expect(submitUncontrolledLine(input, async (line) => {
      expect(line).toBe(secret);
      throw new Error("static transport failure");
    })).rejects.toThrow("static transport failure");

    const loaded = relayerReducer(initialRelayerState, { type: "loaded", state: appState(agent()) });
    const uncertain = relayerReducer(loaded, {
      type: "error",
      error: {
        runID: "run-1",
        sessionID: "agent-a",
        code: "delivery_uncertain",
        message: "Livraison indéterminée.",
        timestamp: "2026-01-01T00:00:00Z",
      },
    });
    const frozenAgent = uncertain.app?.agents[0];
    expect(frozenAgent?.inputFrozen).toBe(true);
    if (!frozenAgent) throw new Error("fixture agent missing");

    const markup = renderToStaticMarkup(
      <AgentCard
        runID="run-1"
        agent={frozenAgent}
        onResize={async () => {}}
        onStop={async () => {}}
        onOpenEvent={() => {}}
        onSubmitLine={async () => {}}
      />,
    );
    expect(input.value).toBe("");
    expect(markup).toContain("disabled");
    expect(markup).toContain("Session gelée");
    expect(markup).not.toContain(secret);
    expect(JSON.stringify(uncertain)).not.toContain(secret);
  });
});
