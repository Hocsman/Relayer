import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { useDialogKeyboard } from "../hooks/useDialogKeyboard";
import {
  cloneProfiles,
  nextProfileID,
  profilesForSave,
  validateAgentProfiles,
  type ProfileFieldErrors,
} from "../lib/agentProfiles";
import type {
  AgentCatalogEntry,
  AgentProfile,
  AgentProfilesView,
  LifecycleResult,
  RelayerBridge,
  RunStatus,
  SaveAgentProfilesAndRestartRequest,
  SaveAgentProfilesRequest,
  SupervisionEvent,
} from "../types/relayer";

interface AgentSettingsPanelProps {
  bridge: RelayerBridge;
  runID: string;
  runStatus: RunStatus;
  pendingEvents: SupervisionEvent[];
  onSave(runID: string, request: SaveAgentProfilesRequest): Promise<AgentProfilesView>;
  onSaveAndRestart(request: SaveAgentProfilesAndRestartRequest): Promise<LifecycleResult>;
  onClose(): void;
}

type Notice = { tone: "success" | "warning"; text: string };

export function AgentSettingsPanel({
  bridge,
  runID,
  runStatus,
  pendingEvents,
  onSave,
  onSaveAndRestart,
  onClose,
}: AgentSettingsPanelProps) {
  const [view, setView] = useState<AgentProfilesView>();
  const [draft, setDraft] = useState<AgentProfile[]>([]);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [activating, setActivating] = useState(false);
  const [closeConfirmation, setCloseConfirmation] = useState(false);
  const [restartConfirmation, setRestartConfirmation] = useState(false);
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<Notice>();

  useEffect(() => {
    let active = true;
    void bridge.getAgentProfiles().then(
      (loaded) => {
        if (!active) return;
        setView(loaded);
        setDraft(cloneProfiles(loaded.profiles).slice(0, 8));
        if (loaded.restartRequired) {
          setNotice({
            tone: "warning",
            text: "Configuration saved — it is not active yet.",
          });
        }
        setLoading(false);
      },
      () => {
        if (!active) return;
        setError("The agent configuration could not be loaded.");
        setLoading(false);
      },
    );
    return () => {
      active = false;
    };
  }, [bridge]);

  const validation = useMemo(
    () =>
      view
        ? validateAgentProfiles(draft, view)
        : { valid: false, global: [], profiles: [] },
    [draft, view],
  );
  const dirty = view
    ? JSON.stringify(draft) !== JSON.stringify(view.profiles)
    : false;
  const transitioning = ["starting", "restarting", "rollback", "stopping"].includes(runStatus);
  const canActivate = runStatus === "idle" || runStatus === "running";
  const busy = saving || activating || transitioning;

  const updateProfile = (index: number, next: AgentProfile) => {
    setDraft((current) => current.map((profile, profileIndex) =>
      profileIndex === index ? next : profile,
    ));
    setCloseConfirmation(false);
    setNotice(undefined);
    setError(undefined);
  };

  const addProfile = (entry: AgentCatalogEntry) => {
    if (!view?.editable || draft.length >= Math.min(8, view.maxProfiles)) return;
    const argv = entry.defaultArgv.length > 0 ? [...entry.defaultArgv] : [""];
    setDraft((current) => [
      ...current,
      {
        id: nextProfileID(entry, current),
        name: entry.id === "custom" ? "New agent" : entry.name,
        presetID: entry.id,
        cwd: "",
        backend: "auto",
        adapter: entry.adapter,
        argv,
        locked: false,
      },
    ]);
    setNotice(undefined);
  };

  const moveProfile = (index: number, direction: -1 | 1) => {
    const destination = index + direction;
    if (destination < 0 || destination >= draft.length) return;
    setDraft((current) => {
      const next = cloneProfiles(current);
      [next[index], next[destination]] = [next[destination], next[index]];
      return next;
    });
  };

  const removeProfile = (index: number) => {
    if (!view || draft.length <= Math.max(1, view.minProfiles)) return;
    setDraft((current) => current.filter((_, profileIndex) => profileIndex !== index));
    setNotice(undefined);
  };

  const dialogRef = useRef<HTMLElement>(null);

  const requestClose = () => {
    if (busy) return;
    if (dirty && !closeConfirmation) {
      setCloseConfirmation(true);
      return;
    }
    onClose();
  };

  useDialogKeyboard(dialogRef, { onClose: requestClose });

  const save = async () => {
    if (!view || !dirty || !validation.valid || busy) return;
    setSaving(true);
    setError(undefined);
    setNotice(undefined);
    try {
      const result = await onSave(runID, {
        expectedRevision: view.revision,
        profiles: profilesForSave(draft),
      });
      setView(result);
      setDraft(cloneProfiles(result.profiles));
      setNotice(result.restartRequired
        ? {
            tone: "warning",
            text: "Configuration saved — it will be applied at the next startup.",
          }
        : {
            tone: "success",
            text: "Configuration saved.",
          });
      setCloseConfirmation(false);
    } catch {
      // A stale CAS or a post-commit durability uncertainty rotates the
      // opaque revision token. Reload the authoritative snapshot so the next
      // save cannot loop forever with stale authority.
      try {
        const reloaded = await bridge.getAgentProfiles();
        setView(reloaded);
        setDraft(cloneProfiles(reloaded.profiles));
        setError("The configuration changed or its state was uncertain. The saved version has been reloaded.");
      } catch {
        setError("The agents could not be saved. No sensitive detail is shown.");
      }
    } finally {
      setSaving(false);
    }
  };

  const saveAndRestart = async () => {
    if (!view || !validation.valid || !view.editable || !canActivate || busy) return;
    setActivating(true);
    setRestartConfirmation(false);
    setCloseConfirmation(false);
    setError(undefined);
    setNotice(undefined);
    try {
      const result = await onSaveAndRestart({
        expectedRunID: runID,
        expectedRevision: view.revision,
        profiles: profilesForSave(draft),
      });
      setView(result.profiles);
      setDraft(cloneProfiles(result.profiles.profiles));
      if (result.outcome === "rolled_back") {
        setNotice({
          tone: "warning",
          text: "The new run did not start. The previous YAML was restored and the earlier plan was relaunched under a new run.",
        });
      } else {
        // The agents are running behind this panel. Leaving it open with a
        // green line makes the operator dismiss a dialog to reach the thing
        // they just asked for; the dashboard is the confirmation.
        setActivating(false);
        onClose();
        return;
      }
    } catch {
      try {
        const reloaded = await bridge.getAgentProfiles();
        setView(reloaded);
        setDraft(cloneProfiles(reloaded.profiles));
      } catch {
        // The lifecycle error below is deliberately complete without exposing
        // a native error or any command/configuration value.
      }
      setError("The run change failed. Decisions stay blocked until the engine is back in a safe state.");
    } finally {
      setActivating(false);
    }
  };

  const requestSaveAndRestart = () => {
    if (runStatus === "running") {
      setRestartConfirmation(true);
      return;
    }
    void saveAndRestart();
  };

  const activationLabel = activating
    ? runStatus === "running" ? "Restarting…" : "Starting…"
    : runStatus === "running"
      ? dirty ? "Save and restart" : "Restart the agents"
      : dirty ? "Save and start" : "Start the agents";

  return (
    <div className="agent-settings-layer">
      <section
        ref={dialogRef}
        className="agent-settings"
        role="dialog"
        aria-modal="true"
        aria-labelledby="agents-title"
      >
        <header className="agent-settings__header">
          <div>
            <span className="eyebrow">Local configuration</span>
            <h1 id="agents-title">Agents</h1>
            <p>Build a team of 1 to 8 CLIs with exact arguments.</p>
          </div>
          <button className="icon-button" type="button" onClick={requestClose} disabled={busy} aria-label="Close agents" autoFocus>
            ×
          </button>
        </header>

        {loading ? (
          <div className="agent-settings__loading">
            <span className="settings-spinner" aria-hidden="true" />
            Loading the catalog…
          </div>
        ) : !view ? (
          <div className="agent-settings__failure" role="alert">
            <strong>Catalog unavailable</strong>
            <p>{error || "The native bridge returned no configuration."}</p>
            <button className="button button--ghost" type="button" onClick={onClose}>Back</button>
          </div>
        ) : (
          <>
            <fieldset className="agent-settings__content" disabled={busy} aria-busy={busy}>
              <Catalog
                entries={view.catalog}
                count={draft.length}
                maximum={Math.min(8, view.maxProfiles)}
                editable={view.editable}
                onAdd={addProfile}
              />
              <section className="profile-editor" aria-label="Configured profiles">
                <header className="profile-editor__header">
                  <div>
                    <span className="eyebrow">Configured team</span>
                    <h2>{draft.length} agent{draft.length !== 1 ? "s" : ""}</h2>
                  </div>
                  <span className="profile-limit">{draft.length} / {Math.min(8, view.maxProfiles)}</span>
                </header>

                {validation.global.map((message) => (
                  <p className="settings-error" role="alert" key={message}>{message}</p>
                ))}

                {!view.editable && (
                  <p className="settings-error" role="alert">
                    This legacy configuration is read-only. Migrate it to <code>version: 1</code> before editing the agents.
                  </p>
                )}

                <div className="profile-list">
                  {draft.map((profile, index) => (
                    <ProfileCard
                      key={`${index}-${profile.id}`}
                      profile={profile}
                      index={index}
                      count={draft.length}
                      minimum={Math.max(1, view.minProfiles)}
                      catalog={view.catalog}
                      errors={validation.profiles[index] || {}}
                      onChange={(next) => updateProfile(index, next)}
                      onMove={(direction) => moveProfile(index, direction)}
                      onRemove={() => removeProfile(index)}
                    />
                  ))}
                </div>
              </section>
            </fieldset>

            <footer className="agent-settings__footer">
              <div className="settings-footer__status">
                <span className="settings-path" title={view.configPath}>{view.configPath}</span>
                <span>{dirty ? "Unsaved changes" : "Configuration in sync"}</span>
                {error && <strong className="settings-save-error" role="alert">{error}</strong>}
                {notice && (
                  <strong
                    className={`settings-notice settings-notice--${notice.tone}`}
                    role="status"
                  >
                    {notice.text}
                  </strong>
                )}
                {view.restartRequired && (
                  <span className="restart-guidance">
                    {runStatus === "running"
                      ? "Restart the agents to apply the saved configuration."
                      : "Start the agents to apply the saved configuration."}
                  </span>
                )}
                {runStatus === "failed" && runID !== "" && (
                  <span className="restart-guidance">
                    The shutdown state is uncertain. Close Relayer and check the local sessions before starting again.
                  </span>
                )}
              </div>
              <div className="settings-footer__actions">
                {closeConfirmation && (
                  <span className="inline-confirm">
                    Discard the changes?
                    <button className="button button--ghost" type="button" onClick={onClose}>Discard</button>
                  </span>
                )}
                <button
                  className="button button--ghost"
                  type="button"
                  disabled={!view.editable || !dirty || !validation.valid || busy}
                  onClick={() => {
                    setCloseConfirmation(false);
                    void save();
                  }}
                >
                  {saving ? "Saving…" : "Save"}
                </button>
                <button
                  className="button button--primary"
                  type="button"
                  disabled={!view.editable || !validation.valid || !canActivate || busy}
                  onClick={requestSaveAndRestart}
                >
                  {activationLabel}
                </button>
              </div>
            </footer>
          </>
        )}
      </section>
      {restartConfirmation && (
        <div className="settings-confirmation-layer" role="presentation">
          <section
            className="lifecycle-confirmation"
            role="alertdialog"
            aria-modal="true"
            aria-labelledby="restart-run-title"
          >
            <span className="eyebrow">Replacing the current run</span>
            <h2 id="restart-run-title">Restart the agents?</h2>
            <p>
              The supervised sessions will be stopped outright. tmux persistence is ignored for this explicit restart.
              Pending requests are never carried over to the new run.
            </p>
            {pendingEvents.length > 0 && (
              <strong className="lifecycle-confirmation__warning">
                {pendingEvents.length} pending request{pendingEvents.length !== 1 ? "s" : ""},
                including {pendingEvents.filter((event) => event.deliveryStatus === "delivering").length} being delivered.
              </strong>
            )}
            <p>
              If the new launch fails, Relayer will try to relaunch the previously active configuration under a new runID.
            </p>
            <div className="lifecycle-confirmation__actions">
              <button className="button button--ghost" type="button" onClick={() => setRestartConfirmation(false)}>
                Cancel
              </button>
              <button className="button button--danger" type="button" disabled={busy} onClick={() => void saveAndRestart()}>
                Restart
              </button>
            </div>
          </section>
        </div>
      )}
    </div>
  );
}

