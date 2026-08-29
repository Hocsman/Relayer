// Capture stills of the desktop interface for the README.
//
// The GIF in the README records the terminal interface with VHS. There is no
// equivalent for the desktop one: it is a WebView, not a terminal, so nothing
// that records a PTY can see it. This drives the demo bridge in headless Chrome
// over the DevTools protocol and writes PNGs.
//
// It uses only Node's built-in WebSocket, so it adds no dependency to a project
// that is deliberately careful about its supply chain. Chrome is found rather
// than installed; if it is missing the script says so and stops.
//
//   npm --prefix cmd/relayer-gui/frontend run dev:demo     # in one terminal
//   node cmd/relayer-gui/frontend/scripts/capture-demo.mjs # in another
import { spawn } from "node:child_process";
import { mkdtemp, mkdir, rm, writeFile } from "node:fs/promises";
import { existsSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const HERE = dirname(fileURLToPath(import.meta.url));
const OUTPUT = resolve(HERE, "../../../../docs");
const URL_UNDER_TEST = process.env.RELAYER_DEMO_URL ?? "http://localhost:5173";
const WIDTH = 1440;
const HEIGHT = 900;

const CHROME_CANDIDATES = [
  "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
  "/Applications/Chromium.app/Contents/MacOS/Chromium",
  "/usr/bin/google-chrome",
  "/usr/bin/chromium",
  "/usr/bin/chromium-browser",
];

function findChrome() {
  const explicit = process.env.CHROME_PATH;
  if (explicit) {
    if (!existsSync(explicit)) throw new Error(`CHROME_PATH does not exist: ${explicit}`);
    return explicit;
  }
  const found = CHROME_CANDIDATES.find((path) => existsSync(path));
  if (!found) {
    throw new Error(
      "No Chrome or Chromium found. Set CHROME_PATH to one, or install either; " +
        "this script only borrows a browser, it never installs one.",
    );
  }
  return found;
}

const sleep = (ms) => new Promise((done) => setTimeout(done, ms));

async function endpoint(port, attempts = 60) {
  for (let attempt = 0; attempt < attempts; attempt += 1) {
    try {
      const response = await fetch(`http://127.0.0.1:${port}/json/version`);
      if (response.ok) return (await response.json()).webSocketDebuggerUrl;
    } catch {
      // Chrome has not opened the port yet.
    }
    await sleep(250);
  }
  throw new Error("Chrome never exposed its DevTools endpoint");
}

function connect(url) {
  const socket = new WebSocket(url);
  const pending = new Map();
  let nextID = 0;
  const ready = new Promise((done, fail) => {
    socket.addEventListener("open", () => done());
    socket.addEventListener("error", () => fail(new Error("DevTools socket failed")));
  });
  socket.addEventListener("message", (event) => {
    const message = JSON.parse(event.data);
    const waiter = pending.get(message.id);
    if (!waiter) return;
    pending.delete(message.id);
    if (message.error) waiter.fail(new Error(message.error.message));
    else waiter.done(message.result);
  });
  const send = (method, params = {}, sessionId) =>
    new Promise((done, fail) => {
      const id = (nextID += 1);
      pending.set(id, { done, fail });
      socket.send(JSON.stringify({ id, method, params, sessionId }));
    });
  return { ready, send, close: () => socket.close() };
}

async function main() {
  const chrome = findChrome();
  const PROFILE = await mkdtemp(join(tmpdir(), "relayer-capture-"));
  const port = 9333;
  const browser = spawn(
    chrome,
    [
      "--headless=new",
      `--remote-debugging-port=${port}`,
      "--no-first-run",
      "--no-default-browser-check",
      "--disable-gpu",
      "--hide-scrollbars",
      `--window-size=${WIDTH},${HEIGHT}`,
      // A throwaway profile in the system temp directory: Chrome writes a few
      // hundred files of its own state, which have no business anywhere near
      // the repository.
      `--user-data-dir=${PROFILE}`,
      "about:blank",
    ],
    { stdio: "ignore" },
  );

  try {
    const client = connect(await endpoint(port));
    await client.ready;

    const { targetId } = await client.send("Target.createTarget", { url: "about:blank" });
    const { sessionId } = await client.send("Target.attachToTarget", { targetId, flatten: true });
    const call = (method, params) => client.send(method, params, sessionId);

    await call("Page.enable");
    await call("Runtime.enable");
    await call("Emulation.setDeviceMetricsOverride", {
      width: WIDTH,
      height: HEIGHT,
      deviceScaleFactor: 2,
      mobile: false,
    });
    await call("Page.navigate", { url: URL_UNDER_TEST });
    await sleep(2500);

    const evaluate = async (expression) => {
      const result = await call("Runtime.evaluate", { expression, awaitPromise: true });
      if (result.exceptionDetails) {
        throw new Error(result.exceptionDetails.exception?.description ?? "evaluate failed");
      }
      return result.result.value;
    };

    const isDemo = await evaluate(`!!document.querySelector('.application-shell')`);
    if (!isDemo) {
      throw new Error(
        `${URL_UNDER_TEST} did not render Relayer. Start it with: ` +
          "npm --prefix cmd/relayer-gui/frontend run dev:demo",
      );
    }

    await mkdir(OUTPUT, { recursive: true });
    const shoot = async (name) => {
      const { data } = await call("Page.captureScreenshot", { format: "png" });
      const path = resolve(OUTPUT, name);
      await writeFile(path, Buffer.from(data, "base64"));
      console.log(`wrote ${path}`);
    };

    const click = (label) =>
      evaluate(
        `(() => { const b = [...document.querySelectorAll('button')]` +
          `.find(x => x.textContent.trim() === ${JSON.stringify(label)}); ` +
          `if (!b) throw new Error('no button: ' + ${JSON.stringify(label)}); b.click(); return true; })()`,
      );

    const waitFor = async (selector, what) => {
      for (let attempt = 0; attempt < 80; attempt += 1) {
        if (await evaluate(`!!document.querySelector(${JSON.stringify(selector)})`)) return;
        await sleep(400);
      }
      throw new Error(`the demo never reached ${what}`);
    };

    await click("Configure the agents");
    await sleep(1200);
    await click("Start the agents");

    // The first prompt the demo raises is the confidential one, which masks its
    // own text by design and therefore shows the least. Dismiss it and wait for
    // the command approval, the prompt whose answers the adapter encodes — that
    // is the one worth putting on the front page.
    await waitFor(".decision-modal", "its first supervision prompt");
    await evaluate(
      `(() => { window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true })); return true; })()`,
    );
    await sleep(600);
    await shoot("gui-dashboard.png");

    await waitFor(".decision-actions", "a prompt the adapter can answer");
    await sleep(400);
    await shoot("gui-decision.png");

    client.close();
  } finally {
    browser.kill();
    // Chrome keeps writing its profile for a moment after the signal, so wait
    // for it to exit before removing the directory. Failing to clean a temp
    // directory must not fail a capture that already produced its images.
    await new Promise((done) => {
      browser.once("exit", done);
      setTimeout(done, 5000);
    });
    await rm(PROFILE, { recursive: true, force: true }).catch(() => {});
  }
}

main().catch((error) => {
  console.error(String(error.message ?? error));
  process.exitCode = 1;
});
