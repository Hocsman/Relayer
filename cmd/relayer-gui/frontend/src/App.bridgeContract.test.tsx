/** @vitest-environment jsdom */
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { App } from "./App";
import { createDemoBridge } from "./lib/demoBridge";
import type { AppState } from "./types/relayer";

// The payloads TestBridgeStateContract and TestRunningBridgeStateContract pin
// on the Go side. The empty window of v0.8.9..v0.8.11 was thrown while the
// page rendered, so reading the payload through the reducer is not enough:
// the whole page renders each one here, as it does when the desktop starts.
import idleBridgeState from "./state/testdata/bridge-state.idle.golden.json";
import runningBridgeState from "./state/testdata/bridge-state.running.golden.json";

declare global {
  // eslint-disable-next-line no-var
  var IS_REACT_ACT_ENVIRONMENT: boolean;
}

let container: HTMLDivElement;
let root: Root;

beforeEach(() => {
  globalThis.IS_REACT_ACT_ENVIRONMENT = true;
  // jsdom has no matchMedia, which the agents' xterm terminals read for the
  // pixel ratio when they open; a browser, and WebView2, always has it.
  window.matchMedia ??= ((query: string) => ({
    matches: false,
    media: query,
    onchange: null,
    addListener() {},
    removeListener() {},
    addEventListener() {},
    removeEventListener() {},
    dispatchEvent: () => false,
  })) as typeof window.matchMedia;
  // Nor ResizeObserver, which fits each terminal to its card.
  globalThis.ResizeObserver ??= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  };
  container = document.createElement("div");
  document.body.appendChild(container);
  root = createRoot(container);
});

afterEach(() => {
  act(() => root.unmount());
  container.remove();
});

async function renderWith(payload: unknown) {
  const bridge = { ...createDemoBridge(), getState: async () => payload as AppState };
  await act(async () => {
    root.render(<App bridge={bridge} />);
  });
  // Let the initial getState promise settle and the page re-render.
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
}

describe("the page renders what the Go bridge sends", () => {
  it("draws an idle application", async () => {
    await renderWith(idleBridgeState);
    expect(container.textContent).not.toBe("");
    expect(container.querySelector("header")).not.toBeNull();
  });

  it("draws a run in progress with its agents and its waiting prompt", async () => {
    await renderWith(runningBridgeState);
    const text = container.textContent ?? "";
    for (const agent of runningBridgeState.agents) {
      expect(text).toContain(agent.name);
    }
    expect(text).toContain(runningBridgeState.pendingEvents[0].summary);
  });
});
