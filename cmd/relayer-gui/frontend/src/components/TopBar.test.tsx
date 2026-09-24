import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { TopBar } from "./TopBar";
import type { AppState, UserInfo } from "../types/relayer";

function sampleState(overrides: Partial<AppState> = {}): AppState {
  return {
    runID: "run-1",
    runStatus: "running",
    agents: [],
    pendingEvents: [],
    policy: {
      defaultAction: "ask",
      dryRun: false,
    },
    audit: {
      enabled: true,
      mode: "detailed",
      status: "ready",
    },
    ...overrides,
  };
}

describe("TopBar", () => {
  it("renders operator mode without viewer badge and shows stop run button", () => {
    const operatorInfo: UserInfo = {
      identity: "admin",
      connID: "conn-admin",
      role: "operator",
      readOnly: false,
    };
    const markup = renderToStaticMarkup(
      <TopBar
        state={sampleState()}
        userInfo={operatorInfo}
        onOpenAgents={() => {}}
        onOpenPreflight={() => {}}
        onOpenAudit={() => {}}
        onOpenObservability={() => {}}
        onOpenRecordings={() => {}}
        onRequestStop={() => {}}
      />,
    );

    expect(markup).not.toContain("VIEWER (READ-ONLY)");
    expect(markup).toContain("Stop the run");
  });

  it("renders viewer badge and hides stop run button when readOnly", () => {
    const viewerInfo: UserInfo = {
      identity: "viewer-bob",
      connID: "conn-bob",
      role: "viewer",
      readOnly: true,
    };
    const markup = renderToStaticMarkup(
      <TopBar
        state={sampleState()}
        userInfo={viewerInfo}
        onOpenAgents={() => {}}
        onOpenPreflight={() => {}}
        onOpenAudit={() => {}}
        onOpenObservability={() => {}}
        onOpenRecordings={() => {}}
        onRequestStop={() => {}}
      />,
    );

    expect(markup).toContain("VIEWER (READ-ONLY)");
    expect(markup).toContain("badge--viewer");
    expect(markup).toContain("viewer-bob");
    expect(markup).not.toContain("Stop the run");
  });
});

// "N pending" in the top bar is the number of requests waiting for a person.
// A prompt the server is answering by itself is not one of them; one whose
// automatic delivery failed is, because somebody has to resynchronize it.
describe("TopBar pending count", () => {
  function pendingPrompt(
    id: string,
    automatic: boolean,
    deliveryStatus: AppState["pendingEvents"][number]["deliveryStatus"] = "pending",
  ): AppState["pendingEvents"][number] {
    return {
      runID: "run-1",
      id,
      sessionID: "agent-a",
      agentID: "agent-a",
      adapter: "codex",
      type: "permission",
      summary: "Run it?",
      sensitive: false,
      risk: "low",
      timestamp: "2026-01-01T00:00:00Z",
      evaluation: {
        action: automatic ? "allow" : "ask",
        proposedAction: automatic ? "allow" : "ask",
        reason: automatic ? "rule_match" : "default_action",
        automatic,
        dryRun: false,
      },
      deliveryStatus,
    };
  }

  function pendingCount(pendingEvents: AppState["pendingEvents"]): string {
    const markup = renderToStaticMarkup(
      <TopBar
        state={sampleState({ pendingEvents })}
        onOpenAgents={() => {}}
        onOpenPreflight={() => {}}
        onOpenAudit={() => {}}
        onOpenObservability={() => {}}
        onOpenRecordings={() => {}}
        onRequestStop={() => {}}
      />,
    );
    return markup.match(/<strong>(\d+)<\/strong> pending/)?.[1] ?? "";
  }

  it("leaves out a prompt the policy is answering", () => {
    expect(pendingCount([pendingPrompt("auto", true), pendingPrompt("human", false)])).toBe("1");
    expect(pendingCount([pendingPrompt("auto", true, "delivering")])).toBe("0");
  });

  it("keeps an automatic prompt whose delivery became uncertain", () => {
    expect(pendingCount([pendingPrompt("auto", true, "uncertain")])).toBe("1");
  });
});
