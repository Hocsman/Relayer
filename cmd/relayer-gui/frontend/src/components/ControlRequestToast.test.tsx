import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { ControlRequestToast } from "./ControlRequestToast";
import type { HandView } from "../types/relayer";

function request(overrides: Partial<HandView> = {}): HandView {
  return {
    runID: "run-1",
    sessionID: "alpha",
    state: "requested",
    holderConnID: "conn-me",
    holderIdentity: "alice",
    requesterConnID: "conn-bob",
    requesterIdentity: "bob",
    requestExpiresAt: "2026-09-20T10:05:00Z",
    ...overrides,
  };
}

describe("ControlRequestToast", () => {
  it("renders nothing when no one is asking", () => {
    const markup = renderToStaticMarkup(
      <ControlRequestToast requests={[]} onGrant={() => {}} onDecline={() => {}} />,
    );
    expect(markup).toBe("");
  });

  it("names the requester and the terminal they want", () => {
    const markup = renderToStaticMarkup(
      <ControlRequestToast requests={[request()]} onGrant={() => {}} onDecline={() => {}} />,
    );
    expect(markup).toContain("bob");
    expect(markup).toContain("alpha");
    expect(markup).toContain("Hand over");
    expect(markup).toContain("Keep it");
  });

  it("announces itself to assistive technology", () => {
    const markup = renderToStaticMarkup(
      <ControlRequestToast requests={[request()]} onGrant={() => {}} onDecline={() => {}} />,
    );
    expect(markup).toContain('role="alertdialog"');
    expect(markup).toContain('aria-live="assertive"');
  });

  it("falls back to a neutral label when the requester has no identity", () => {
    const markup = renderToStaticMarkup(
      <ControlRequestToast
        requests={[request({ requesterIdentity: undefined })]}
        onGrant={() => {}}
        onDecline={() => {}}
      />,
    );
    expect(markup).toContain("An operator");
  });

  it("renders one toast per contended session", () => {
    const markup = renderToStaticMarkup(
      <ControlRequestToast
        requests={[request(), request({ sessionID: "beta", requesterConnID: "conn-carol", requesterIdentity: "carol" })]}
        onGrant={() => {}}
        onDecline={() => {}}
      />,
    );
    expect(markup).toContain("alpha");
    expect(markup).toContain("beta");
    expect(markup).toContain("carol");
  });
});
