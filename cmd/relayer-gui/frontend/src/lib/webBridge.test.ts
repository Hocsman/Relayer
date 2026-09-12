import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { createWebBridge } from "./webBridge";

class MockWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSING = 2;
  static CLOSED = 3;

  static instances: MockWebSocket[] = [];
  url: string;
  readyState: number = 0; // CONNECTING
  sentMessages: string[] = [];

  onopen: (() => void) | null = null;
  onclose: (() => void) | null = null;
  onerror: (() => void) | null = null;
  onmessage: ((event: { data: string }) => void) | null = null;

  constructor(url: string) {
    this.url = url;
    MockWebSocket.instances.push(this);
    // Simulate connection opening in next tick
    setTimeout(() => {
      this.readyState = 1; // OPEN
      if (this.onopen) this.onopen();
    }, 0);
  }

  send(data: string): void {
    this.sentMessages.push(data);
  }

  close(): void {
    this.readyState = 3; // CLOSED
    if (this.onclose) this.onclose();
  }

  receive(data: unknown): void {
    if (this.onmessage) {
      this.onmessage({ data: JSON.stringify(data) });
    }
  }
}

describe("webBridge", () => {
  let mockStore: Record<string, string> = {};

  beforeEach(() => {
    MockWebSocket.instances = [];
    mockStore = {};

    const mockLocalStorage = {
      getItem: (key: string) => mockStore[key] ?? null,
      setItem: (key: string, val: string) => {
        mockStore[key] = String(val);
      },
      removeItem: (key: string) => {
        delete mockStore[key];
      },
      clear: () => {
        mockStore = {};
      },
    };

    vi.stubGlobal("WebSocket", MockWebSocket);
    vi.stubGlobal("window", {
      location: {
        protocol: "http:",
        host: "localhost:8080",
        search: "",
        hash: "",
      },
    });
    vi.stubGlobal("localStorage", mockLocalStorage);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("extracts token from query parameters and stores in localStorage", () => {
    window.location.search = "?token=my-secret-token";
    createWebBridge();

    expect(localStorage.getItem("relayer_token")).toBe("my-secret-token");
    const ws = MockWebSocket.instances[0];
    expect(ws.url).toBe("ws://localhost:8080/api/ws?token=my-secret-token");
  });

  it("extracts token from localStorage if query parameter is empty", () => {
    localStorage.setItem("relayer_token", "stored-token");
    createWebBridge();

    const ws = MockWebSocket.instances[0];
    expect(ws.url).toBe("ws://localhost:8080/api/ws?token=stored-token");
  });

  it("handles RPC request and resolves with result", async () => {
    const bridge = createWebBridge({ token: "tok" });
    const ws = MockWebSocket.instances[0];

    // Wait for connection to open
    await new Promise((r) => setTimeout(r, 10));

    const statePromise = bridge.getState();

    // Verify message was sent
    expect(ws.sentMessages.length).toBe(1);
    const req = JSON.parse(ws.sentMessages[0]);
    expect(req.method).toBe("getState");
    expect(req.id).toBeDefined();

    // Send mock response
    ws.receive({
      id: req.id,
      result: { runID: "run-test", runStatus: "running", agents: [] },
    });

    const result = await statePromise;
    expect(result.runID).toBe("run-test");
    expect(result.runStatus).toBe("running");
  });

  it("rejects RPC promise on error response", async () => {
    const bridge = createWebBridge({ token: "tok" });
    const ws = MockWebSocket.instances[0];
    await new Promise((r) => setTimeout(r, 10));

    const preflightPromise = bridge.runPreflight();

    const req = JSON.parse(ws.sentMessages[0]);
    ws.receive({
      id: req.id,
      error: "Diagnostics unavailable",
    });

    await expect(preflightPromise).rejects.toThrow("Diagnostics unavailable");
  });

  it("dispatches broadcast events to registered listeners", async () => {
    const bridge = createWebBridge({ token: "tok" });
    const ws = MockWebSocket.instances[0];
    await new Promise((r) => setTimeout(r, 10));

    const listener = vi.fn();
    const unsubscribe = bridge.on("relayer:snapshot", listener);

    const snapshotPayload = {
      runID: "run-1",
      sessionID: "session-1",
      revision: 1,
      output: "test output",
      status: "running",
      running: true,
      attached: true,
    };

    ws.receive({
      event: "relayer:snapshot",
      payload: snapshotPayload,
    });

    expect(listener).toHaveBeenCalledWith(snapshotPayload);

    // Test unsubscribe
    unsubscribe();
    ws.receive({
      event: "relayer:snapshot",
      payload: snapshotPayload,
    });
    expect(listener).toHaveBeenCalledTimes(1);
  });

  it("queues requests before connection opens and flushes on open", async () => {
    const bridge = createWebBridge({ token: "tok" });
    const ws = MockWebSocket.instances[0];

    // While still CONNECTING
    expect(ws.readyState).toBe(0);
    const promise = bridge.getState();

    // After open event fires
    await new Promise((r) => setTimeout(r, 10));
    expect(ws.sentMessages.length).toBe(1);

    const req = JSON.parse(ws.sentMessages[0]);
    ws.receive({
      id: req.id,
      result: { runID: "run-flushed" },
    });

    const res = await promise;
    expect(res.runID).toBe("run-flushed");
  });
});