function Catalog({
  entries,
  count,
  maximum,
  editable,
  onAdd,
}: {
  entries: AgentCatalogEntry[];
  count: number;
  maximum: number;
  editable: boolean;
  onAdd(entry: AgentCatalogEntry): void;
}) {
  return (
    <aside className="agent-catalog" aria-label="Agent catalog">
      <header>
        <span className="eyebrow">Catalog</span>
        <h2>Add a CLI</h2>
        <p>The installation badges are detected by the local engine.</p>
      </header>
      <div className="catalog-list">
        {entries.map((entry) => (
          <article className="catalog-card" key={entry.id}>
            <div className={`catalog-logo catalog-logo--${entry.id}`} aria-hidden="true">
              {catalogInitial(entry.id)}
            </div>
            <div className="catalog-card__body">
              <h3>{entry.name}</h3>
              <p>{entry.description}</p>
              <div className="catalog-badges">
                <span className={`catalog-badge ${entry.id === "custom" ? "" : entry.installed ? "catalog-badge--installed" : "catalog-badge--missing"}`}>
                  {entry.id === "custom" ? "Free-form command" : entry.installed ? "Installed" : "Not detected"}
                </span>
                <span className="catalog-badge">{entry.adapter} · {entry.adapterStatus}</span>
              </div>
            </div>
            <button
              className="catalog-add"
              type="button"
              disabled={!editable || count >= maximum}
              onClick={() => onAdd(entry)}
              aria-label={`Add ${entry.name}`}
            >
              +
            </button>
          </article>
        ))}
      </div>
      <p className="catalog-security">
        The exact argv, model identifier included, is saved in the local YAML. Relayer
        never infers the model. The secret filter is conservative and heuristic: put no key,
        environment variable or credential in these arguments.
      </p>
    </aside>
  );
}

