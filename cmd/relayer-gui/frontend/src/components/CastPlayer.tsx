import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import { SearchAddon } from "@xterm/addon-search";
import "@xterm/xterm/css/xterm.css";
import { RELAYER_TERMINAL_THEME } from "../lib/terminalTheme";
import { resizeFromFrame, totalDuration } from "../lib/castParser";
import type { CastFrame, CastFrameKind } from "../lib/castParser";
import {
  PLAYBACK_SPEEDS,
  advance,
  clampSpeed,
  formatTimecode,
  seekPlan,
} from "../lib/castPlayback";
import type { PlaybackState } from "../lib/castPlayback";
import type {
  RecordingFrameView,
  RecordingHeaderView,
  RecordingView,
} from "../types/relayer";

interface CastPlayerProps {
  recording: RecordingView;
  header: RecordingHeaderView;
  frames: RecordingFrameView[];
  loading: boolean;
  partial?: boolean;
}

// A seek replays the prefix with no delay, so the cost of the operation is the
// number of frames in it. Five thousand writes land well inside a frame budget
// on a slow machine; a long session has far more than that, and the operator is
// told the screen was rebuilt from the tail rather than left to watch the tab
// stop responding.
const SEEK_FRAME_BUDGET = 5000;

// The transport reads at a tenth of a second. Publishing the clock at that rate
// instead of once per animation frame keeps the timecode honest without
// re-rendering the controls sixty times a second behind a terminal that is
// already doing the expensive work.
const CLOCK_PUBLISH_STEP = 10;

// The longest stretch of wall time one tick may charge to the timeline.
//
// requestAnimationFrame does not fire while the tab is hidden, so a replay left
// playing in a background tab comes back with minutes between two frames. Billed
// in full, that single tick emits every frame of those minutes in one go — an
// unbounded write burst with no budget over it, on the frame the operator
// switched back. A replay is not synchronised to anything, so the honest
// reading of a gap is that the replay was not running: the clock resumes where
// it stopped.
const MAX_TICK_SECONDS = 0.25;

// A marker's data is recorded content: it is written by whatever produced the
// session, so it has no length its author promised to respect and may carry the
// control bytes that were meaningful inside a terminal buffer and are noise
// outside one. The transport bar takes a short printable label or nothing.
const MARKER_LABEL_LIMIT = 48;

const knownKinds: readonly string[] = ["o", "i", "r", "m"];

function markerLabel(data: string): string {
  const printable = data.replace(/[\x00-\x1f\x7f]+/g, " ").trim();
  if (!printable) return "marker";
  if (printable.length <= MARKER_LABEL_LIMIT) return printable;
  return `${printable.slice(0, MARKER_LABEL_LIMIT - 1)}…`;
}

function toCastFrames(frames: RecordingFrameView[]): CastFrame[] {
  const converted: CastFrame[] = [];
  for (const frame of frames) {
    if (!knownKinds.includes(frame.kind)) continue;
    if (typeof frame.time !== "number" || !Number.isFinite(frame.time)) continue;
    converted.push({ time: frame.time, kind: frame.kind as CastFrameKind, data: frame.data });
  }
  return converted;
}

function idleState(): PlaybackState {
  return { playing: false, elapsed: 0, speed: 1, index: 0 };
}

