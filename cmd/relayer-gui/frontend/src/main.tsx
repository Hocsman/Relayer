import React from "react";
import ReactDOM from "react-dom/client";
import { App, StartupFailure } from "./App";
import { createWailsBridge } from "./lib/bridge";
import { createDemoBridge } from "./lib/demoBridge";
import { createWebBridge } from "./lib/webBridge";
import { safeError } from "./lib/safety";
import "./styles.css";

const root = ReactDOM.createRoot(document.getElementById("root")!);

try {
  // Demo data is available only through the explicit development command.
  // When running inside Wails Desktop app, window.relayerBridge or window.go?.main?.App is present.
  // Otherwise, we are running in a standard web browser connected to relayer serve (webBridge).
  const demoEnabled = import.meta.env.DEV && import.meta.env.VITE_RELAYER_DEMO === "true";
  const hasWails = Boolean(window.relayerBridge || window.go?.main?.App);
  const bridge = demoEnabled
    ? createDemoBridge()
    : hasWails
    ? createWailsBridge()
    : createWebBridge();

  root.render(
    <React.StrictMode>
      <App bridge={bridge} />
    </React.StrictMode>,
  );
} catch (error) {
  root.render(<StartupFailure message={safeError(error, "The native bridge is unavailable.")} />);
}
