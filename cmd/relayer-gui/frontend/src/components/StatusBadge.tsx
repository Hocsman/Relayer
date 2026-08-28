import type { SessionStatus } from "../types/relayer";

const labels: Record<SessionStatus, string> = {
  starting: "Starting",
  running: "Active",
  detached: "Detached",
  attached: "Attached",
  waiting: "Action required",
  stopping: "Stopping",
  exited: "Exited",
  failed: "Error",
};

export function StatusBadge({ status }: { status: SessionStatus }) {
  return (
    <span className={`status-badge status-badge--${status}`}>
      <span className="status-badge__dot" aria-hidden="true" />
      {labels[status] ?? status}
    </span>
  );
}
