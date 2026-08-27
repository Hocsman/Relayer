import React from "react";
import ReactDOM from "react-dom/client";
import { App, StartupFailure } from "./App";
import { createWailsBridge } from "./lib/bridge";
import { createDemoBridge } from "./lib/demoBridge";
import { safeError } from "./lib/safety";
import "./styles.css";

const root = ReactDOM.createRoot(document.getElementById("root")!);

try {
  // Demo data is available only through the explicit development command.
  // A production build always requires the native bridge.
  const demoEnabled = import.meta.env.DEV && import.meta.env.VITE_RELAYER_DEMO === "true";
  const bridge = demoEnabled ? createDemoBridge() : createWailsBridge();
  root.render(
    <React.StrictMode>
      <App bridge={bridge} />
    </React.StrictMode>,
  );
} catch (error) {
  root.render(<StartupFailure message={safeError(error, "Bridge natif indisponible.")} />);
}
