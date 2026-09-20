import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import type {
  RecordingFrameView,
  RecordingHeaderView,
  RecordingView,
} from "../types/relayer";
import { CastPlayer } from "./CastPlayer";

// jsdom has no canvas and the suite runs in node, so no test here may let xterm
// be constructed. renderToStaticMarkup never runs effects, which is exactly
// where the terminal is built: what these assertions cover is the transport
// shell around it.

function sampleRecording(overrides: Partial<RecordingView> = {}): RecordingView {
  return {
    id: "rec-1",
    runID: "run-1",
    sessionID: "sess-1",
    agentID: "claude-code",
    name: "deploy rehearsal",
    backend: "pty",
    adapter: "claude",
    startedAt: "2026-09-11T12:00:00Z",
    endedAt: "2026-09-11T12:01:07Z",
    durationSeconds: 67,
    width: 120,
    height: 30,
    bytes: 4096,
    frames: 4,
    truncated: false,
    droppedFrames: 0,
    inputRecorded: false,
    redacted: true,
    exitCode: 0,
    active: false,
    ...overrides,
  };
}

function sampleHeader(): RecordingHeaderView {
  return { version: 2, width: 120, height: 30, timestamp: 1700000000, title: "deploy" };
}

function sampleFrames(): RecordingFrameView[] {
  return [
    { time: 0.1, kind: "o", data: "$ relayer run\r\n" },
    { time: 1, kind: "r", data: "80x24" },
    { time: 12, kind: "m", data: "migration applied" },
    { time: 67, kind: "o", data: "done\r\n" },
  ];
}

describe("CastPlayer", () => {
  it("renders an accessible transport bar with the recording timeline", () => {
    const markup = renderToStaticMarkup(
      <CastPlayer
        recording={sampleRecording()}
        header={sampleHeader()}
        frames={sampleFrames()}
        loading={false}
      />,
    );

    expect(markup).toContain('class="cast-player"');
    expect(markup).toContain('class="cast-player__screen"');
    expect(markup).toContain('class="cast-transport"');
    expect(markup).toContain('class="cast-transport__play"');
    expect(markup).toContain('class="cast-transport__scrub"');
    expect(markup).toContain('class="cast-transport__time"');
    expect(markup).toContain('class="cast-transport__speed"');

    expect(markup).toContain('aria-label="Play replay"');
    expect(markup).toContain('aria-label="Seek within the recording"');
    expect(markup).toContain('aria-label="Replay of recorded session deploy rehearsal"');
    expect(markup).toContain('type="range"');

    // Position and duration, both from the frame timeline.
    expect(markup).toContain("0:00 / 1:07");
    expect(markup).toContain("1×");
  });

  it("renders every control enabled once frames are loaded", () => {
    const markup = renderToStaticMarkup(
      <CastPlayer
        recording={sampleRecording()}
        header={sampleHeader()}
        frames={sampleFrames()}
        loading={false}
      />,
    );

    expect(markup).not.toContain("disabled");
  });

  it("renders the loading state with the controls disabled", () => {
    const markup = renderToStaticMarkup(
      <CastPlayer
        recording={sampleRecording()}
        header={sampleHeader()}
        frames={[]}
        loading={true}
      />,
    );

    expect(markup).toContain("Loading recording…");
    expect(markup).toContain('class="cast-transport__play"');
    expect(markup).toContain("disabled");
    // The duration still comes from the recording metadata while frames load.
    expect(markup).toContain("0:00 / 1:07");
  });

  it("renders an empty state when the recording holds no replayable frames", () => {
    const markup = renderToStaticMarkup(
      <CastPlayer
        recording={sampleRecording({ durationSeconds: 0 })}
        header={sampleHeader()}
        frames={[]}
        loading={false}
      />,
    );

    expect(markup).toContain("This recording has no replayable frames.");
    expect(markup).toContain("0:00 / 0:00");
  });

  it("renders marker frames as transport chips and never as terminal bytes", () => {
    const markup = renderToStaticMarkup(
      <CastPlayer
        recording={sampleRecording()}
        header={sampleHeader()}
        frames={sampleFrames()}
        loading={false}
      />,
    );

    expect(markup).toContain('class="cast-transport__markers"');
    expect(markup).toContain('class="cast-transport__marker"');
    expect(markup).toContain("migration applied");
    expect(markup).toContain('class="cast-transport__marker-time"');
    expect(markup).toContain(">0:12<");

    // The screen region stays empty: frames are written by the playback loop,
    // never rendered into the markup.
    expect(markup).toContain('class="cast-player__screen"');
    expect(markup).not.toContain("relayer run");
  });

  it("warns when the recording itself is incomplete", () => {
    const truncated = renderToStaticMarkup(
      <CastPlayer
        recording={sampleRecording({ truncated: true })}
        header={sampleHeader()}
        frames={sampleFrames()}
        loading={false}
      />,
    );
    expect(truncated).toContain('class="cast-player__notice"');
    expect(truncated).toContain("Recording stopped at its size limit");

    const dropped = renderToStaticMarkup(
      <CastPlayer
        recording={sampleRecording({ droppedFrames: 12 })}
        header={sampleHeader()}
        frames={sampleFrames()}
        loading={false}
      />,
    );
    expect(dropped).toContain("Some frames were dropped while recording");

    const clean = renderToStaticMarkup(
      <CastPlayer
        recording={sampleRecording()}
        header={sampleHeader()}
        frames={sampleFrames()}
        loading={false}
      />,
    );
    expect(clean).not.toContain("cast-player__notice");
  });

  it("says so when only part of the recording was loaded", () => {
    const markup = renderToStaticMarkup(
      <CastPlayer
        recording={sampleRecording()}
        header={sampleHeader()}
        frames={sampleFrames()}
        loading={false}
        partial={true}
      />,
    );
    // Without this the replay simply stops early and reads as a session that
    // ended there, which in an audit view is a false record.
    expect(markup).toContain("Only the beginning of this recording was loaded");
  });

  // A marker's data is recorded content of unbounded length, and it lands both
  // in the chip's text and in its title attribute.
  it("bounds a marker label and strips its control bytes", () => {
    const markup = renderToStaticMarkup(
      <CastPlayer
        recording={sampleRecording()}
        header={sampleHeader()}
        frames={[
          { time: 1, kind: "m", data: `checkpoint\x07\x1b[31m one${"x".repeat(4000)}` },
          { time: 2, kind: "m", data: "\x00\x00" },
        ]}
        loading={false}
      />,
    );

    expect(markup).not.toContain("x".repeat(100));
    expect(markup).not.toContain("\x07");
    expect(markup).toContain("…");
    // A marker whose data is nothing but control bytes still gets a label.
    expect(markup).toContain(">marker<");
  });

  it("falls back to the session identifier when the recording has no name", () => {
    const markup = renderToStaticMarkup(
      <CastPlayer
        recording={sampleRecording({ name: undefined })}
        header={sampleHeader()}
        frames={sampleFrames()}
        loading={false}
      />,
    );
    expect(markup).toContain('aria-label="Replay of recorded session sess-1"');
  });

  it("drops frames of an unknown kind instead of failing to render", () => {
    const markup = renderToStaticMarkup(
      <CastPlayer
        recording={sampleRecording()}
        header={sampleHeader()}
        frames={[
          { time: 0.1, kind: "z", data: "unsupported" },
          { time: 5, kind: "m", data: "checkpoint" },
        ]}
        loading={false}
      />,
    );
    expect(markup).toContain("checkpoint");
    expect(markup).not.toContain("unsupported");
    expect(markup).toContain("0:00 / 0:05");
  });
});
