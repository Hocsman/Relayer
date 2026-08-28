import type {
  AgentCatalogEntry,
  AgentProfile,
  AgentProfileInput,
  AgentProfilesView,
} from "../types/relayer";

const agentIDPattern = /^[a-z][a-z0-9_-]{0,63}$/;
const sensitiveOptionPattern = /^(?:--?)?(?:access[-_]?key|api[-_]?key|auth|authentication|authorization|bearer|client[-_]?secret|cookie|credential|key|otp|passphrase|password|pin|private[-_]?key|secret|session|token)(?:=|$)/i;
const jwtPattern = /^eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}$/;
const credentialURLPattern = /^[a-z][a-z0-9+.-]*:\/\/[^\s/@:]+:[^\s/@]+@/i;
const keyLikePattern = /^(?:sk|pk|api)[-_][A-Za-z0-9_-]{12,}$/i;

export interface ProfileFieldErrors {
  id?: string;
  name?: string;
  cwd?: string;
  presetID?: string;
  argv?: string;
}

export interface AgentProfilesValidation {
  valid: boolean;
  global: string[];
  profiles: ProfileFieldErrors[];
}

export function cloneProfiles(profiles: AgentProfile[]): AgentProfile[] {
  return profiles.map((profile) => ({
    ...profile,
    argv: profile.argv ? [...profile.argv] : undefined,
  }));
}

export function profilesForSave(profiles: AgentProfile[]): AgentProfileInput[] {
  return profiles.map((profile) => ({
    id: profile.id,
    name: profile.name,
    presetID: profile.presetID,
    cwd: profile.cwd,
    backend: profile.backend,
    adapter: profile.adapter,
    argv: profile.preserveOnSave ? [] : [...(profile.argv ?? [])],
    preserve: profile.preserveOnSave === true,
  }));
}

export function validateAgentProfiles(
  profiles: AgentProfile[],
  view: Pick<AgentProfilesView, "catalog" | "minProfiles" | "maxProfiles">,
): AgentProfilesValidation {
  const minimum = Math.max(1, view.minProfiles || 1);
  const maximum = Math.min(8, Math.max(minimum, view.maxProfiles || 8));
  const global: string[] = [];
  const errors = profiles.map(() => ({} as ProfileFieldErrors));

  if (profiles.length < minimum || profiles.length > maximum) {
    global.push(`Configure between ${minimum} and ${maximum} agents.`);
  }

  const catalog = new Map(view.catalog.map((entry) => [entry.id, entry]));
  const identifiers = new Map<string, number>();
  profiles.forEach((profile, index) => {
    const id = profile.id.trim();
    if (id) {
      const normalized = id.toLocaleLowerCase();
      const previous = identifiers.get(normalized);
      if (previous !== undefined) {
        errors[index].id = `Identifier already used by agent ${previous + 1}.`;
        errors[previous].id = "This identifier is used more than once.";
      } else {
        identifiers.set(normalized, index);
      }
    }

    if (profile.locked) {
      // Locked profiles may represent shell, environment or adapter features
      // that this form cannot round-trip. Their argv is deliberately absent,
      // and their existing fields remain authoritative in Go.
      return;
    }

    if (!profile.preserveOnSave && !agentIDPattern.test(id)) {
      errors[index].id = "Use 1 to 64 characters: lowercase letters, digits, _ or -.";
    }

    const name = profile.name.trim();
    if (!name || name.length > 80 || name.includes("\u0000")) {
      errors[index].name = "The name must be 1 to 80 characters long.";
    }
    if (profile.cwd.length > 4096 || profile.cwd.includes("\u0000")) {
      errors[index].cwd = "The working directory is invalid or too long.";
    }

    const preset = catalog.get(profile.presetID);
    if (!preset) {
      errors[index].presetID = "Unknown catalogue selection.";
    } else if (profile.adapter !== "generic" && profile.adapter !== preset.adapter) {
      errors[index].presetID = "Adapter incompatible with this catalogue profile.";
    }

    if (profile.preserveOnSave) {
      // Existing commands are intentionally opaque. Go remains authoritative
      // until the user explicitly chooses to replace the entire argv vector.
      return;
    }

    const argv = profile.argv ?? [];
    if (argv.length < 1 || argv.length > 64) {
      errors[index].argv = "The command must contain 1 to 64 arguments.";
      return;
    }
    if (preset && argv.length < 1 + Math.max(0, preset.minimumArguments)) {
      errors[index].argv = "This profile requires explicit arguments, including the chosen model.";
      return;
    }
    if (!argv[0].trim()) {
      errors[index].argv = "The first argument must be an executable.";
      return;
    }
    if (argv.some((argument) => argument.length > 4096 || argument.includes("\u0000"))) {
      errors[index].argv = "One argument is invalid or too long.";
      return;
    }
    if (
      preset &&
      argv.slice(1, 1 + Math.max(0, preset.minimumArguments)).some((argument) => !argument.trim())
    ) {
      errors[index].argv = "Set the subcommand and the model explicitly.";
      return;
    }
    if (
      preset &&
      preset.argumentPrefix.some((argument, prefixIndex) => argv[prefixIndex + 1] !== argument)
    ) {
      errors[index].argv = "The command does not follow the exact prefix this profile requires.";
      return;
    }
    if (argv.some(looksSensitiveArgument)) {
      errors[index].argv = "Do not put a key, token, password or credential in argv.";
      return;
    }
    if (
      preset &&
      !preset.installed &&
      preset.defaultArgv.length > 0 &&
      argv[0] === preset.defaultArgv[0]
    ) {
      errors[index].argv = "Executable not detected. Install it or give an explicit path.";
    }
  });

  return {
    valid: global.length === 0 && errors.every((entry) => Object.keys(entry).length === 0),
    global,
    profiles: errors,
  };
}

export function nextProfileID(entry: AgentCatalogEntry, profiles: AgentProfile[]): string {
  const baseByPreset: Record<AgentCatalogEntry["id"], string> = {
    "claude-code": "claude",
    "codex-cli": "codex",
    "mimo-code": "mimo",
    ollama: "ollama",
    custom: "agent",
  };
  const base = baseByPreset[entry.id];
  const existing = new Set(profiles.map((profile) => profile.id.toLocaleLowerCase()));
  if (!existing.has(base)) return base;
  for (let suffix = 2; suffix <= 999; suffix += 1) {
    const candidate = `${base}-${suffix}`;
    if (!existing.has(candidate)) return candidate;
  }
  return `${base}-${profiles.length + 1}`;
}

function looksSensitiveArgument(argument: string): boolean {
  const value = argument.trim();
  return (
    sensitiveOptionPattern.test(value) ||
    jwtPattern.test(value) ||
    credentialURLPattern.test(value) ||
    keyLikePattern.test(value)
  );
}
