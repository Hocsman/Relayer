import { useEffect, useMemo, useState, type ReactNode } from "react";
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
  RelayerBridge,
} from "../types/relayer";

interface AgentSettingsPanelProps {
  bridge: RelayerBridge;
  onClose(): void;
}

type Notice = { tone: "success" | "warning"; text: string };

export function AgentSettingsPanel({ bridge, onClose }: AgentSettingsPanelProps) {
  const [view, setView] = useState<AgentProfilesView>();
  const [draft, setDraft] = useState<AgentProfile[]>([]);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [closeConfirmation, setCloseConfirmation] = useState(false);
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
            text: "Configuration enregistrée — elle sera appliquée au prochain démarrage.",
          });
        }
        setLoading(false);
      },
      () => {
        if (!active) return;
        setError("Impossible de charger la configuration des agents.");
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
        name: entry.id === "custom" ? "Nouvel agent" : entry.name,
        presetID: entry.id,
        cwd: "",
        backend: "auto",
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

  const requestClose = () => {
    if (dirty && !closeConfirmation) {
      setCloseConfirmation(true);
      return;
    }
    onClose();
  };

  const save = async () => {
    if (!view || !dirty || !validation.valid || saving) return;
    setSaving(true);
    setError(undefined);
    setNotice(undefined);
    try {
      const result = await bridge.saveAgentProfiles({
        expectedRevision: view.revision,
        profiles: profilesForSave(draft),
      });
      setView(result);
      setDraft(cloneProfiles(result.profiles));
      setNotice(result.restartRequired
        ? {
            tone: "warning",
            text: "Configuration enregistrée — elle sera appliquée au prochain démarrage.",
          }
        : {
            tone: "success",
            text: "Configuration enregistrée.",
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
        setError("La configuration a changé ou son état était incertain. La version enregistrée a été rechargée.");
      } catch {
        setError("Impossible d’enregistrer les agents. Aucun détail sensible n’est affiché.");
      }
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="agent-settings-layer">
      <section className="agent-settings" role="dialog" aria-modal="true" aria-labelledby="agents-title">
        <header className="agent-settings__header">
          <div>
            <span className="eyebrow">Configuration locale</span>
            <h1 id="agents-title">Agents</h1>
            <p>Composez une équipe de 1 à 8 CLI avec des arguments exacts.</p>
          </div>
          <button className="icon-button" type="button" onClick={requestClose} aria-label="Fermer les agents" autoFocus>
            ×
          </button>
        </header>

        {loading ? (
          <div className="agent-settings__loading">
            <span className="settings-spinner" aria-hidden="true" />
            Chargement du catalogue…
          </div>
        ) : !view ? (
          <div className="agent-settings__failure" role="alert">
            <strong>Catalogue indisponible</strong>
            <p>{error || "Le bridge natif n’a retourné aucune configuration."}</p>
            <button className="button button--ghost" type="button" onClick={onClose}>Retour</button>
          </div>
        ) : (
          <>
            <div className="agent-settings__content">
              <Catalog
                entries={view.catalog}
                count={draft.length}
                maximum={Math.min(8, view.maxProfiles)}
                editable={view.editable}
                onAdd={addProfile}
              />
              <section className="profile-editor" aria-label="Profils configurés">
                <header className="profile-editor__header">
                  <div>
                    <span className="eyebrow">Équipe configurée</span>
                    <h2>{draft.length} agent{draft.length > 1 ? "s" : ""}</h2>
                  </div>
                  <span className="profile-limit">{draft.length} / {Math.min(8, view.maxProfiles)}</span>
                </header>

                {validation.global.map((message) => (
                  <p className="settings-error" role="alert" key={message}>{message}</p>
                ))}

                {!view.editable && (
                  <p className="settings-error" role="alert">
                    Cette configuration historique est en lecture seule. Migrez-la vers <code>version: 1</code> avant de modifier les agents.
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
            </div>

            <footer className="agent-settings__footer">
              <div className="settings-footer__status">
                <span className="settings-path" title={view.configPath}>{view.configPath}</span>
                <span>{dirty ? "Modifications non enregistrées" : "Configuration synchronisée"}</span>
                {error && <strong className="settings-save-error" role="alert">{error}</strong>}
                {notice && <strong className={`settings-notice settings-notice--${notice.tone}`}>{notice.text}</strong>}
                {view.restartRequired && (
                  <span className="restart-guidance">
                    Arrêtez les sessions depuis le dashboard, fermez Relayer, puis rouvrez l’application.
                  </span>
                )}
              </div>
              <div className="settings-footer__actions">
                {closeConfirmation && (
                  <span className="inline-confirm">
                    Abandonner les modifications ?
                    <button className="button button--ghost" type="button" onClick={onClose}>Abandonner</button>
                  </span>
                )}
                <button
                  className="button button--primary"
                  type="button"
                  disabled={!view.editable || !dirty || !validation.valid || saving}
                  onClick={() => {
                    setCloseConfirmation(false);
                    void save();
                  }}
                >
                  {saving ? "Enregistrement…" : "Enregistrer"}
                </button>
              </div>
            </footer>
          </>
        )}
      </section>
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
    <aside className="agent-catalog" aria-label="Catalogue d’agents">
      <header>
        <span className="eyebrow">Catalogue</span>
        <h2>Ajouter un CLI</h2>
        <p>Les badges d’installation sont détectés par le moteur local.</p>
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
                  {entry.id === "custom" ? "Commande libre" : entry.installed ? "Installé" : "Non détecté"}
                </span>
                <span className="catalog-badge">Generic · stable</span>
              </div>
            </div>
            <button
              className="catalog-add"
              type="button"
              disabled={!editable || count >= maximum}
              onClick={() => onAdd(entry)}
              aria-label={`Ajouter ${entry.name}`}
            >
              +
            </button>
          </article>
        ))}
      </div>
      <p className="catalog-security">
        Aucune clé fournisseur, variable d’environnement ou sélection de modèle n’est enregistrée ici.
        DeepSeek et les autres modèles se configurent dans le CLI compatible choisi, jamais comme secrets Relayer.
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
          <h3>{profile.name || "Agent sans nom"}</h3>
          <span>{preset?.name || "Sélection inconnue"} · generic</span>
        </div>
        <div className="profile-card__controls">
          <button type="button" onClick={() => onMove(-1)} disabled={profile.locked || index === 0} aria-label="Monter cet agent">↑</button>
          <button type="button" onClick={() => onMove(1)} disabled={profile.locked || index === count - 1} aria-label="Descendre cet agent">↓</button>
          <button className="profile-remove" type="button" onClick={onRemove} disabled={profile.locked || count <= minimum} aria-label="Supprimer cet agent">×</button>
        </div>
      </header>

      {profile.locked ? (
        <div className="profile-lock" role="note">
          <span aria-hidden="true">⌁</span>
          <div>
            <strong>Profil avancé en lecture seule</strong>
            <p>{readOnlyReasonLabel(profile.readOnlyReason)}</p>
            <small>{profile.id} · {profile.backend}</small>
          </div>
        </div>
      ) : (
        <>

      <div className="profile-fields">
        <Field label="Nom" error={errors.name}>
          <input value={profile.name} maxLength={80} onChange={(event) => patch("name", event.target.value)} />
        </Field>
        <Field label="Identifiant" error={errors.id}>
          <input
            value={profile.id}
            maxLength={64}
            spellCheck={false}
            disabled={profile.preserveOnSave}
            title={profile.preserveOnSave ? "Remplacez d’abord la commande pour changer l’identifiant." : undefined}
            onChange={(event) => patch("id", event.target.value)}
          />
        </Field>
        <Field label="Catalogue" error={errors.presetID}>
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
        <Field label="Dossier de travail" error={errors.cwd} wide>
          <input
            value={profile.cwd}
            maxLength={4096}
            spellCheck={false}
            placeholder="Vide = dossier par défaut"
            onChange={(event) => patch("cwd", event.target.value)}
          />
        </Field>
      </div>

      {profile.preserveOnSave ? (
        <div className="argv-editor argv-editor--masked" role="note">
          <div>
            <strong>Commande existante masquée</strong>
            <span>
              {profile.executableLabel || "Commande configurée"} · {profile.argumentCount ?? 0} argument{(profile.argumentCount ?? 0) > 1 ? "s" : ""}
            </span>
            <small>Les valeurs argv existantes ne sont jamais envoyées au WebView.</small>
          </div>
          <button
            type="button"
            onClick={() => onChange({
              ...profile,
              preserveOnSave: false,
              argv: [...(preset?.defaultArgv.length ? preset.defaultArgv : [""])],
            })}
          >
            Remplacer la commande
          </button>
        </div>
      ) : (
      <div className="argv-editor">
        <div className="argv-editor__title">
          <div>
            <strong>Arguments exacts</strong>
            <span>Aucun shell, expansion ou interpolation.</span>
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
                placeholder={argumentIndex === 0 ? "exécutable" : "argument"}
                onChange={(event) => updateArgument(argumentIndex, event.target.value)}
              />
              <button
                type="button"
                disabled={(profile.argv?.length ?? 0) === 1}
                aria-label={`Supprimer l’argument ${argumentIndex}`}
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
    default: return "+";
  }
}

function readOnlyReasonLabel(reason: AgentProfile["readOnlyReason"]): string {
  switch (reason) {
    case "advanced_shell":
      return "Ce profil utilise une commande shell qui reste modifiable uniquement dans le YAML.";
    case "advanced_environment":
      return "Ce profil possède des variables d’environnement qui ne sont jamais exposées ici.";
    case "advanced_adapter":
      return "Ce profil utilise un adaptateur avancé que ce formulaire ne peut pas réécrire.";
    case "sensitive_arguments":
      return "Les arguments sont masqués car ils ressemblent à des données sensibles.";
    case "invalid_command":
      return "La commande existante ne peut pas être représentée sans perte dans ce formulaire.";
    case "legacy_profile_fields":
      return "Ce profil utilise un identifiant ou des champs historiques conservés tels quels dans le YAML.";
    default:
      return "Ce profil utilise des champs avancés qui restent protégés contre une réécriture partielle.";
  }
}
