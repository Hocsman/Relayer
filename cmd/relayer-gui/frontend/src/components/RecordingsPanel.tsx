import { useCallback, useEffect, useRef, useState } from "react";
import { useDialogKeyboard } from "../hooks/useDialogKeyboard";
import { CastPlayer } from "./CastPlayer";
import type {
  RecordingFrameView,
  RecordingHeaderView,
  RecordingView,
  RelayerBridge,
} from "../types/relayer";

interface RecordingsPanelProps {
  bridge: RelayerBridge;
  readOnly?: boolean;
  onClose(): void;
}

// A single request never pages an entire session into memory, and the cap
// bounds how many follow-up requests the panel will chain before it stops.
const CHUNK_LIMIT = 500;
const MAX_CHUNKS = 24;

export function RecordingsPanel({ bridge, readOnly, onClose }: RecordingsPanelProps) {
  const [recordings, setRecordings] = useState<RecordingView[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState(false);
  const [selectedID, setSelectedID] = useState<string | null>(null);
  const [header, setHeader] = useState<RecordingHeaderView>();
  const [frames, setFrames] = useState<RecordingFrameView[]>([]);
  const [partial, setPartial] = useState(false);
  const [playerLoading, setPlayerLoading] = useState(false);
  const [playerError, setPlayerError] = useState(false);
  const [downloading, setDownloading] = useState(false);
  const [confirmingDeleteID, setConfirmingDeleteID] = useState<string | null>(null);
  const [deleting, setDeleting] = useState(false);
  const [actionError, setActionError] = useState(false);

  const dialogRef = useRef<HTMLElement>(null);
  useDialogKeyboard(dialogRef, { onClose });

  // Every load of a recording carries the token it started with. Closing the
  // panel retires them all, so nothing writes into state that no longer exists.
  const loadTokenRef = useRef(0);
  useEffect(
    () => () => {
      loadTokenRef.current += 1;
    },
    [],
  );

  // A build without a recording store has no list call at all. That is a
  // supported configuration, not a failure, so it gets its own empty state.
  const supported = Boolean(bridge.listRecordings);
  const deletable = Boolean(bridge.deleteRecording) && !readOnly;

  const loadData = useCallback(async () => {
    if (!bridge.listRecordings) {
      setLoading(false);
      return;
    }
    setLoading(true);
    setError(false);
    try {
      setRecordings(await bridge.listRecordings({}));
    } catch {
      setError(true);
    } finally {
      setLoading(false);
    }
  }, [bridge]);

  useEffect(() => {
    void loadData();
  }, [loadData]);

  // A session that starts or finishes while the panel is open changes what can
  // be replayed, so the list follows the store rather than the moment it opened.
  useEffect(
    () =>
      bridge.on("relayer:recording", () => {
        void loadData();
      }),
    [bridge, loadData],
  );

  // Paging one recording is up to MAX_CHUNKS round trips, and the metadata for
  // the row is picked up synchronously while the frames arrive later. Two
  // clicks in that window would leave the slower load to finish last and win:
  // the panel would then show one recording's header and frames under another
  // one's name, duration and flags. In an audit view that is not a glitch, it
  // is a false record. So each load carries a token, and a load that is no
  // longer the current one stops paging and writes nothing.
  const selectRecording = useCallback(
    async (id: string) => {
      const token = loadTokenRef.current + 1;
      loadTokenRef.current = token;

      setSelectedID(id);
      setHeader(undefined);
      setFrames([]);
      setPartial(false);
      setConfirmingDeleteID(null);
      if (!bridge.readRecordingChunk) return;
      setPlayerLoading(true);
      setPlayerError(false);
      try {
        const collected: RecordingFrameView[] = [];
        let nextHeader: RecordingHeaderView | undefined;
        let offset = 0;
        let complete = false;
        for (let chunkIndex = 0; chunkIndex < MAX_CHUNKS; chunkIndex += 1) {
          const chunk = await bridge.readRecordingChunk(id, offset, CHUNK_LIMIT);
          if (loadTokenRef.current !== token) return;
          nextHeader = chunk.header;
          collected.push(...chunk.frames);
          if (chunk.complete || chunk.nextOffset <= offset) {
            complete = true;
            break;
          }
          offset = chunk.nextOffset;
        }
        setHeader(nextHeader);
        setFrames(collected);
        // Running out of chunks is not the end of the session. Saying so is the
        // difference between a short replay and a session that looks like it
        // stopped where the panel stopped reading.
        setPartial(!complete);
      } catch {
        if (loadTokenRef.current !== token) return;
        setPlayerError(true);
      } finally {
        if (loadTokenRef.current === token) setPlayerLoading(false);
      }
    },
    [bridge],
  );

  const selected = recordings.find((recording) => recording.id === selectedID);

  // A failed export or delete used to raise the list-level error flag, which is
  // only ever rendered when the list is empty — so the one case that matters,
  // a delete that failed against a populated list, showed nothing at all.
  const handleDownload = async (recording: RecordingView) => {
    if (!bridge.exportRecording) return;
    setDownloading(true);
    setActionError(false);
    try {
      const content = await bridge.exportRecording(recording.id);
      const blob = new Blob([content], { type: "application/x-asciicast" });
      const url = URL.createObjectURL(blob);
      const anchor = document.createElement("a");
      anchor.href = url;
      const timestamp = recording.startedAt.replace(/[:.]/g, "-");
      anchor.download = `relayer-session-${recording.sessionID}-${timestamp}.cast`;
      document.body.appendChild(anchor);
      anchor.click();
      document.body.removeChild(anchor);
      URL.revokeObjectURL(url);
    } catch {
      setActionError(true);
    } finally {
      setDownloading(false);
    }
  };

  const handleDelete = async (id: string) => {
    if (!bridge.deleteRecording) return;
    setDeleting(true);
    setActionError(false);
    try {
      await bridge.deleteRecording(id);
      setConfirmingDeleteID(null);
      if (selectedID === id) {
        loadTokenRef.current += 1;
        setSelectedID(null);
        setHeader(undefined);
        setFrames([]);
        setPartial(false);
      }
      await loadData();
    } catch {
      setActionError(true);
    } finally {
      setDeleting(false);
    }
  };

  return (
    <div className="recordings-layer" role="presentation">
      <section
        ref={dialogRef}
        className="recordings-panel"
        role="dialog"
        aria-modal="true"
        aria-labelledby="recordings-title"
      >
        <header className="recordings-panel__header">
          <div>
            <span className="eyebrow">Session recording & replay</span>
            <h1 id="recordings-title">Recorded Sessions</h1>
            <p>
              Terminal sessions captured in the asciicast format · Replay for audit review and
              operator training.
            </p>
          </div>
          <button
            className="icon-button"
            type="button"
            onClick={onClose}
            aria-label="Close recordings panel"
            autoFocus
          >
            ×
          </button>
        </header>

        <div className="recordings-panel__body">
          <RecordingsContentView
            recordings={recordings}
            supported={supported}
            loading={loading}
            error={error}
            selectedID={selectedID}
            header={header}
            frames={frames}
            partial={partial}
            playerLoading={playerLoading}
            playerError={playerError}
            deletable={deletable}
            deleting={deleting}
            confirmingDeleteID={confirmingDeleteID}
            onSelect={(id) => void selectRecording(id)}
            onConfirmDelete={setConfirmingDeleteID}
            onDelete={(id) => void handleDelete(id)}
            onRefresh={() => void loadData()}
          />
        </div>

        <footer className="recordings-panel__footer">
          <p>
            Recordings hold terminal output only. Credentials intercepted by the policy engine are
            redacted before a frame is written to disk.
          </p>
          {actionError && (
            <p className="recordings-panel__action-error" role="alert">
              The local engine refused the last action. The recording was left unchanged.
            </p>
          )}
          <div className="recordings-panel__actions">
            <button
              className="button button--ghost"
              type="button"
              disabled={!selected || downloading || !bridge.exportRecording}
              onClick={() => {
                if (selected) void handleDownload(selected);
              }}
            >
              {downloading ? "Preparing…" : "Download .cast"}
            </button>
          </div>
        </footer>
      </section>
    </div>
  );
}

export interface RecordingsContentViewProps {
  recordings: RecordingView[];
  supported?: boolean;
  loading?: boolean;
  error?: boolean;
  selectedID: string | null;
  header?: RecordingHeaderView;
  frames: RecordingFrameView[];
  partial?: boolean;
  playerLoading?: boolean;
  playerError?: boolean;
  deletable?: boolean;
  deleting?: boolean;
  confirmingDeleteID: string | null;
  onSelect(id: string): void;
  onConfirmDelete(id: string | null): void;
  onDelete(id: string): void;
  onRefresh(): void;
}

export function RecordingsContentView({
  recordings,
  supported = true,
  loading = false,
  error = false,
  selectedID,
  header,
  frames,
  partial = false,
  playerLoading = false,
  playerError = false,
  deletable = false,
  deleting = false,
  confirmingDeleteID,
  onSelect,
  onConfirmDelete,
  onDelete,
  onRefresh,
}: RecordingsContentViewProps) {
  if (!supported) {
    return (
      <div className="recordings-table__empty">
        <p>
          This build has no recording store. Session replay is available when Relayer runs behind
          the web gateway.
        </p>
      </div>
    );
  }

  if (loading && recordings.length === 0) {
    return (
      <div className="recordings-panel__loading">
        <span className="settings-spinner" aria-hidden="true" />
        Loading recorded sessions…
      </div>
    );
  }

  if (error && recordings.length === 0) {
    return (
      <div className="recordings-panel__failure" role="alert">
        <span aria-hidden="true">!</span>
        <div>
          <strong>Recordings unavailable</strong>
          <p>The recorded sessions could not be retrieved from the local engine.</p>
        </div>
      </div>
    );
  }

  const selected = recordings.find((recording) => recording.id === selectedID);

  return (
    <>
      <div className="recordings-toolbar">
        <span className="recordings-toolbar__count">
          {recordings.length} recording{recordings.length !== 1 ? "s" : ""}
        </span>
        <button
          className="button button--ghost"
          type="button"
          onClick={onRefresh}
          disabled={loading}
          aria-label="Refresh recordings"
        >
          {loading ? "Refreshing…" : "Refresh"}
        </button>
      </div>

      <section className="recordings-table-section" aria-label="Recorded sessions list">
        {recordings.length === 0 ? (
          <div className="recordings-table__empty">
            <p>No session has been recorded yet.</p>
          </div>
        ) : (
          <div className="recordings-table-wrapper">
            <table className="recordings-table">
              <thead>
                <tr>
                  <th scope="col">Agent</th>
                  <th scope="col">Session</th>
                  <th scope="col">Started (UTC)</th>
                  <th scope="col">Duration</th>
                  <th scope="col">Size</th>
                  <th scope="col">Frames</th>
                  <th scope="col">Flags</th>
                  <th scope="col">Actions</th>
                </tr>
              </thead>
              <tbody>
                {recordings.map((recording) => (
                  <RecordingRow
                    key={recording.id}
                    recording={recording}
                    selected={recording.id === selectedID}
                    deletable={deletable}
                    deleting={deleting}
                    confirming={confirmingDeleteID === recording.id}
                    onSelect={() => onSelect(recording.id)}
                    onConfirmDelete={() =>
                      onConfirmDelete(confirmingDeleteID === recording.id ? null : recording.id)
                    }
                    onDelete={() => onDelete(recording.id)}
                  />
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>

      <section className="recordings-player" aria-label="Session replay">
        {!selected ? (
          <div className="recordings-player__empty">
            <p>Select a recording to replay it.</p>
          </div>
        ) : playerError ? (
          <div className="recordings-player__empty" role="alert">
            <p>This recording could not be read from the local engine.</p>
          </div>
        ) : header ? (
          <CastPlayer
            recording={selected}
            header={header}
            frames={frames}
            loading={playerLoading}
            partial={partial}
          />
        ) : (
          <div className="recordings-player__empty">
            <span className="settings-spinner" aria-hidden="true" />
            <p>Loading the recorded frames…</p>
          </div>
        )}
      </section>
    </>
  );
}

function RecordingRow({
  recording,
  selected,
  deletable,
  deleting,
  confirming,
  onSelect,
  onConfirmDelete,
  onDelete,
}: {
  recording: RecordingView;
  selected: boolean;
  deletable: boolean;
  deleting: boolean;
  confirming: boolean;
  onSelect(): void;
  onConfirmDelete(): void;
  onDelete(): void;
}) {
  const startedAt = recording.startedAt
    ? recording.startedAt.replace("T", " ").replace("Z", "")
    : "-";

  return (
    <tr className={`recordings-row ${selected ? "recordings-row--selected" : ""}`}>
      <td>
        <button
          type="button"
          className="recordings-row__select-button"
          onClick={onSelect}
          aria-pressed={selected}
        >
          {recording.name || recording.agentID || "-"}
        </button>
      </td>
      <td className="recordings-cell--mono">{recording.sessionID}</td>
      <td className="recordings-cell--mono">{startedAt}</td>
      <td className="recordings-cell--mono">{formatDuration(recording.durationSeconds)}</td>
      <td className="recordings-cell--mono">{formatBytes(recording.bytes)}</td>
      <td className="recordings-cell--mono">{recording.frames}</td>
      <td>
        <div className="recordings-badges">
          {recording.active && (
            <span className="recordings-badge recordings-badge--active">Recording</span>
          )}
          {recording.truncated && (
            <span className="recordings-badge recordings-badge--truncated">Truncated</span>
          )}
          {recording.droppedFrames > 0 && (
            <span className="recordings-badge recordings-badge--dropped">
              {recording.droppedFrames} dropped
            </span>
          )}
          {recording.inputRecorded && (
            <span className="recordings-badge recordings-badge--input">Input recorded</span>
          )}
          {recording.redacted && (
            <span className="recordings-badge recordings-badge--redacted">Redacted</span>
          )}
        </div>
      </td>
      <td>
        <div className="recordings-row__actions">
          {recording.active ? (
            <span className="recordings-row__note">Still being written</span>
          ) : deletable ? (
            confirming ? (
              <span className="inline-confirm">
                Delete this recording?
                <button
                  className="button button--danger button--tiny"
                  type="button"
                  disabled={deleting}
                  onClick={onDelete}
                >
                  {deleting ? "Deleting…" : "Delete"}
                </button>
                <button
                  className="button button--ghost button--tiny"
                  type="button"
                  disabled={deleting}
                  onClick={onConfirmDelete}
                >
                  Cancel
                </button>
              </span>
            ) : (
              <button
                className="button button--ghost button--tiny"
                type="button"
                onClick={onConfirmDelete}
              >
                Delete
              </button>
            )
          ) : null}
        </div>
      </td>
    </tr>
  );
}

function formatDuration(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return "0s";
  const total = Math.round(seconds);
  const hours = Math.floor(total / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  const rest = total % 60;
  if (hours > 0) return `${hours}h ${minutes}m ${rest}s`;
  if (minutes > 0) return `${minutes}m ${rest}s`;
  return `${rest}s`;
}

function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes <= 0) return "0 B";
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}
