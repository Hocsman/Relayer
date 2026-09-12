import { test, expect } from "@playwright/test";

test.describe("Relayer Desktop Supervisor E2E", () => {
  test.beforeEach(async ({ page }) => {
    await page.goto("/");
    await expect(page.locator(".application-shell")).toBeVisible();
  });

  test("displays top bar with brand, metrics and initial ready state", async ({ page }) => {
    await expect(page.locator(".brand strong")).toHaveText("Relayer");
    await expect(page.locator(".brand span").first()).toBeVisible();
    await expect(page.locator(".run-state--idle")).toHaveText(/Ready to start/i);
    await expect(page.locator(".button--health")).toBeVisible();
    await expect(page.locator(".button--metrics")).toBeVisible();
    await expect(page.locator(".button--agents")).toBeVisible();
    await expect(page.locator(".button--audit")).toBeVisible();
  });

  test("opens and closes the system health (preflight) panel", async ({ page }) => {
    await page.locator(".button--health").click();
    const modal = page.locator('section.preflight-panel[role="dialog"]');
    await expect(modal).toBeVisible();
    await expect(modal.locator("#preflight-title")).toHaveText("System health");

    // Preflight overview and check counts should load
    await expect(modal.locator(".preflight-overview")).toBeVisible();
    await expect(modal.locator(".preflight-counts")).toBeVisible();

    // Close via close icon button
    await modal.locator('button[aria-label="Close system health"]').click();
    await expect(modal).not.toBeVisible();
  });

  test("opens and closes the agents settings panel", async ({ page }) => {
    await page.locator(".button--agents").click();
    const modal = page.locator('section.agent-settings[role="dialog"]');
    await expect(modal).toBeVisible();
    await expect(modal.locator("#agents-title")).toHaveText("Agents");

    // Close via close icon button
    await modal.locator('button[aria-label="Close agents"]').click();
    await expect(modal).not.toBeVisible();
  });

  test("adds an agent from the catalog without crashing and saves successfully", async ({ page }) => {
    await page.locator(".button--agents").click();
    const modal = page.locator('section.agent-settings[role="dialog"]');
    await expect(modal).toBeVisible();

    // Wait for catalog list to be fully loaded
    await expect(modal.locator(".catalog-list")).toBeVisible({ timeout: 10000 });

    const profileCards = modal.locator(".profile-card");
    await expect(profileCards).toHaveCount(2);

    // Add Aider from catalog
    const addAiderButton = modal.locator('button[aria-label="Add Aider"]');
    await expect(addAiderButton).toBeVisible();
    await addAiderButton.click();

    await expect(profileCards).toHaveCount(3);
    const aiderCard = profileCards.nth(2);
    await expect(aiderCard).toContainText("Aider");
    await expect(aiderCard.locator('input[placeholder="executable"]')).toHaveValue("aider");

    // Add Goose CLI from catalog
    const addGooseButton = modal.locator('button[aria-label="Add Goose CLI"]');
    await expect(addGooseButton).toBeVisible();
    await addGooseButton.click();

    await expect(profileCards).toHaveCount(4);
    const gooseCard = profileCards.nth(3);
    await expect(gooseCard).toContainText("Goose CLI");
    await expect(gooseCard.locator('input[placeholder="executable"]')).toHaveValue("goose");

    // Save the configuration
    const saveButton = modal.getByRole("button", { name: "Save", exact: true });
    await expect(saveButton).toBeEnabled();
    await saveButton.click();

    // Verify notice appears
    await expect(modal.locator(".settings-notice")).toContainText(/Configuration saved/i);

    // Close agents settings
    await modal.locator('button[aria-label="Close agents"]').click();
    await expect(modal).not.toBeVisible();
  });

  test("full supervision lifecycle: start agents, stream terminal, intercept event, and submit decisions", async ({ page }) => {
    // 1. Open agent settings and start the agents
    await page.locator(".button--agents").click();
    const settingsModal = page.locator('section.agent-settings[role="dialog"]');
    await expect(settingsModal).toBeVisible();

    const startButton = settingsModal.locator('button:has-text("Start the agents")');
    await expect(startButton).toBeVisible();
    await startButton.click();

    // Wait for the settings panel to close and workspace to become active
    await expect(settingsModal).not.toBeVisible();
    await expect(page.locator(".run-state--running")).toBeVisible({ timeout: 10000 });

    // 2. Agents grid and terminal cards should be active
    const workspace = page.locator("main.workspace");
    await expect(workspace).toBeVisible();
    await expect(page.locator(".agent-grid")).toBeVisible();

    // Two agents should be visible: Claude and Codex
    await expect(page.locator(".agent-card")).toHaveCount(2);

    // 3. Wait for the first prompt modal to appear (demo-a sensitive credential prompt)
    const decisionModal = page.locator('section.decision-modal[role="dialog"]');
    await expect(decisionModal).toBeVisible({ timeout: 15000 });

    // demo-a prompt has a masked input for secret submission
    const secretInput = decisionModal.locator('input[name="relayer-manual-decision"]');
    await expect(secretInput).toBeVisible();
    await secretInput.fill("operator-secret-token");
    await decisionModal.locator('button[type="submit"]').click();

    // The modal should close after submitting
    await expect(decisionModal).not.toBeVisible();

    // 4. Wait for the second prompt (demo-b command authorization from Codex)
    await expect(decisionModal).toBeVisible({ timeout: 15000 });
    const allowButton = decisionModal.locator(".button--decision-allow");
    await expect(allowButton).toBeVisible();
    const denyButton = decisionModal.locator(".button--decision-deny");
    await expect(denyButton).toBeVisible();

    // Click Allow to authorize the intercepted command
    await allowButton.click();

    // Modal should close and session should complete
    await expect(decisionModal).not.toBeVisible();
  });

  test("controls per-agent lifecycle: stopping and restarting individual agents without affecting siblings", async ({ page }) => {
    // 1. Open agent settings and start the agents
    await page.locator(".button--agents").click();
    const settingsModal = page.locator('section.agent-settings[role="dialog"]');
    await expect(settingsModal).toBeVisible();
    await settingsModal.locator('button:has-text("Start the agents")').click();
    await expect(settingsModal).not.toBeVisible();
    await expect(page.locator(".run-state--running")).toBeVisible({ timeout: 10000 });

    const agentCards = page.locator(".agent-card");
    await expect(agentCards).toHaveCount(2);

    const firstAgent = agentCards.first();
    const secondAgent = agentCards.nth(1);

    // Stop the first agent
    const stopFirstButton = firstAgent.locator('button:has-text("Stop")');
    await expect(stopFirstButton).toBeVisible();
    await stopFirstButton.click();

    // First agent displays Start button
    const startFirstButton = firstAgent.locator('button:has-text("Start")');
    await expect(startFirstButton).toBeVisible({ timeout: 5000 });

    // Second agent remains running and unaffected
    await expect(secondAgent.locator('button:has-text("Stop")')).toBeVisible();

    // Restart the first agent
    await startFirstButton.click();
    await expect(firstAgent.locator('button:has-text("Stop")')).toBeVisible({ timeout: 5000 });
  });

  test("inspects audit journal, verifies cryptographic integrity, and filters entries", async ({ page }) => {
    await page.locator(".button--audit").click();
    const auditPanel = page.locator('section.audit-panel[role="dialog"]');
    await expect(auditPanel).toBeVisible();
    await expect(auditPanel.locator("#audit-title")).toHaveText("Audit Trail & Integrity");

    // Cryptographic verification card
    const verification = auditPanel.locator(".audit-verification");
    await expect(verification).toBeVisible();
    await expect(verification).toHaveClass(/audit-verification--passed/);
    await expect(verification).toContainText("Cryptographic Sequence Verified");

    // Metrics summary
    await expect(auditPanel.locator(".audit-metrics-grid")).toBeVisible();

    // Table of audit entries
    const table = auditPanel.locator(".audit-table");
    await expect(table).toBeVisible();
    const rows = auditPanel.locator(".audit-row");
    await expect(rows.first()).toBeVisible();

    // Expand first entry to inspect details
    const firstExpandButton = rows.first().locator(".audit-row__expand-button");
    await firstExpandButton.click();
    await expect(auditPanel.locator(".audit-row-detail")).toBeVisible();

    // Filter by kind
    const kindSelect = auditPanel.locator("#audit-filter-kind");
    await kindSelect.selectOption("policy_evaluated");
    // Ensure visible rows include policy_evaluated
    await expect(auditPanel.locator(".audit-badge--kind-policy_evaluated").first()).toBeVisible();

    // Close audit panel
    await auditPanel.locator('button[aria-label="Close audit panel"]').click();
    await expect(auditPanel).not.toBeVisible();
  });

  test("opens and closes the observability & live metrics panel, renders charts and exporter status", async ({ page }) => {
    await page.locator(".button--metrics").click();
    const metricsPanel = page.locator('section.observability-panel[role="dialog"]');
    await expect(metricsPanel).toBeVisible();
    await expect(metricsPanel.locator("#observability-title")).toHaveText("Observability & Metrics");

    // KPI grid should be rendered
    await expect(metricsPanel.locator(".observability-kpi-grid")).toBeVisible();
    await expect(metricsPanel.locator(".observability-kpi-card").first()).toBeVisible();

    // Decisions breakdown chart and latency histogram
    await expect(metricsPanel.locator('svg[aria-label="Decisions ratio chart"]')).toBeVisible();
    await expect(metricsPanel.locator('svg[aria-label="Reaction latency histogram chart"]')).toBeVisible();

    // Guardrail violations list
    await expect(metricsPanel.locator(".guardrails-list")).toBeVisible();

    // Telemetry Exporters status list
    await expect(metricsPanel.locator(".exporters-grid")).toBeVisible();
    await expect(metricsPanel.locator(".exporter-item").first()).toBeVisible();

    // Refresh button should be active and clickable
    const refreshBtn = metricsPanel.locator('button:has-text("Refresh")');
    await expect(refreshBtn).toBeVisible();
    await refreshBtn.click();

    // Close via close icon button
    await metricsPanel.locator('button[aria-label="Close observability panel"]').click();
    await expect(metricsPanel).not.toBeVisible();
  });
});

