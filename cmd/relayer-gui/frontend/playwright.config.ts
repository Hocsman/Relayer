import { defineConfig, devices } from "@playwright/test";
import { existsSync } from "node:fs";

// Determine local browser channel (Chrome, Edge, or standard chromium)
function getBrowserChannel(): string | undefined {
  if (process.env.PLAYWRIGHT_CHANNEL) {
    return process.env.PLAYWRIGHT_CHANNEL;
  }
  if (
    existsSync("C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe") ||
    existsSync("/Applications/Google Chrome.app/Contents/MacOS/Google Chrome") ||
    existsSync("/usr/bin/google-chrome")
  ) {
    return "chrome";
  }
  if (
    existsSync("C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe") ||
    existsSync("/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge") ||
    existsSync("/usr/bin/microsoft-edge")
  ) {
    return "msedge";
  }
  return undefined;
}

export default defineConfig({
  testDir: "./e2e",
  timeout: 30000,
  expect: {
    timeout: 5000,
  },
  fullyParallel: false,
  workers: 1,
  reporter: [["list"]],
  use: {
    baseURL: "http://localhost:5174",
    headless: true,
    viewport: { width: 1440, height: 900 },
    trace: "on-first-retry",
  },
  projects: [
    {
      name: "Desktop Browser",
      use: {
        ...devices["Desktop Chrome"],
        channel: getBrowserChannel(),
      },
    },
  ],
  webServer: {
    command: "npm run dev:demo -- --port 5174",
    port: 5174,
    reuseExistingServer: !process.env.CI,
    timeout: 30000,
  },
});
