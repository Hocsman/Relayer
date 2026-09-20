import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { PresenceStrip } from "./PresenceStrip";
import type { HandView, PresenceView } from "../types/relayer";

function roster(overrides: Partial<PresenceView> = {}): PresenceView {
  return {
    runID: "run-1",
    sessionID: "alpha",
    observerCount: 2,
    members: [
      {
        connID: "conn-me",
        identity: "alice",
        role: "operator",
        observing: true,
        holdsHand: true,
        requestingHand: false,
        since: "2026-09-20T10:00:00Z",
      },
      {
        connID: "conn-bob",
        identity: "bob smith",
        role: "viewer",
        observing: true,
        holdsHand: false,
        requestingHand: false,
        since: "2026-09-20T10:01:00Z",
      },
    ],
    ...overrides,
  };
}

function hand(overrides: Partial<HandView> = {}): HandView {
  return {
    runID: "run-1",
    sessionID: "alpha",
    state: "held",
    holderConnID: "conn-me",
    holderIdentity: "alice",
    ...overrides,
  };
}

describe("PresenceStrip", () => {
  it("renders nothing when nobody is present and the terminal is free", () => {
    const markup = renderToStaticMarkup(
      <PresenceStrip
        presence={roster({ members: [], observerCount: 0 })}
        hand={hand({ state: "free", holderConnID: undefined, holderIdentity: undefined })}
        selfConnID="conn-me"
      />,
    );
    expect(markup).toBe("");
  });

  it("tells the holder that they have the terminal", () => {
    const markup = renderToStaticMarkup(
      <PresenceStrip presence={roster()} hand={hand()} selfConnID="conn-me" />,
    );
    expect(markup).toContain("You have the terminal");
    expect(markup).not.toContain("presence-strip__hand--locked");
  });

  it("names the colleague holding the terminal for everyone else", () => {
    const markup = renderToStaticMarkup(
      <PresenceStrip presence={roster()} hand={hand()} selfConnID="conn-bob" />,
    );
    expect(markup).toContain("alice has the terminal");
    expect(markup).toContain("presence-strip__hand--locked");
  });

  it("excludes the viewer themselves from the observer avatars", () => {
    const markup = renderToStaticMarkup(
      <PresenceStrip presence={roster()} hand={hand()} selfConnID="conn-me" />,
    );
    // Only bob is an "other", rendered from the two words of his identity.
    expect(markup).toContain("BS");
    expect(markup).toContain("1 other watching");
    expect(markup).not.toContain(">AL<");
  });

  it("reports an empty room when nobody else is watching", () => {
    const solo = roster({
      members: [
        {
          connID: "conn-me",
          identity: "alice",
          role: "operator",
          observing: true,
          holdsHand: true,
          requestingHand: false,
          since: "2026-09-20T10:00:00Z",
        },
      ],
      observerCount: 1,
    });
    const markup = renderToStaticMarkup(
      <PresenceStrip presence={solo} hand={hand()} selfConnID="conn-me" />,
    );
    expect(markup).toContain("No one else watching");
  });

  it("marks a colleague who is asking for the terminal", () => {
    const asking = roster({
      members: [
        {
          connID: "conn-me",
          identity: "alice",
          role: "operator",
          observing: true,
          holdsHand: true,
          requestingHand: false,
          since: "2026-09-20T10:00:00Z",
        },
        {
          connID: "conn-bob",
          identity: "bob",
          role: "operator",
          observing: true,
          holdsHand: false,
          requestingHand: true,
          since: "2026-09-20T10:01:00Z",
        },
      ],
    });
    const markup = renderToStaticMarkup(
      <PresenceStrip
        presence={asking}
        hand={hand({ state: "requested", requesterConnID: "conn-bob", requesterIdentity: "bob" })}
        selfConnID="conn-me"
      />,
    );
    expect(markup).toContain("presence-strip__avatar--requesting");
    expect(markup).toContain("asking for the terminal");
  });

  it("renders without a roster when only the hand is known", () => {
    const markup = renderToStaticMarkup(
      <PresenceStrip hand={hand()} selfConnID="conn-me" />,
    );
    expect(markup).toContain("You have the terminal");
    expect(markup).toContain("No one else watching");
  });
});
