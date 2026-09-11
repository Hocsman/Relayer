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
  FullSettingsView,
  LifecycleResult,
  NotificationSettings,
  NotificationWebhookSetting,
  RelayerBridge,
  RunStatus,
  SaveAgentProfilesAndRestartRequest,
  SaveAgentProfilesRequest,
  SecuritySettings,
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
type SettingsTab = "agents" | "security" | "notifications";

export function AgentSettingsPanel({
  bridge,
  runID,
  runStatus,
  pendingEvents,
  onSave,
  onSaveAndRestart,
  onClose,
}: AgentSettingsPanelProps) {
  const [activeTab, setActiveTab] = useState<SettingsTab>("agents");
  const [view, setView] = useState<AgentProfilesView>();
  const [fullView, setFullView] = useState<FullSettingsView>();
  const [draft, setDraft] = useState<AgentProfile[]>([]);
  const [securityDraft, setSecurityDraft] = useState<SecuritySettings>({
    profile: "developer-friendly",
    defaultAction: "ask",
    dryRun: false,
    blockDestructive: true,
    blockExfiltration: true,
    blockSensitivePaths: true,
    blockOutsideWorkspace: false,
    workspaceRoot: "",
    rateLimitPerMinute: 30,
    maxConsecutiveAutoDecisions: 10,
  });
  const [notificationDraft, setNotificationDraft] = useState<NotificationSettings>({
    enabled: true,
    bell: true,
    desktop: true,
    minSeverity: "info",
    webhooks: [],
  });
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [activating, setActivating] = useState(false);
  const [closeConfirmation, setCloseConfirmation] = useState(false);
  const [restartConfirmation, setRestartConfirmation] = useState(false);
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<Notice>();

  useEffect(() => {
    let active = true;
    const loader = typeof bridge.getFullSettings === "function"
      ? bridge.getFullSettings()
      : bridge.getAgentProfiles().then((loaded) => ({
          ...loaded,
          security: securityDraft,
          notifications: notificationDraft,
        }));

    void loader.then(
      (loaded) => {
        if (!active) return;
        setView(loaded);
        if ("security" in loaded && (loaded as FullSettingsView).security) {
          const fullLoaded = loaded as FullSettingsView;
          setFullView(fullLoaded);
          setSecurityDraft(fullLoaded.security);
          setNotificationDraft(fullLoaded.notifications);
        }
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
        setError("The configuration could not be loaded.");
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
  const agentsDirty = view
    ? JSON.stringify(draft) !== JSON.stringify(view.profiles)
    : false;
  const securityDirty = fullView
    ? JSON.stringify(securityDraft) !== JSON.stringify(fullView.security)
    : false;
  const notificationsDirty = fullView
    ? JSON.stringify(notificationDraft) !== JSON.stringify(fullView.notifications)
    : false;
  const dirty = agentsDirty || securityDirty || notificationsDirty;
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

  const dialogRef = useRef<HTMLDivElement>(null);

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
      if (typeof bridge.saveFullSettings === "function") {
        const result = await bridge.saveFullSettings(runID, {
          expectedRevision: view.revision,
          profiles: agentsDirty ? profilesForSave(draft) : undefined,
          security: securityDirty ? securityDraft : undefined,
          notifications: notificationsDirty ? notificationDraft : undefined,
        });
        setFullView(result);
        setView(result);
        setDraft(cloneProfiles(result.profiles));
        setSecurityDraft(result.security);
        setNotificationDraft(result.notifications);
        setNotice(result.restartRequired
          ? {
              tone: "warning",
              text: "Configuration saved — restart required to apply agent changes.",
            }
          : {
              tone: "success",
              text: "Configuration saved and applied immediately.",
            });
        setCloseConfirmation(false);
      } else {
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
      }
    } catch {
      try {
        const reloaded = typeof bridge.getFullSettings === "function"
          ? await bridge.getFullSettings()
          : await bridge.getAgentProfiles();
        setView(reloaded);
        if ("security" in reloaded && (reloaded as FullSettingsView).security) {
          const fullReloaded = reloaded as FullSettingsView;
          setFullView(fullReloaded);
          setSecurityDraft(fullReloaded.security);
          setNotificationDraft(fullReloaded.notifications);
        }
        setDraft(cloneProfiles(reloaded.profiles));
        setError("The configuration changed or its state was uncertain. The saved version has been reloaded.");
      } catch {
        setError("The configuration could not be saved. No sensitive detail is shown.");
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
      if (typeof bridge.saveFullSettings === "function" && (securityDirty || notificationsDirty)) {
        await bridge.saveFullSettings(runID, {
          expectedRevision: view.revision,
          profiles: profilesForSave(draft),
          security: securityDraft,
          notifications: notificationDraft,
        });
      }
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
    <div className="agent-settings-layer" ref={dialogRef}>
      <section
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
            <nav className="settings-tabs" aria-label="Settings sections">
              <button
                type="button"
                className={`settings-tab ${activeTab === "agents" ? "settings-tab--active" : ""}`}
                onClick={() => setActiveTab("agents")}
              >
                🤖 Agents
              </button>
              <button
                type="button"
                className={`settings-tab ${activeTab === "security" ? "settings-tab--active" : ""}`}
                onClick={() => setActiveTab("security")}
              >
                🛡️ Security & Guardrails
              </button>
              <button
                type="button"
                className={`settings-tab ${activeTab === "notifications" ? "settings-tab--active" : ""}`}
                onClick={() => setActiveTab("notifications")}
              >
                🔔 Notifications & Webhooks
              </button>
            </nav>

            {activeTab === "agents" && (
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
            )}

            {activeTab === "security" && (
              <SecuritySettingsTab
                settings={securityDraft}
                onChange={(next) => {
                  setSecurityDraft(next);
                  setNotice(undefined);
                  setError(undefined);
                }}
                disabled={busy}
              />
            )}

            {activeTab === "notifications" && (
              <NotificationSettingsTab
                settings={notificationDraft}
                onChange={(next) => {
                  setNotificationDraft(next);
                  setNotice(undefined);
                  setError(undefined);
                }}
                disabled={busy}
              />
            )}

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

function SecuritySettingsTab({
  settings,
  onChange,
  disabled,
}: {
  settings: SecuritySettings;
  onChange(settings: SecuritySettings): void;
  disabled: boolean;
}) {
  const patch = <K extends keyof SecuritySettings>(field: K, value: SecuritySettings[K]) =>
    onChange({ ...settings, [field]: value });

  const onProfilePresetChange = (profile: string) => {
    if (profile === "strict") {
      onChange({
        ...settings,
        profile: "strict",
        defaultAction: "ask",
        dryRun: false,
        blockDestructive: true,
        blockExfiltration: true,
        blockSensitivePaths: true,
        blockOutsideWorkspace: true,
        rateLimitPerMinute: 20,
        maxConsecutiveAutoDecisions: 5,
      });
    } else if (profile === "developer-friendly") {
      onChange({
        ...settings,
        profile: "developer-friendly",
        defaultAction: "ask",
        dryRun: false,
        blockDestructive: true,
        blockExfiltration: true,
        blockSensitivePaths: true,
        blockOutsideWorkspace: false,
        rateLimitPerMinute: 30,
        maxConsecutiveAutoDecisions: 10,
      });
    } else if (profile === "permissive") {
      onChange({
        ...settings,
        profile: "permissive",
        defaultAction: "allow",
        dryRun: true,
        blockDestructive: false,
        blockExfiltration: false,
        blockSensitivePaths: false,
        blockOutsideWorkspace: false,
        rateLimitPerMinute: 60,
        maxConsecutiveAutoDecisions: 30,
      });
    } else {
      patch("profile", profile);
    }
  };

  return (
    <div className="settings-section" aria-label="Security settings">
      <div className="settings-group">
        <h3>Policy Profile & Arbitration</h3>
        <p>Choose an established safety posture or calibrate individual security parameters.</p>
        <div className="settings-row">
          <span>Security Profile</span>
          <select
            className="settings-select"
            value={settings.profile}
            onChange={(e) => onProfilePresetChange(e.target.value)}
            disabled={disabled}
            aria-label="Security profile"
          >
            <option value="developer-friendly">developer-friendly (Balanced)</option>
            <option value="strict">strict (High Security)</option>
            <option value="permissive">permissive (Audit / Dry-Run)</option>
            <option value="custom">custom</option>
          </select>
        </div>
        <div className="settings-row">
          <span>Default Action</span>
          <select
            className="settings-select"
            value={settings.defaultAction}
            onChange={(e) => patch("defaultAction", e.target.value)}
            disabled={disabled}
            aria-label="Default action"
          >
            <option value="ask">ask (Require human confirmation)</option>
            <option value="allow">allow (Execute if no rule denies)</option>
            <option value="deny">deny (Block unless explicitly permitted)</option>
          </select>
        </div>
        <div className="settings-row">
          <label className="settings-toggle">
            <input
              type="checkbox"
              checked={settings.dryRun}
              onChange={(e) => patch("dryRun", e.target.checked)}
              disabled={disabled}
            />
            <span>Dry-Run Mode (Evaluate policies without blocking commands)</span>
          </label>
        </div>
      </div>

      <div className="settings-group">
        <h3>Guardrails & Path Protection</h3>
        <p>Automated interceptors that prevent accidental system damage or data leakage.</p>
        <div className="settings-row">
          <label className="settings-toggle">
            <input
              type="checkbox"
              checked={settings.blockDestructive}
              onChange={(e) => patch("blockDestructive", e.target.checked)}
              disabled={disabled}
            />
            <span>Block Destructive Commands (e.g. rm -rf, drop database, format)</span>
          </label>
        </div>
        <div className="settings-row">
          <label className="settings-toggle">
            <input
              type="checkbox"
              checked={settings.blockExfiltration}
              onChange={(e) => patch("blockExfiltration", e.target.checked)}
              disabled={disabled}
            />
            <span>Block Data Exfiltration (curl, wget, reverse shells, suspicious egress)</span>
          </label>
        </div>
        <div className="settings-row">
          <label className="settings-toggle">
            <input
              type="checkbox"
              checked={settings.blockSensitivePaths}
              onChange={(e) => patch("blockSensitivePaths", e.target.checked)}
              disabled={disabled}
            />
            <span>Block Sensitive Paths (.env, .git/config, id_rsa, credentials)</span>
          </label>
        </div>
        <div className="settings-row">
          <label className="settings-toggle">
            <input
              type="checkbox"
              checked={settings.blockOutsideWorkspace}
              onChange={(e) => patch("blockOutsideWorkspace", e.target.checked)}
              disabled={disabled}
            />
            <span>Block File Access Outside Workspace</span>
          </label>
        </div>
        <div className="settings-row">
          <span>Workspace Root Directory</span>
          <input
            className="settings-input"
            style={{ width: "280px" }}
            value={settings.workspaceRoot}
            placeholder="Empty = current directory"
            onChange={(e) => patch("workspaceRoot", e.target.value)}
            disabled={disabled}
            aria-label="Workspace root"
          />
        </div>
      </div>

      <div className="settings-group">
        <h3>Rate Limiting & Safety Bounds</h3>
        <p>Prevent runaway loops and excessive automated approvals.</p>
        <div className="settings-row">
          <span>Rate Limit (Decisions / Minute)</span>
          <input
            className="settings-input"
            type="number"
            min={1}
            max={600}
            style={{ width: "100px" }}
            value={settings.rateLimitPerMinute}
            onChange={(e) => patch("rateLimitPerMinute", parseInt(e.target.value, 10) || 30)}
            disabled={disabled}
            aria-label="Rate limit per minute"
          />
        </div>
        <div className="settings-row">
          <span>Max Consecutive Auto Decisions</span>
          <input
            className="settings-input"
            type="number"
            min={1}
            max={100}
            style={{ width: "100px" }}
            value={settings.maxConsecutiveAutoDecisions}
            onChange={(e) => patch("maxConsecutiveAutoDecisions", parseInt(e.target.value, 10) || 10)}
            disabled={disabled}
            aria-label="Max consecutive auto decisions"
          />
        </div>
      </div>
    </div>
  );
}

function NotificationSettingsTab({
  settings,
  onChange,
  disabled,
}: {
  settings: NotificationSettings;
  onChange(settings: NotificationSettings): void;
  disabled: boolean;
}) {
  const [webhookName, setWebhookName] = useState("");
  const [webhookUrl, setWebhookUrl] = useState("");
  const [webhookFormat, setWebhookFormat] = useState("slack");
  const [webhookSeverity, setWebhookSeverity] = useState("warning");

  const patch = <K extends keyof NotificationSettings>(field: K, value: NotificationSettings[K]) =>
    onChange({ ...settings, [field]: value });

  const addWebhook = () => {
    if (!webhookName.trim() || !webhookUrl.trim()) return;
    const newHook: NotificationWebhookSetting = {
      name: webhookName.trim(),
      url: webhookUrl.trim(),
      format: webhookFormat,
      minSeverity: webhookSeverity,
      timeout: "5s",
    };
    patch("webhooks", [...(settings.webhooks || []), newHook]);
    setWebhookName("");
    setWebhookUrl("");
  };

  const removeWebhook = (index: number) => {
    patch("webhooks", (settings.webhooks || []).filter((_, i) => i !== index));
  };

  return (
    <div className="settings-section" aria-label="Notification settings">
      <div className="settings-group">
        <h3>Dispatch Channels</h3>
        <p>Choose where and how arbitration alerts and guardrail intercepts are sent.</p>
        <div className="settings-row">
          <label className="settings-toggle">
            <input
              type="checkbox"
              checked={settings.enabled}
              onChange={(e) => patch("enabled", e.target.checked)}
              disabled={disabled}
            />
            <span>Enable Notifications Master Switch</span>
          </label>
        </div>
        <div className="settings-row">
          <label className="settings-toggle">
            <input
              type="checkbox"
              checked={settings.desktop}
              onChange={(e) => patch("desktop", e.target.checked)}
              disabled={disabled}
            />
            <span>Desktop Native Notifications (Windows Toast, macOS Notification Center, libnotify)</span>
          </label>
        </div>
        <div className="settings-row">
          <label className="settings-toggle">
            <input
              type="checkbox"
              checked={settings.bell}
              onChange={(e) => patch("bell", e.target.checked)}
              disabled={disabled}
            />
            <span>Terminal Acoustic Bell (\a on pending events)</span>
          </label>
        </div>
        <div className="settings-row">
          <span>Global Minimum Severity</span>
          <select
            className="settings-select"
            value={settings.minSeverity}
            onChange={(e) => patch("minSeverity", e.target.value)}
            disabled={disabled}
            aria-label="Minimum severity"
          >
            <option value="info">info (All events)</option>
            <option value="warning">warning (Arbitrations & Warnings)</option>
            <option value="critical">critical (Security intercepts & Guardrail blocks)</option>
          </select>
        </div>
      </div>

      <div className="settings-group">
        <h3>Webhooks</h3>
        <p>Broadcast security events to team channels (Slack, Discord, or generic JSON endpoints).</p>

        {settings.webhooks && settings.webhooks.length > 0 ? (
          <table className="settings-webhooks-table" aria-label="Configured webhooks">
            <thead>
              <tr>
                <th>Name</th>
                <th>URL</th>
                <th>Format</th>
                <th>Min Severity</th>
                <th>Action</th>
              </tr>
            </thead>
            <tbody>
              {settings.webhooks.map((hook, index) => (
                <tr key={`${index}-${hook.name}`}>
                  <td><strong>{hook.name}</strong></td>
                  <td style={{ maxWidth: "240px", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }} title={hook.url}>
                    {hook.url}
                  </td>
                  <td><span className="catalog-badge">{hook.format}</span></td>
                  <td>{hook.minSeverity}</td>
                  <td>
                    <button
                      type="button"
                      className="button button--ghost button--small"
                      onClick={() => removeWebhook(index)}
                      disabled={disabled}
                      aria-label={`Remove webhook ${hook.name}`}
                    >
                      Remove
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : (
          <p style={{ margin: "4px 0 10px", color: "var(--faint)", fontSize: "11px" }}>
            No webhooks configured.
          </p>
        )}

        <div style={{ display: "flex", gap: "8px", alignItems: "center", marginTop: "8px", flexWrap: "wrap" }}>
          <input
            className="settings-input"
            style={{ width: "130px" }}
            placeholder="Name (e.g. #security)"
            value={webhookName}
            onChange={(e) => setWebhookName(e.target.value)}
            disabled={disabled}
            aria-label="New webhook name"
          />
          <input
            className="settings-input"
            style={{ flex: "1 1 200px" }}
            placeholder="https://hooks.slack.com/... or Discord URL"
            value={webhookUrl}
            onChange={(e) => setWebhookUrl(e.target.value)}
            disabled={disabled}
            aria-label="New webhook URL"
          />
          <select
            className="settings-select"
            value={webhookFormat}
            onChange={(e) => setWebhookFormat(e.target.value)}
            disabled={disabled}
            aria-label="New webhook format"
          >
            <option value="slack">Slack</option>
            <option value="discord">Discord</option>
            <option value="generic">Generic JSON</option>
          </select>
          <select
            className="settings-select"
            value={webhookSeverity}
            onChange={(e) => setWebhookSeverity(e.target.value)}
            disabled={disabled}
            aria-label="New webhook severity"
          >
            <option value="info">info</option>
            <option value="warning">warning</option>
            <option value="critical">critical</option>
          </select>
          <button
            type="button"
            className="button button--ghost button--small"
            onClick={addWebhook}
            disabled={disabled || !webhookName.trim() || !webhookUrl.trim()}
          >
            + Add Webhook
          </button>
        </div>
      </div>
    </div>
  );
}

