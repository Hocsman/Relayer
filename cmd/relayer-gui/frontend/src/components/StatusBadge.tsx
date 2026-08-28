import type { SessionStatus } from "../types/relayer";

const labels: Record<SessionStatus, string> = {
  starting: "Démarrage",
  running: "Actif",
  detached: "Détaché",
  attached: "Attaché",
  waiting: "Action requise",
  stopping: "Arrêt en cours",
  exited: "Terminé",
  failed: "Erreur",
};

export function StatusBadge({ status }: { status: SessionStatus }) {
  return (
    <span className={`status-badge status-badge--${status}`}>
      <span className="status-badge__dot" aria-hidden="true" />
      {labels[status] ?? status}
    </span>
  );
}
