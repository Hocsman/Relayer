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
        onRequestStop={() => {}}
      />,
    );

    expect(markup).not.toContain("VIEWER (READ-ONLY)");
    expect(markup).toContain("Stop the run");
  });

  it("renders viewer badge and hides stop run button when readOnly", () => {
    const viewerInfo: UserInfo = {
      identity: "viewer-bob",
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
        onRequestStop={() => {}}
      />,
    );

    expect(markup).toContain("VIEWER (READ-ONLY)");
    expect(markup).toContain("badge--viewer");
    expect(markup).toContain("viewer-bob");
    expect(markup).not.toContain("Stop the run");
  });
});
