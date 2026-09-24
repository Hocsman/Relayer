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
  it("renders keyboard shortcut badges on Allow and Deny buttons", () => {
    const markup = render(event({ decisions: ["allow", "deny"] }));
    expect(markup).toContain("Ctrl+↵");
    expect(markup).toContain("Esc");
    expect(markup).toContain('class="button__shortcut"');
  });
});

// Once an answer is on its way, a second one can only be refused, or worse,
// typed into whatever the agent prints next. The server now delivers the
// policy's own answers, so "on its way" also covers an automatic prompt that
// is still pending: the server is about to decide it.
describe("DecisionModal while an answer is on its way", () => {
  function controls(markup: string) {
    const actions = markup.slice(markup.indexOf("decision-actions"));
    return {
      decisions: actions.slice(0, actions.indexOf("</div>")).match(/disabled/g)?.length ?? 0,
      input: /<input[^>]*id="manual-decision"[^>]*disabled/.test(markup),
      submit: markup.includes('type="submit" disabled'),
    };
  }

  const automatic = {
    action: "allow" as const,
    proposedAction: "allow" as const,
    ruleName: "safe-reads",
    reason: "rule_match",
    automatic: true,
    dryRun: false,
  };

  it("leaves a person's pending prompt answerable", () => {
    expect(controls(render(event({ decisions: ["allow", "deny"] })))).toEqual({
      decisions: 0,
      input: false,
      submit: false,
    });
  });

  it("disables every answer while the prompt is being delivered", () => {
    const markup = render(event({ decisions: ["allow", "deny"], deliveryStatus: "delivering" }));
    expect(controls(markup)).toEqual({ decisions: 2, input: true, submit: true });
    expect(markup).toContain("being delivered");
  });

  it("disables every answer while the policy is about to decide the prompt", () => {
    const markup = render(event({ decisions: ["allow", "deny"], evaluation: automatic }));
    expect(controls(markup)).toEqual({ decisions: 2, input: true, submit: true });
    expect(markup).toContain("Policy decision in progress");
    expect(markup).not.toContain("Human action required");
  });

  it("disables every answer while the policy's answer is being delivered", () => {
    const markup = render(event({
      decisions: ["allow", "deny"],
      evaluation: automatic,
      deliveryStatus: "delivering",
    }));
    expect(controls(markup)).toEqual({ decisions: 2, input: true, submit: true });
  });

  it("asks a person again once the policy hands the prompt back", () => {
    const markup = render(event({
      decisions: ["allow", "deny"],
      evaluation: { ...automatic, action: "ask", automatic: false, reason: "fallback_unsupported" },
    }));
    expect(controls(markup)).toEqual({ decisions: 0, input: false, submit: false });
    expect(markup).toContain("Human action required");
  });
});

describe("DecisionModal readOnly mode (Viewer)", () => {
  it("renders viewer notice and disables all arbitration controls when readOnly", () => {
    const markup = renderToStaticMarkup(
      <DecisionModal
        event={event({ decisions: ["allow", "deny"] })}
        agent={agent()}
        queueSize={1}
        readOnly={true}
        onClose={() => {}}
        onSubmit={async () => true}
        onDecide={async () => true}
      />,
    );
    expect(markup).toContain("decision-modal__viewer-notice");
    expect(markup).toContain("Mode Lecture Seule");
    expect(markup).toContain("Read-only (viewer)");
    // Both Allow and Deny buttons disabled
    const actions = markup.slice(markup.indexOf("decision-actions"));
    expect(actions.slice(0, actions.indexOf("</div>")).match(/disabled/g)?.length).toBe(2);
    // Submit button disabled
    expect(markup).toContain('type="submit" disabled');
  });
});

describe("DecisionModal tool call badge", () => {
  const call = {
    server: "fs",
    tool: "delete_file",
    risk: "high" as const,
    params: [{ name: "path", value: "/etc/passwd" }],
  };

  it("shows the tool a prompt is about, with its arguments", () => {
    const markup = render(event({ toolCall: call }));
    expect(markup).toContain("delete_file");
    expect(markup).toContain("/etc/passwd");
    expect(markup).toContain("tool-call--high");
  });

  it("renders no badge when the prompt is not a tool call", () => {
    expect(render(event({}))).not.toContain("tool-call__name");
  });

  it("suppresses the badge on a confidential prompt", () => {
    // A credential prompt's surroundings are exactly what must not be
    // reprinted; a badge built from them would undo the masking beside it.
    const markup = render(
      event({ sensitive: true, summary: "Enter your API token", toolCall: call }),
    );
    expect(markup).not.toContain("tool-call__name");
    expect(markup).not.toContain("/etc/passwd");
    expect(markup).toContain("Confidential input required");
  });
});
