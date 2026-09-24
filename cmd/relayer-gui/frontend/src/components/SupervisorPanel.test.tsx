import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { SupervisorPanel } from "./SupervisorPanel";
import type { AppState, SupervisionEvent } from "../types/relayer";

function prompt(id: string, automatic: boolean, overrides: Partial<SupervisionEvent> = {}): SupervisionEvent {
  return {
    runID: "run-1",
    id,
    sessionID: "agent-a",
    agentID: "agent-a",
    adapter: "codex",
    type: "permission",
    summary: `Prompt ${id}`,
    sensitive: false,
    risk: "low",
    timestamp: "2026-01-01T00:00:00Z",
    evaluation: {
      action: automatic ? "allow" : "ask",
      proposedAction: automatic ? "allow" : "ask",
      ruleName: automatic ? "safe-reads" : undefined,
      reason: automatic ? "rule_match" : "default_action",
      automatic,
      dryRun: false,
    },
    deliveryStatus: "pending",
    ...overrides,
  };
}

function state(pendingEvents: SupervisionEvent[]): AppState {
  return {
    runID: "run-1",
    runStatus: "running",
    policy: { defaultAction: "ask", dryRun: false },
    audit: { enabled: true, mode: "metadata", status: "ready" },
    agents: [],
    pendingEvents,
  };
}

function render(pendingEvents: SupervisionEvent[]) {
  return renderToStaticMarkup(
    <SupervisorPanel state={state(pendingEvents)} errors={[]} onSelectEvent={() => {}} />,
  );
}

function queueCount(markup: string): string {
  return markup.match(/class="queue-count[^"]*">(\d+)</)?.[1] ?? "";
}

// The queue count is the number the operator acts on. A prompt the server is
// answering by itself is listed, so the operator can see it happen, but it is
// not a request waiting for them.
describe("SupervisorPanel with policy decisions in progress", () => {
  it("counts only the prompts waiting for a person", () => {
    const markup = render([prompt("auto", true), prompt("human", false)]);
    expect(queueCount(markup)).toBe("1");
    expect(markup).toContain("1 supervision request waiting for a person");
  });

  it("still lists the prompt the policy is answering, as the policy's", () => {
    const markup = render([prompt("auto", true, { deliveryStatus: "delivering" })]);
    expect(markup).toContain("Prompt auto");
    expect(markup).toContain("event-item--policy");
    expect(markup).toContain("Policy · Allow · delivering");
    expect(queueCount(markup)).toBe("0");
    expect(markup).not.toContain("queue-count--active");
  });

  it("says a queued policy answer has not been written yet", () => {
    expect(render([prompt("auto", true)])).toContain("Policy · Allow · queued");
  });

  it("counts an automatic prompt whose delivery failed, because a person must act", () => {
    const markup = render([prompt("auto", true, { deliveryStatus: "uncertain" })]);
    expect(queueCount(markup)).toBe("1");
    expect(markup).not.toContain("event-item--policy");
  });

  it("shows all clear only when nothing at all is pending", () => {
    expect(render([])).toContain("All clear");
    expect(render([prompt("auto", true)])).not.toContain("All clear");
  });
});
