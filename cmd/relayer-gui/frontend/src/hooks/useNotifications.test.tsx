/** @vitest-environment jsdom */
import { act, useState } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { useNotifications } from "./useNotifications";
import type { BridgeEventMap, BridgeEventName, NotificationEvent, RelayerBridge } from "../types/relayer";

declare global {
  // eslint-disable-next-line no-var
  var IS_REACT_ACT_ENVIRONMENT: boolean;
}

let container: HTMLDivElement;
let root: Root;

beforeEach(() => {
  globalThis.IS_REACT_ACT_ENVIRONMENT = true;
  localStorage.clear();
  container = document.createElement("div");
  document.body.appendChild(container);
  root = createRoot(container);
});

afterEach(() => {
  act(() => root.unmount());
  container.remove();
  localStorage.clear();
});

function createMockBridge() {
  const listeners = new Map<string, Set<(payload: unknown) => void>>();
  const bridge = {
    on<K extends BridgeEventName>(event: K, listener: (payload: BridgeEventMap[K]) => void) {
      let set = listeners.get(event);
      if (!set) {
        set = new Set();
        listeners.set(event, set);
      }
      set.add(listener as (payload: unknown) => void);
      return () => {
        set?.delete(listener as (payload: unknown) => void);
      };
    },
    emit<K extends BridgeEventName>(event: K, payload: BridgeEventMap[K]) {
      listeners.get(event)?.forEach((l) => l(payload));
    },
  } as unknown as RelayerBridge & { emit<K extends BridgeEventName>(event: K, payload: BridgeEventMap[K]): void };

  return bridge;
}

describe("useNotifications", () => {
  it("receives relayer:notification events and manages toasts queue", () => {
    const bridge = createMockBridge();
    let hookResult: ReturnType<typeof useNotifications> | null = null;

    function TestComponent() {
      hookResult = useNotifications(bridge);
      return (
        <div>
          <span data-testid="count">{hookResult.toasts.length}</span>
          {hookResult.toasts.map((t) => (
            <div key={t.id} data-testid={`toast-${t.id}`}>
              {t.title}: {t.body}
            </div>
          ))}
        </div>
      );
    }

    act(() => {
      root.render(<TestComponent />);
    });

    expect(hookResult!.toasts.length).toBe(0);

    // Emit a notification
    act(() => {
      bridge.emit("relayer:notification", {
        title: "Relayer Arbitration",
        body: "Agent requires approval",
        agentName: "claude-1",
        sessionID: "sess-1",
        eventID: "ev-1",
        kind: "pending_decision",
        severity: "warning",
        reason: "destructive command",
        timestamp: new Date().toISOString(),
      });
    });

    expect(hookResult!.toasts.length).toBe(1);
    expect(hookResult!.toasts[0].title).toBe("Relayer Arbitration");
    expect(hookResult!.toasts[0].agentName).toBe("claude-1");
    expect(hookResult!.toasts[0].severity).toBe("warning");

    // Deduplicate same eventID
    act(() => {
      bridge.emit("relayer:notification", {
        title: "Relayer Arbitration",
        body: "Agent requires approval",
        agentName: "claude-1",
        sessionID: "sess-1",
        eventID: "ev-1",
        kind: "pending_decision",
        severity: "warning",
        timestamp: new Date().toISOString(),
      });
    });

    expect(hookResult!.toasts.length).toBe(1);

    // Add a second distinct notification
    act(() => {
      bridge.emit("relayer:notification", {
        title: "Guardrail Blocked",
        body: "File access forbidden",
        agentName: "codex-1",
        eventID: "ev-2",
        kind: "guardrail_blocked",
        severity: "critical",
        timestamp: new Date().toISOString(),
      });
    });

    expect(hookResult!.toasts.length).toBe(2);
    expect(hookResult!.toasts[0].eventID).toBe("ev-2"); // latest is prepended

    // Dismiss first toast
    act(() => {
      hookResult!.dismissToast("ev-2");
    });

    expect(hookResult!.toasts.length).toBe(1);
    expect(hookResult!.toasts[0].eventID).toBe("ev-1");
  });

  it("handles soundEnabled state and persists in localStorage", () => {
    const bridge = createMockBridge();
    let hookResult: ReturnType<typeof useNotifications> | null = null;

    function TestComponent() {
      hookResult = useNotifications(bridge);
      return null;
    }

    act(() => {
      root.render(<TestComponent />);
    });

    expect(hookResult!.soundEnabled).toBe(true);

    act(() => {
      hookResult!.setSoundEnabled(false);
    });

    expect(hookResult!.soundEnabled).toBe(false);
    expect(localStorage.getItem("relayer:soundEnabled")).toBe("false");

    act(() => {
      hookResult!.setSoundEnabled(true);
    });

    expect(hookResult!.soundEnabled).toBe(true);
    expect(localStorage.getItem("relayer:soundEnabled")).toBe("true");
  });
});
