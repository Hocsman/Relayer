/** @vitest-environment jsdom */
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { DecisionModal } from "./DecisionModal";
import type { AgentState, SupervisionEvent } from "../types/relayer";

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

function agent(): AgentState {
  return {
    sessionID: "agent-a",
    agentID: "agent-a",
    name: "Agent A",
    displayCommand: "fixture-agent",
    backend: "pty",
    adapter: "codex",
    status: "waiting",
    output: "ready",
    revision: 1,
    running: true,
    attached: false,
  };
}

function event(overrides: Partial<SupervisionEvent> = {}): SupervisionEvent {
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
    deliveryStatus: "pending",
    decisions: ["allow", "deny"],
    ...overrides,
  };
}

function mount(value: SupervisionEvent) {
  const onDecide = vi.fn(async () => true);
  const onSubmit = vi.fn(async () => true);
  const onClose = vi.fn();
  act(() => {
    root.render(
      <DecisionModal
        event={value}
        agent={agent()}
        queueSize={1}
        onClose={onClose}
        onSubmit={onSubmit}
        onDecide={onDecide}
      />,
    );
  });
  return { onDecide, onSubmit, onClose };
}

async function press(key: string, options: KeyboardEventInit = {}) {
  await act(async () => {
    window.dispatchEvent(new KeyboardEvent("keydown", { key, bubbles: true, cancelable: true, ...options }));
    await Promise.resolve();
  });
}

function typeInto(value: string) {
  const input = container.querySelector<HTMLInputElement>("#manual-decision");
  if (!input) throw new Error("the manual field is not rendered");
  input.value = value;
}

// The shortcuts are the fastest way to answer, so they are also the fastest
// way to answer a prompt somebody else is already answering. The server
// refuses the second answer, but only after the operator believes it was
// theirs; and before v0.8.9 that refusal locked the prompt as indeterminate.
describe("DecisionModal shortcuts on a prompt that is not the operator's to answer", () => {
  it("still answers a person's prompt from the keyboard", async () => {
    const { onDecide } = mount(event());
    await press("Enter", { ctrlKey: true });
    expect(onDecide).toHaveBeenCalledWith("run-1", "agent-a", "prompt-1", "allow");
  });

  it("sends nothing on Ctrl+Enter while an answer is being delivered", async () => {
    const { onDecide, onSubmit } = mount(event({ deliveryStatus: "delivering" }));
    await press("Enter", { ctrlKey: true });
    typeInto("yes");
    await press("Enter", { ctrlKey: true });
    expect(onDecide).not.toHaveBeenCalled();
    expect(onSubmit).not.toHaveBeenCalled();
  });

  it("does not deny on Esc while an answer is being delivered, and only minimizes", async () => {
    const { onDecide, onClose } = mount(event({ deliveryStatus: "delivering" }));
    await press("Escape");
    expect(onDecide).not.toHaveBeenCalled();
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("sends nothing from the keyboard while the policy is about to answer", async () => {
    const automatic = event({
      evaluation: {
        action: "allow",
        proposedAction: "allow",
        ruleName: "safe-reads",
        reason: "rule_match",
        automatic: true,
        dryRun: false,
      },
    });
    const { onDecide, onSubmit, onClose } = mount(automatic);
    await press("Enter", { ctrlKey: true });
    typeInto("y");
    await press("Enter", { ctrlKey: true });
    await press("Escape");
    expect(onDecide).not.toHaveBeenCalled();
    expect(onSubmit).not.toHaveBeenCalled();
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("sends nothing from the form either while an answer is being delivered", async () => {
    const { onSubmit } = mount(event({ deliveryStatus: "delivering" }));
    typeInto("yes");
    const form = container.querySelector("form");
    await act(async () => {
      form?.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
      await Promise.resolve();
    });
    expect(onSubmit).not.toHaveBeenCalled();
  });
});