export function CastPlayer({ recording, header, frames, loading, partial }: CastPlayerProps) {
  const containerRef = useRef<HTMLDivElement>(null);
  const termRef = useRef<Terminal | null>(null);
  const stateRef = useRef<PlaybackState>(idleState());
  const framesRef = useRef<CastFrame[]>([]);
  const publishedRef = useRef(-1);
  const seekHandleRef = useRef(0);
  const pendingSeekRef = useRef<number | null>(null);

  const [playback, setPlayback] = useState<PlaybackState>(idleState());
  const [seekTruncated, setSeekTruncated] = useState(false);

  const castFrames = useMemo(() => toCastFrames(frames), [frames]);
  const markers = useMemo(
    () => castFrames.filter((frame) => frame.kind === "m").slice(0, 32),
    [castFrames],
  );
  const duration = useMemo(() => {
    const fromFrames = totalDuration(castFrames);
    if (fromFrames > 0) return fromFrames;
    return recording.durationSeconds > 0 ? recording.durationSeconds : 0;
  }, [castFrames, recording.durationSeconds]);

  framesRef.current = castFrames;

  const publish = useCallback((force: boolean) => {
    const current = stateRef.current;
    const tick = Math.floor(current.elapsed * CLOCK_PUBLISH_STEP);
    if (!force && tick === publishedRef.current) return;
    publishedRef.current = tick;
    setPlayback({ ...current });
  }, []);

  // A replay writes recorded bytes into a terminal that is wired to nothing.
  // "o" is the recorded screen. "i" is what the operator typed, and its echo is
  // already inside the "o" stream, so writing it would double every keystroke.
  // "r" is geometry. "m" is a marker and belongs in the transport bar, never in
  // the buffer — it was never on the operator's screen.
  const writeFrame = useCallback((term: Terminal, frame: CastFrame) => {
    if (frame.kind === "o") {
      term.write(frame.data);
      return;
    }
    if (frame.kind === "r") {
      const size = resizeFromFrame(frame);
      if (!size) return;
      try {
        term.resize(size.columns, size.rows);
      } catch {
        // A geometry the recording claims but the view cannot honour.
      }
    }
  }, []);

  // Build the terminal. The recording decides its geometry: this pane is a
  // replay, so it must never report a size upward — there may well be a live
  // PTY on the other end of that path, and resizing it because somebody opened
  // an audit replay would reflow the running session.
  useEffect(() => {
    const container = containerRef.current;
    if (!container) return;

    const term = new Terminal({
      cursorBlink: false,
      cursorStyle: "bar",
      disableStdin: true,
      convertEol: true,
      fontSize: 12,
      fontFamily: '"SFMono-Regular", Consolas, "Liberation Mono", ui-monospace, monospace',
      lineHeight: 1.4,
      theme: RELAYER_TERMINAL_THEME,
      scrollback: 5000,
      allowProposedApi: true,
    });

    const fitAddon = new FitAddon();
    term.loadAddon(fitAddon);

    const searchAddon = new SearchAddon();
    term.loadAddon(searchAddon);

    termRef.current = term;
    term.open(container);

    if (header.width > 0 && header.height > 0) {
      try {
        term.resize(header.width, header.height);
      } catch {
        // Fall through to whatever xterm settled on.
      }
    } else {
      // A recording whose header never made it to disk. Fitting the pane is a
      // guess, but it is a better one than the 80x24 default.
      try {
        fitAddon.fit();
      } catch {
        // Container may have 0 dimensions initially
      }
    }

    term.attachCustomKeyEventHandler((event: KeyboardEvent) => {
      // If user presses Ctrl+C while text is selected, let browser copy to clipboard
      if (event.type === "keydown" && event.ctrlKey && event.code === "KeyC") {
        if (term.hasSelection()) {
          return false;
        }
        return true;
      }
      return true;
    });

    return () => {
      searchAddon.dispose();
      term.dispose();
      termRef.current = null;
    };
  }, [recording.id, header.width, header.height]);

  // A seek queued for the next animation frame outlives the component unless it
  // is cancelled here: the callback would wake with the terminal already
  // disposed and write into it.
  const cancelPendingSeek = useCallback(() => {
    if (seekHandleRef.current !== 0) {
      window.cancelAnimationFrame(seekHandleRef.current);
      seekHandleRef.current = 0;
    }
    pendingSeekRef.current = null;
  }, []);

  useEffect(() => cancelPendingSeek, [cancelPendingSeek]);

  // A different recording is a different timeline: rewind the clock rather than
  // letting the previous position bleed into it, and drop a seek aimed at the
  // recording that is no longer on screen.
  useEffect(() => {
    cancelPendingSeek();
    stateRef.current = idleState();
    publishedRef.current = -1;
    setSeekTruncated(false);
    setPlayback(idleState());
  }, [recording.id, cancelPendingSeek]);

  // The playback loop. It owns the clock while it runs; React state only
  // mirrors it for the transport bar.
  useEffect(() => {
    if (!playback.playing) return;

    let handle = 0;
    let previous = 0;

    const step = (now: number) => {
      if (previous === 0) {
        previous = now;
        handle = window.requestAnimationFrame(step);
        return;
      }
      const delta = (now - previous) / 1000;
      previous = now;

      // A queued seek is about to install a new clock and a new index. Emitting
      // against the old index first would replay everything between it and the
      // position the operator has already dragged to — the one write burst the
      // seek budget exists to prevent, arriving through the playback loop.
      if (pendingSeekRef.current !== null) {
        handle = window.requestAnimationFrame(step);
        return;
      }

      const result = advance(
        stateRef.current,
        framesRef.current,
        delta > MAX_TICK_SECONDS ? MAX_TICK_SECONDS : delta,
      );
      stateRef.current = result.state;

      const term = termRef.current;
      if (term) {
        for (const frame of result.emit) {
          writeFrame(term, frame);
        }
      }

      if (!result.state.playing) {
        publish(true);
        return;
      }
      publish(false);
      handle = window.requestAnimationFrame(step);
    };

    handle = window.requestAnimationFrame(step);
    return () => window.cancelAnimationFrame(handle);
  }, [playback.playing, publish, writeFrame]);

  const seekTo = useCallback(
    (target: number) => {
      cancelPendingSeek();
      const plan = seekPlan(framesRef.current, target, SEEK_FRAME_BUDGET);
      stateRef.current = { ...stateRef.current, elapsed: target > 0 ? target : 0, index: plan.index };

      const term = termRef.current;
      if (term) {
        term.reset();
        for (const frame of plan.emit) {
          writeFrame(term, frame);
        }
      }

      setSeekTruncated(plan.truncated);
      publish(true);
    },
    [cancelPendingSeek, publish, writeFrame],
  );

  // Dragging the scrubber fires one change event per pixel of travel, and every
  // seek clears the screen and replays up to SEEK_FRAME_BUDGET frames into it.
  // Running one per event queues hundreds of thousands of writes behind a single
  // drag — the exact freeze the budget exists to prevent, reached by doing the
  // bounded work an unbounded number of times. So the clock follows the thumb
  // immediately, keeping the control live, and the screen is rebuilt once per
  // animation frame from the latest position only.
  const queueSeek = useCallback(
    (target: number) => {
      const clamped = Number.isFinite(target) && target > 0 ? target : 0;
      pendingSeekRef.current = clamped;
      stateRef.current = { ...stateRef.current, elapsed: clamped };
      publish(true);

      if (seekHandleRef.current !== 0) return;
      seekHandleRef.current = window.requestAnimationFrame(() => {
        seekHandleRef.current = 0;
        const next = pendingSeekRef.current;
        pendingSeekRef.current = null;
        if (next !== null) seekTo(next);
      });
    },
    [publish, seekTo],
  );

  const handleTogglePlay = () => {
    if (stateRef.current.playing) {
      stateRef.current = { ...stateRef.current, playing: false };
      publish(true);
      return;
    }
    // Pressing play on a finished recording starts it over rather than sitting
    // on the last frame doing nothing.
    if (stateRef.current.index >= framesRef.current.length) {
      seekTo(0);
    }
    stateRef.current = { ...stateRef.current, playing: true };
    publish(true);
  };

  const handleCycleSpeed = () => {
    const current = clampSpeed(stateRef.current.speed);
    const position = PLAYBACK_SPEEDS.indexOf(current);
    const next = PLAYBACK_SPEEDS[(position + 1) % PLAYBACK_SPEEDS.length];
    stateRef.current = { ...stateRef.current, speed: next };
    publish(true);
  };

  const ready = !loading && castFrames.length > 0;
  const scrubMax = duration > 0 ? duration : 1;
  const label = recording.name || recording.sessionID || recording.id;

  // One line, and the most serious reading of the timeline wins it. `partial`
  // ranks above a dropped frame because a replay that simply stops early reads
  // as a session that ended there, and in an audit view that is a false record.
  const notice = recording.truncated
    ? "Recording stopped at its size limit — the tail of the session is missing."
    : partial
      ? "Only the beginning of this recording was loaded — the replay ends before the session did."
      : recording.droppedFrames > 0
        ? "Some frames were dropped while recording — the replay may skip ahead."
        : seekTruncated
          ? "Seeking rebuilt the screen from the most recent frames only."
          : "";

  return (
    <div className="cast-player">
      <div
        ref={containerRef}
        className="cast-player__screen"
        role="log"
        tabIndex={0}
        aria-label={`Replay of recorded session ${label}`}
        aria-live="off"
      >
        {loading && <p className="cast-player__loading">Loading recording…</p>}
        {!loading && castFrames.length === 0 && (
          <p className="cast-player__loading">This recording has no replayable frames.</p>
        )}
      </div>

      {notice && (
        <p className="cast-player__notice" role="status">
          {notice}
        </p>
      )}

      <div className="cast-transport">
        <button
          type="button"
          className="cast-transport__play"
          onClick={handleTogglePlay}
          disabled={!ready}
          aria-label={playback.playing ? "Pause replay" : "Play replay"}
        >
          <span aria-hidden="true">{playback.playing ? "❚❚" : "▶"}</span>
          {playback.playing ? "Pause" : "Play"}
        </button>

        <input
          type="range"
          className="cast-transport__scrub"
          min={0}
          max={scrubMax}
          step={0.1}
          value={playback.elapsed > scrubMax ? scrubMax : playback.elapsed}
          disabled={!ready}
          aria-label="Seek within the recording"
          aria-valuetext={`${formatTimecode(playback.elapsed)} of ${formatTimecode(duration)}`}
          onChange={(event) => queueSeek(Number(event.target.value))}
        />

        <span className="cast-transport__time">
          {formatTimecode(playback.elapsed)} / {formatTimecode(duration)}
        </span>

        <button
          type="button"
          className="cast-transport__speed"
          onClick={handleCycleSpeed}
          disabled={!ready}
          aria-label={`Playback speed ${playback.speed}×, click to change`}
        >
          {playback.speed}×
        </button>

        {markers.length > 0 && (
          <div className="cast-transport__markers" aria-label="Recording markers">
            {markers.map((marker, index) => {
              const text = markerLabel(marker.data);
              return (
                <button
                  key={`${marker.time}-${index}`}
                  type="button"
                  className="cast-transport__marker"
                  onClick={() => seekTo(marker.time)}
                  disabled={!ready}
                  title={`${formatTimecode(marker.time)} · ${text}`}
                >
                  <span className="cast-transport__marker-time">{formatTimecode(marker.time)}</span>
                  {text}
                </button>
              );
            })}
          </div>
        )}
      </div>
    </div>
  );
}