function ProfileCard({
  profile,
  index,
  count,
  minimum,
  catalog,
  errors,
  onChange,
  onMove,
  onRemove,
}: {
  profile: AgentProfile;
  index: number;
  count: number;
  minimum: number;
  catalog: AgentCatalogEntry[];
  errors: ProfileFieldErrors;
  onChange(profile: AgentProfile): void;
  onMove(direction: -1 | 1): void;
  onRemove(): void;
}) {
  const preset = catalog.find((entry) => entry.id === profile.presetID);
  const patch = <K extends keyof AgentProfile>(field: K, value: AgentProfile[K]) =>
    onChange({ ...profile, [field]: value });

  const changePreset = (presetID: AgentProfile["presetID"]) => {
    const previous = catalog.find((entry) => entry.id === profile.presetID);
    const next = catalog.find((entry) => entry.id === presetID);
    const currentArgv = profile.argv ?? [];
    const unchangedDefault = Boolean(previous) &&
      JSON.stringify(currentArgv) === JSON.stringify(previous?.defaultArgv);
    onChange({
      ...profile,
      presetID,
      adapter: next?.adapter ?? "generic",
      preserveOnSave: false,
      argv: unchangedDefault || currentArgv.every((argument) => !argument)
        ? [...(next?.defaultArgv.length ? next.defaultArgv : [""])]
        : currentArgv,
    });
  };

  const updateArgument = (argumentIndex: number, value: string) => {
    const argv = [...(profile.argv ?? [])];
    argv[argumentIndex] = value;
    patch("argv", argv);
  };

  return (
    <article className={`profile-card${Object.keys(errors).length ? " profile-card--invalid" : ""}${profile.locked ? " profile-card--locked" : ""}`}>
      <header className="profile-card__header">
        <div className="profile-order">{index + 1}</div>
        <div>
          <h3>{profile.name || "Unnamed agent"}</h3>
          <span>
            {preset?.name || "Unknown selection"} · {profile.adapter || (profile.readOnlyReason === "advanced_adapter" ? "advanced adapter" : "unknown adapter")}
          </span>
        </div>
        <div className="profile-card__controls">
          <button type="button" onClick={() => onMove(-1)} disabled={profile.locked || index === 0} aria-label="Move this agent up">↑</button>
          <button type="button" onClick={() => onMove(1)} disabled={profile.locked || index === count - 1} aria-label="Move this agent down">↓</button>
          <button className="profile-remove" type="button" onClick={onRemove} disabled={profile.locked || count <= minimum} aria-label="Remove this agent">×</button>
        </div>
      </header>

      {profile.locked ? (
        <div className="profile-lock" role="note">
          <span aria-hidden="true">⌁</span>
          <div>
            <strong>Read-only advanced profile</strong>
            <p>{readOnlyReasonLabel(profile.readOnlyReason)}</p>
            <small>{profile.id} · {profile.backend}</small>
          </div>
        </div>
      ) : (
        <>

      <div className="profile-fields">
        <Field label="Name" error={errors.name}>
          <input value={profile.name} maxLength={80} onChange={(event) => patch("name", event.target.value)} />
        </Field>
        <Field label="Identifier" error={errors.id}>
          <input
            value={profile.id}
            maxLength={64}
            spellCheck={false}
            disabled={profile.preserveOnSave}
            title={profile.preserveOnSave ? "Replace the command first to change the identifier." : undefined}
            onChange={(event) => patch("id", event.target.value)}
          />
        </Field>
        <Field label="Catalog" error={errors.presetID}>
          <select value={profile.presetID} onChange={(event) => changePreset(event.target.value as AgentProfile["presetID"])}>
            {catalog.map((entry) => <option key={entry.id} value={entry.id}>{entry.name}</option>)}
          </select>
        </Field>
        <Field label="Backend">
          <select value={profile.backend} onChange={(event) => patch("backend", event.target.value as AgentProfile["backend"])}>
            <option value="auto">Auto</option>
            <option value="pty">PTY</option>
            <option value="tmux">tmux</option>
          </select>
        </Field>
        <Field label="Working directory" error={errors.cwd} wide>
          <input
            value={profile.cwd}
            maxLength={4096}
            spellCheck={false}
            placeholder="Empty = default directory"
            onChange={(event) => patch("cwd", event.target.value)}
          />
        </Field>
      </div>

      {profile.preserveOnSave ? (
        <div className="argv-editor argv-editor--masked" role="note">
          <div>
            <strong>Existing command hidden</strong>
            <span>
              {profile.executableLabel || "Configured command"} · {profile.argumentCount ?? 0} argument{(profile.argumentCount ?? 0) !== 1 ? "s" : ""}
            </span>
            <small>The existing argv values are never sent to the WebView.</small>
          </div>
          <button
            type="button"
            onClick={() => onChange({
              ...profile,
              preserveOnSave: false,
              argv: [...(preset?.defaultArgv.length ? preset.defaultArgv : [""])],
            })}
          >
            Replace the command
          </button>
        </div>
      ) : (
      <div className="argv-editor">
        <div className="argv-editor__title">
          <div>
            <strong>Exact arguments</strong>
            <span>No shell, expansion or interpolation.</span>
          </div>
          <button
            type="button"
            disabled={(profile.argv?.length ?? 0) >= 64}
            onClick={() => patch("argv", [...(profile.argv ?? []), ""])}
          >
            + argument
          </button>
        </div>
        <div className="argv-list">
          {(profile.argv ?? []).map((argument, argumentIndex) => (
            <div className="argv-row" key={argumentIndex}>
              <span>{argumentIndex}</span>
              <input
                value={argument}
                maxLength={4096}
                spellCheck={false}
                aria-label={`Argument ${argumentIndex}`}
                placeholder={argumentIndex === 0 ? "executable" : "argument"}
                onChange={(event) => updateArgument(argumentIndex, event.target.value)}
              />
              <button
                type="button"
                disabled={(profile.argv?.length ?? 0) === 1}
                aria-label={`Remove argument ${argumentIndex}`}
                onClick={() => patch("argv", (profile.argv ?? []).filter((_, current) => current !== argumentIndex))}
              >
                ×
              </button>
            </div>
          ))}
        </div>
        {errors.argv && <p className="field-error" role="alert">{errors.argv}</p>}
      </div>
      )}
        </>
      )}
    </article>
  );
}

