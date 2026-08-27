import { useEffect, useLayoutEffect, useRef, useState } from "react";

interface TerminalSnapshotViewProps {
  runID: string;
  sessionID: string;
  label: string;
  output: string;
  revision: number;
  onResize(runID: string, sessionID: string, columns: number, rows: number): Promise<void>;
}

const FOLLOW_THRESHOLD = 12;

// This is intentionally a bounded text-snapshot viewer, not a VT emulator.
// Keeping it isolated makes a later migration to a true terminal stream local.
export function TerminalSnapshotView({
  runID,
  sessionID,
  label,
  output,
  revision,
  onResize,
}: TerminalSnapshotViewProps) {
  const viewportRef = useRef<HTMLDivElement>(null);
  const followRef = useRef(true);
  const resizeRef = useRef(onResize);
  const lastSizeRef = useRef({ columns: 0, rows: 0 });
  const [following, setFollowing] = useState(true);

  resizeRef.current = onResize;

  useLayoutEffect(() => {
    const viewport = viewportRef.current;
    if (viewport && followRef.current) {
      viewport.scrollTop = viewport.scrollHeight;
    }
  }, [output, revision]);

  useEffect(() => {
    const viewport = viewportRef.current;
    if (!viewport) return;

    let timeout = 0;
    const reportSize = () => {
      window.clearTimeout(timeout);
      timeout = window.setTimeout(() => {
        const styles = window.getComputedStyle(viewport);
        const fontSize = Number.parseFloat(styles.fontSize) || 13;
        const lineHeight = Number.parseFloat(styles.lineHeight) || fontSize * 1.55;
        const horizontalPadding =
          (Number.parseFloat(styles.paddingLeft) || 0) +
          (Number.parseFloat(styles.paddingRight) || 0);
        const verticalPadding =
          (Number.parseFloat(styles.paddingTop) || 0) +
          (Number.parseFloat(styles.paddingBottom) || 0);

        const canvas = document.createElement("canvas");
        const context = canvas.getContext("2d");
        if (context) context.font = styles.font;
        const characterWidth = Math.max(1, context?.measureText("M").width || fontSize * 0.62);
        const columns = Math.max(1, Math.floor((viewport.clientWidth - horizontalPadding) / characterWidth));
        const rows = Math.max(1, Math.floor((viewport.clientHeight - verticalPadding) / lineHeight));

        if (
          columns === lastSizeRef.current.columns &&
          rows === lastSizeRef.current.rows
        ) {
          return;
        }
        lastSizeRef.current = { columns, rows };
        void resizeRef.current(runID, sessionID, columns, rows);
      }, 120);
    };

    const observer = new ResizeObserver(reportSize);
    observer.observe(viewport);
    reportSize();
    return () => {
      observer.disconnect();
      window.clearTimeout(timeout);
    };
  }, [runID, sessionID]);

  const handleScroll = () => {
    const viewport = viewportRef.current;
    if (!viewport) return;
    const atBottom =
      viewport.scrollHeight - viewport.scrollTop - viewport.clientHeight <= FOLLOW_THRESHOLD;
    followRef.current = atBottom;
    setFollowing(atBottom);
  };

  const resumeFollowing = () => {
    const viewport = viewportRef.current;
    if (!viewport) return;
    followRef.current = true;
    setFollowing(true);
    viewport.scrollTo({ top: viewport.scrollHeight, behavior: "smooth" });
  };

  return (
    <div className="terminal-shell">
      <div
        ref={viewportRef}
        className="terminal-snapshot"
        role="log"
        aria-label={label}
        aria-live="off"
        onScroll={handleScroll}
        data-revision={revision}
      >
        {output ? <pre>{output}</pre> : <p className="terminal-snapshot__empty">En attente de sortie…</p>}
      </div>
      {!following && (
        <button className="follow-button" type="button" onClick={resumeFollowing}>
          <span aria-hidden="true">↓</span> Reprendre le direct
        </button>
      )}
    </div>
  );
}
