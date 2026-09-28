/** @vitest-environment jsdom */
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "../App";
import { createDemoBridge } from "../lib/demoBridge";
import type { RelayerBridge, UpdateInfo } from "../types/relayer";

declare global {
  // eslint-disable-next-line no-var
  var IS_REACT_ACT_ENVIRONMENT: boolean;
}

let container: HTMLDivElement;
let root: Root;

beforeEach(() => {
  globalThis.IS_REACT_ACT_ENVIRONMENT = true;
  window.localStorage.clear();
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

const newer: UpdateInfo = { current: "0.8.14", latest: "0.9.0", available: true, disabled: false };

async function renderApp(bridge: RelayerBridge) {
  await act(async () => {
    root.render(<App bridge={bridge} />);
  });
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
}

function bannerButton(label: string) {
  return [...container.querySelectorAll<HTMLButtonElement>(".update-banner button")].find(
    (button) => button.textContent === label,
  );
}

describe("the update banner", () => {
  it("offers a newer release and opens the download the desktop validated", async () => {
    const openUpdate = vi.fn(async () => {});
    await renderApp({ ...createDemoBridge(), checkForUpdate: async () => newer, openUpdate });

    expect(container.querySelector(".update-banner")?.textContent).toContain("Relayer 0.9.0");
    await act(async () => bannerButton("Download")!.click());
    expect(openUpdate).toHaveBeenCalledTimes(1);
  });

  it("stays away for a release the user set aside, and comes back for the next one", async () => {
    await renderApp({ ...createDemoBridge(), checkForUpdate: async () => newer, openUpdate: async () => {} });
    await act(async () => bannerButton("Not now")!.click());
    expect(container.querySelector(".update-banner")).toBeNull();

    act(() => root.unmount());
    root = createRoot(container);
    await renderApp({ ...createDemoBridge(), checkForUpdate: async () => newer, openUpdate: async () => {} });
    expect(container.querySelector(".update-banner")).toBeNull();

    act(() => root.unmount());
    root = createRoot(container);
    const next = { ...newer, latest: "0.9.1" };
    await renderApp({ ...createDemoBridge(), checkForUpdate: async () => next, openUpdate: async () => {} });
    expect(container.querySelector(".update-banner")?.textContent).toContain("Relayer 0.9.1");
  });

  it("asks nothing once the user turned the check off", async () => {
    window.localStorage.setItem("relayer:updateCheck", "false");
    const checkForUpdate = vi.fn(async () => newer);
    await renderApp({ ...createDemoBridge(), checkForUpdate, openUpdate: async () => {} });
    expect(checkForUpdate).not.toHaveBeenCalled();
    expect(container.querySelector(".update-banner")).toBeNull();
  });

  it("shows nothing when there is no newer release, or the check failed", async () => {
    await renderApp({
      ...createDemoBridge(),
      checkForUpdate: async () => ({ ...newer, available: false, latest: "0.8.14" }),
    });
    expect(container.querySelector(".update-banner")).toBeNull();

    act(() => root.unmount());
    root = createRoot(container);
    await renderApp({ ...createDemoBridge(), checkForUpdate: async () => Promise.reject(new Error("offline")) });
    expect(container.querySelector(".update-banner")).toBeNull();
  });
});
