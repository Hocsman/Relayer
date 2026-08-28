import { describe, expect, it } from "vitest";
import { nextProfileID, profilesForSave, validateAgentProfiles } from "./agentProfiles";
import type { AgentCatalogEntry, AgentProfile, AgentProfilesView } from "../types/relayer";

const catalog: AgentCatalogEntry[] = [
  {
    id: "claude-code",
    name: "Claude Code",
    description: "CLI",
    installStatus: "installed",
    installed: true,
    adapter: "claude",
    adapterStatus: "experimental",
    defaultArgv: ["claude"],
    requiresCustomArgv: false,
    minimumArguments: 0,
    argumentPrefix: [],
  },
  {
    id: "mimo-code",
    name: "MiMo Code",
    description: "CLI",
    installStatus: "not_installed",
    installed: false,
    adapter: "generic",
    adapterStatus: "stable",
    defaultArgv: ["mimo"],
    requiresCustomArgv: false,
    minimumArguments: 0,
    argumentPrefix: [],
  },
  {
    id: "ollama",
    name: "Ollama / DeepSeek",
    description: "local models",
    installStatus: "installed",
    installed: true,
    adapter: "generic",
    adapterStatus: "stable",
    defaultArgv: ["ollama", "run", ""],
    requiresCustomArgv: false,
    minimumArguments: 2,
    argumentPrefix: ["run"],
  },
  {
    id: "custom",
    name: "Custom",
    description: "argv",
    installed: true,
    installStatus: "unknown",
    adapter: "generic",
    adapterStatus: "stable",
    defaultArgv: [],
    requiresCustomArgv: true,
    minimumArguments: 0,
    argumentPrefix: [],
  },
];

const view: Pick<AgentProfilesView, "catalog" | "minProfiles" | "maxProfiles"> = {
  catalog,
  minProfiles: 1,
  maxProfiles: 8,
};

function profile(overrides: Partial<AgentProfile> = {}): AgentProfile {
  return {
    id: "claude",
    name: "Claude",
    presetID: "claude-code",
    cwd: "",
    backend: "auto",
    adapter: "generic",
    argv: ["claude"],
    locked: false,
    ...overrides,
  };
}

describe("agent profile validation", () => {
  it("accepts one valid exact-argv profile", () => {
    expect(validateAgentProfiles([profile()], view).valid).toBe(true);
    expect(validateAgentProfiles([profile({ adapter: "claude" })], view).valid).toBe(true);
  });

  it("rejects an adapter that does not belong to the selected catalogue profile", () => {
    const result = validateAgentProfiles([profile({ adapter: "codex" })], view);
    expect(result.valid).toBe(false);
    expect(result.profiles[0].presetID).toContain("Adapter incompatible");
  });

  it("rejects duplicate case-insensitive identifiers", () => {
    const result = validateAgentProfiles(
      [profile(), profile({ id: "claude", name: "Second" })],
      view,
    );
    expect(result.valid).toBe(false);
    expect(result.profiles.every((entry) => Boolean(entry.id))).toBe(true);
  });

  it.each([
    ["--api-key=do-not-save"],
    ["--auth"],
    ["token=do-not-save"],
    ["pk-fixturesecretvalue"],
    ["api-fixturesecretvalue"],
    ["eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.ABCDEFGHIJKLMNOP"],
    ["https://alice:secret@example.test"],
  ])("rejects secret-shaped argv %j", (argument) => {
    const result = validateAgentProfiles([profile({ argv: [argument] })], view);
    expect(result.profiles[0].argv).toContain("Do not put");
  });

  it("requires an explicit path when a catalog executable is not detected", () => {
    const result = validateAgentProfiles(
      [profile({ presetID: "mimo-code", argv: ["mimo"] })],
      view,
    );
    expect(result.valid).toBe(false);

    const explicit = validateAgentProfiles(
      [profile({ presetID: "mimo-code", argv: ["/opt/tools/mimo"] })],
      view,
    );
    expect(explicit.valid).toBe(true);
  });

  it("requires explicit Ollama arguments without inventing a model", () => {
    const missing = validateAgentProfiles(
      [profile({ presetID: "ollama", argv: ["ollama"] })],
      view,
    );
    expect(missing.valid).toBe(false);
    expect(missing.profiles[0].argv).toContain("chosen model");

    const blankModel = validateAgentProfiles(
      [profile({ presetID: "ollama", argv: ["ollama", "run", ""] })],
      view,
    );
    expect(blankModel.valid).toBe(false);
    expect(blankModel.profiles[0].argv).toContain("Set the subcommand");

    const wrongPrefix = validateAgentProfiles(
      [profile({ presetID: "ollama", argv: ["ollama", "serve", "model-selected-by-user"] })],
      view,
    );
    expect(wrongPrefix.valid).toBe(false);
    expect(wrongPrefix.profiles[0].argv).toContain("exact prefix");

    const explicit = validateAgentProfiles(
      [profile({ presetID: "ollama", argv: ["ollama", "run", "model-selected-by-user"] })],
      view,
    );
    expect(explicit.valid).toBe(true);
  });

  it("generates a stable unused ID", () => {
    expect(nextProfileID(catalog[0], [profile(), profile({ id: "claude-2" })])).toBe("claude-3");
  });

  it("accepts a locked profile without exposing argv", () => {
    const locked = profile({
      id: "Legacy.Agent",
      presetID: "custom",
      argv: undefined,
      locked: true,
      readOnlyReason: "advanced_environment",
      preserveOnSave: true,
    });
    expect(validateAgentProfiles([locked], view).valid).toBe(true);
    expect(locked.argv).toBeUndefined();
    expect(profilesForSave([locked])).toEqual([
      expect.objectContaining({ id: "Legacy.Agent", argv: [], preserve: true }),
    ]);
  });

  it("keeps an existing argv opaque until explicit replacement", () => {
    const masked = profile({
      argv: undefined,
      executableLabel: "claude",
      argumentCount: 3,
      preserveOnSave: true,
    });
    expect(validateAgentProfiles([masked], view).valid).toBe(true);
    expect(profilesForSave([masked])).toEqual([
      expect.objectContaining({ id: "claude", adapter: "generic", argv: [], preserve: true }),
    ]);
  });

  it("keeps an explicit generic adapter when a Claude command is replaced", () => {
    const replaced = profile({
      adapter: "generic",
      argv: ["claude", "--new-session"],
      preserveOnSave: false,
    });
    expect(validateAgentProfiles([replaced], view).valid).toBe(true);
    expect(profilesForSave([replaced])).toEqual([
      expect.objectContaining({
        presetID: "claude-code",
        adapter: "generic",
        argv: ["claude", "--new-session"],
        preserve: false,
      }),
    ]);
  });

  it("preserves a historical identifier while its argv remains opaque", () => {
    const historical = profile({
      id: "Reviewer.V1",
      argv: undefined,
      executableLabel: "custom command",
      preserveOnSave: true,
    });
    expect(validateAgentProfiles([historical], view).valid).toBe(true);
    expect(profilesForSave([historical])).toEqual([
      expect.objectContaining({ id: "Reviewer.V1", argv: [], preserve: true }),
    ]);
  });
});
