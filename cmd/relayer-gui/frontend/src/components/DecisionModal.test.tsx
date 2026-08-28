import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { DecisionModal } from "./DecisionModal";
import type { AgentState, SupervisionEvent } from "../types/relayer";

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
    ...overrides,
  };
}

function render(value: SupervisionEvent) {
  return renderToStaticMarkup(
    <DecisionModal
      event={value}
      agent={agent()}
      queueSize={1}
      onClose={() => {}}
      onSubmit={async () => true}
      onDecide={async () => true}
    />,
  );
}

// An Allow button on a prompt whose adapter has no verified bytes for accepting
// it promises a delivery that fails at the last step. The core reports what
// each occurrence accepts; the modal shows exactly that and nothing else.
describe("DecisionModal semantic answers", () => {
  it("offers both answers when the adapter encodes both", () => {
    const markup = render(event({ decisions: ["allow", "deny"] }));
    expect(markup).toContain("Allow");
    expect(markup).toContain("Deny");
    expect(markup).toContain("Or answer manually");
  });

  it("offers only the answer the occurrence accepts", () => {
    const markup = render(event({ decisions: ["deny"] }));
    expect(markup).toContain("Deny");
    expect(markup).not.toContain("Allow");
  });

  it("falls back to the manual field when the adapter encodes nothing", () => {
    const markup = render(event({ adapter: "generic", decisions: [] }));
    expect(markup).not.toContain("decision-actions");
    expect(markup).toContain("Answer to submit");
  });

  it("drops a value the interface does not recognise instead of rendering it", () => {
    const markup = render(
      event({ decisions: ["allow", "sudo" as unknown as "allow"] }),
    );
    expect(markup).toContain("Allow");
    expect(markup).not.toContain("sudo");
  });

  // A session whose delivery state is unknown may not receive anything more,
  // by any path.
  it("disables the semantic answers while delivery is indeterminate", () => {
    const markup = render(event({ decisions: ["allow", "deny"], deliveryStatus: "uncertain" }));
    const actions = markup.slice(markup.indexOf("decision-actions"));
    expect(actions.slice(0, actions.indexOf("</div>")).match(/disabled/g)?.length).toBe(2);
  });
});

// A decision taken from a one-line summary is taken blind. The pane tail is
// already on the agent card; the modal puts it where the choice is made — with
// the one exception the project already makes for confidential prompts.
describe("DecisionModal terminal context", () => {
  it("shows the tail of the pane that stopped", () => {
    const markup = render(event({ decisions: [] }));
    expect(markup).toContain("Terminal context");
    expect(markup).toContain("ready");
  });

  it("does not reprint a confidential prompt under a masked summary", () => {
    const markup = render(event({ sensitive: true, summary: "Enter your API token" }));
    expect(markup).not.toContain("Terminal context");
    expect(markup).not.toContain("Enter your API token");
    expect(markup).toContain("Confidential input required");
  });
});

// The permissive answer must not be the one the eye picks. button--primary is
// the loudest control in the application — a filled gradient with a glow — and
// it used to sit on Allow while Deny got a low-opacity tint. In a tool whose
// purpose is to make a person stop and choose, that is a defect, not a style.
describe("DecisionModal answer weighting", () => {
  it("gives Allow and Deny the same weight", () => {
    const markup = render(event({ decisions: ["allow", "deny"] }));
    expect(markup).toContain("button--decision-allow");
    expect(markup).toContain("button--decision-deny");
    expect(markup).not.toContain("button--primary");
  });

  // Exactly one primary action per dialog, and never the permissive one. With
  // no semantic answer the manual field is the only way to answer at all.
  it("keeps the manual submit primary only when the adapter offers nothing", () => {
    expect(render(event({ decisions: [] }))).toContain("button--primary");
    expect(render(event({ decisions: ["allow", "deny"] }))).toContain("button--ghost");
  });
});