function Field({
  label,
  error,
  wide,
  children,
}: {
  label: string;
  error?: string;
  wide?: boolean;
  children: ReactNode;
}) {
  return (
    <label className={`profile-field${wide ? " profile-field--wide" : ""}`}>
      <span>{label}</span>
      {children}
      {error && <small className="field-error">{error}</small>}
    </label>
  );
}

function catalogInitial(id: AgentCatalogEntry["id"]): string {
  switch (id) {
    case "claude-code": return "C";
    case "codex-cli": return "⌁";
    case "mimo-code": return "M";
    case "ollama": return "O";
    default: return "+";
  }
}

function readOnlyReasonLabel(reason: AgentProfile["readOnlyReason"]): string {
  switch (reason) {
    case "advanced_shell":
      return "This profile uses a shell command that can only be edited in the YAML.";
    case "advanced_environment":
      return "This profile has environment variables that are never exposed here.";
    case "advanced_adapter":
      return "This profile uses an advanced adapter that this form cannot rewrite.";
    case "sensitive_arguments":
      return "The arguments are hidden because they look like sensitive data.";
    case "invalid_command":
      return "The existing command cannot be represented without loss in this form.";
    case "legacy_profile_fields":
      return "This profile uses a legacy identifier or legacy fields kept as they are in the YAML.";
    default:
      return "This profile uses advanced fields that stay protected against a partial rewrite.";
  }
}
