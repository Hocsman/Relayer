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
    adapter: "generic",
    adapterStatus: "stable",
    defaultArgv: ["claude"],
    requiresCustomArgv: false,
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
    argv: ["claude"],
    locked: false,
    ...overrides,
  };
}

describe("agent profile validation", () => {
  it("accepts one valid exact-argv profile", () => {
    expect(validateAgentProfiles([profile()], view).valid).toBe(true);
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
    expect(result.profiles[0].argv).toContain("Ne placez pas");
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
      expect.objectContaining({ id: "claude", argv: [], preserve: true }),
    ]);
  });

  it("preserves a historical identifier while its argv remains opaque", () => {
    const historical = profile({
      id: "Reviewer.V1",
      argv: undefined,
      executableLabel: "commande personnalisée",
      preserveOnSave: true,
    });
    expect(validateAgentProfiles([historical], view).valid).toBe(true);
    expect(profilesForSave([historical])).toEqual([
      expect.objectContaining({ id: "Reviewer.V1", argv: [], preserve: true }),
    ]);
  });
});
