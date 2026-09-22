import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import type { RecordingView, RelayerBridge } from "../types/relayer";
import { RecordingsContentView, RecordingsPanel, recordingPermissions } from "./RecordingsPanel";

function sampleRecordings(): RecordingView[] {
  return [
    {
      id: "rec-1",
      runID: "run-1",
      sessionID: "sess-1",
      agentID: "claude-code",
      name: "Agent A · Claude",
      backend: "pty",
      adapter: "claude",
      startedAt: "2026-09-19T09:14:02Z",
      endedAt: "2026-09-19T09:16:35Z",
      durationSeconds: 153,
      width: 120,
      height: 32,
      bytes: 48213,
      frames: 128,
      truncated: false,
      droppedFrames: 0,
      inputRecorded: false,
      redacted: true,
      exitCode: 0,
      active: false,
    },
    {
      id: "rec-2",
      runID: "run-1",
      sessionID: "sess-2",
      agentID: "codex-cli",
      name: "Agent B · Codex",
      backend: "pty",
      adapter: "codex",
      startedAt: "2026-09-19T09:20:41Z",
      durationSeconds: 62,
      width: 100,
      height: 30,
      bytes: 1048576,
      frames: 44,
      truncated: true,
      droppedFrames: 12,
      inputRecorded: true,
      redacted: true,
      active: true,
    },
  ];
}

describe("RecordingsContentView", () => {
  it("renders the recordings table with metadata and status badges", () => {
    const markup = renderToStaticMarkup(
      <RecordingsContentView
        recordings={sampleRecordings()}
        selectedID={null}
        frames={[]}
        deletable={true}
        confirmingDeleteID={null}
        onSelect={() => {}}
        onConfirmDelete={() => {}}
        onDelete={() => {}}
        onRefresh={() => {}}
      />,
    );

    expect(markup).toContain("2 recordings");
    expect(markup).toContain("Agent A · Claude");
    expect(markup).toContain("sess-1");
    expect(markup).toContain("2026-09-19 09:14:02");
    expect(markup).toContain("2m 33s");
    expect(markup).toContain("47.1 KB");
    expect(markup).toContain("Truncated");
    expect(markup).toContain("12 dropped");
    expect(markup).toContain("Input recorded");
    expect(markup).toContain("Redacted");
  });

  it("marks an in-progress recording and offers no delete control for it", () => {
    const markup = renderToStaticMarkup(
      <RecordingsContentView
        recordings={sampleRecordings()}
        selectedID={null}
        frames={[]}
        deletable={true}
        confirmingDeleteID={null}
        onSelect={() => {}}
        onConfirmDelete={() => {}}
        onDelete={() => {}}
        onRefresh={() => {}}
      />,
    );

    expect(markup).toContain("Recording</span>");
    expect(markup).toContain("Still being written");
    // The finished recording keeps its delete control; the active one does not,
    // so exactly one Delete button is offered for the two rows.
    expect(markup.match(/>Delete</g) ?? []).toHaveLength(1);
  });

  it("hides every delete control when the panel is read-only", () => {
    const markup = renderToStaticMarkup(
      <RecordingsContentView
        recordings={sampleRecordings()}
        selectedID={null}
        frames={[]}
        deletable={false}
        confirmingDeleteID={null}
        onSelect={() => {}}
        onConfirmDelete={() => {}}
        onDelete={() => {}}
        onRefresh={() => {}}
      />,
    );

    expect(markup).not.toContain(">Delete<");
  });

  it("asks for confirmation before deleting a recording", () => {
    const markup = renderToStaticMarkup(
      <RecordingsContentView
        recordings={sampleRecordings()}
        selectedID={null}
        frames={[]}
        deletable={true}
        confirmingDeleteID="rec-1"
        onSelect={() => {}}
        onConfirmDelete={() => {}}
        onDelete={() => {}}
        onRefresh={() => {}}
      />,
    );

    expect(markup).toContain("Delete this recording?");
    expect(markup).toContain("Cancel");
  });

  it("renders empty, loading, error and unsupported states cleanly", () => {
    const emptyMarkup = renderToStaticMarkup(
      <RecordingsContentView
        recordings={[]}
        selectedID={null}
        frames={[]}
        confirmingDeleteID={null}
        onSelect={() => {}}
        onConfirmDelete={() => {}}
        onDelete={() => {}}
        onRefresh={() => {}}
      />,
    );
    expect(emptyMarkup).toContain("No session has been recorded yet.");

    const loadingMarkup = renderToStaticMarkup(
      <RecordingsContentView
        recordings={[]}
        loading={true}
        selectedID={null}
        frames={[]}
        confirmingDeleteID={null}
        onSelect={() => {}}
        onConfirmDelete={() => {}}
        onDelete={() => {}}
        onRefresh={() => {}}
      />,
    );
    expect(loadingMarkup).toContain("Loading recorded sessions…");

    const errorMarkup = renderToStaticMarkup(
      <RecordingsContentView
        recordings={[]}
        error={true}
        selectedID={null}
        frames={[]}
        confirmingDeleteID={null}
        onSelect={() => {}}
        onConfirmDelete={() => {}}
        onDelete={() => {}}
        onRefresh={() => {}}
      />,
    );
    expect(errorMarkup).toContain("Recordings unavailable");

    const unsupportedMarkup = renderToStaticMarkup(
      <RecordingsContentView
        recordings={[]}
        supported={false}
        selectedID={null}
        frames={[]}
        confirmingDeleteID={null}
        onSelect={() => {}}
        onConfirmDelete={() => {}}
        onDelete={() => {}}
        onRefresh={() => {}}
      />,
    );
    expect(unsupportedMarkup).toContain("This build has no recording store.");
  });

  it("prompts for a selection and reports a chunk that could not be read", () => {
    const idleMarkup = renderToStaticMarkup(
      <RecordingsContentView
        recordings={sampleRecordings()}
        selectedID={null}
        frames={[]}
        confirmingDeleteID={null}
        onSelect={() => {}}
        onConfirmDelete={() => {}}
        onDelete={() => {}}
        onRefresh={() => {}}
      />,
    );
    expect(idleMarkup).toContain("Select a recording to replay it.");

    const failedMarkup = renderToStaticMarkup(
      <RecordingsContentView
        recordings={sampleRecordings()}
        selectedID="rec-1"
        frames={[]}
        playerError={true}
        confirmingDeleteID={null}
        onSelect={() => {}}
        onConfirmDelete={() => {}}
        onDelete={() => {}}
        onRefresh={() => {}}
      />,
    );
    expect(failedMarkup).toContain("This recording could not be read");
  });

  // The panel stops after MAX_CHUNKS pages. A recording longer than that is cut
  // where the reading stopped, not where the session ended, and the replay has
  // to say so or it stands as a complete record of a session it only partly
  // holds.
  it("carries a partial load through to the replay notice", () => {
    const markup = renderToStaticMarkup(
      <RecordingsContentView
        recordings={sampleRecordings()}
        selectedID="rec-1"
        header={{ version: 2, width: 120, height: 32 }}
        frames={[{ time: 0.5, kind: "o", data: "$ relayer run\r\n" }]}
        partial={true}
        confirmingDeleteID={null}
        onSelect={() => {}}
        onConfirmDelete={() => {}}
        onDelete={() => {}}
        onRefresh={() => {}}
      />,
    );

    expect(markup).toContain("Only the beginning of this recording was loaded");
  });
});

describe("RecordingsPanel", () => {
  it("renders dialog shell with accessibility attributes and the download action", () => {
    const dummyBridge: RelayerBridge = {
      getState: async () => ({} as never),
      runPreflight: async () => ({} as never),
      submitDecision: async () => {},
      submitAutomaticDecision: async () => {},
      submitLine: async () => {},
      resizeSession: async () => {},
      stopSession: async () => {},
      startSession: async () => {},
      restartSession: async () => {},
      getAgentProfiles: async () => ({} as never),
      saveAgentProfiles: async () => ({} as never),
      saveAgentProfilesAndRestart: async () => ({} as never),
      getFullSettings: async () => ({} as never),
      saveFullSettings: async () => ({} as never),
      stopRun: async () => ({} as never),
      getAuditSummary: async () => ({} as never),
      getAuditEntries: async () => [],
      verifyAuditJournal: async () => ({} as never),
      exportAuditReport: async () => "[]",
      getTelemetrySnapshot: async () => ({} as never),
      listRecordings: async () => sampleRecordings(),
      getRecording: async () => sampleRecordings()[0],
      readRecordingChunk: async () => ({} as never),
      exportRecording: async () => "",
      deleteRecording: async () => {},
      on: () => () => {},
    };

    const markup = renderToStaticMarkup(
      <RecordingsPanel bridge={dummyBridge} onClose={() => {}} />,
    );

    expect(markup).toContain('role="dialog"');
    expect(markup).toContain('aria-modal="true"');
    expect(markup).toContain('id="recordings-title"');
    expect(markup).toContain("Recorded Sessions");
    expect(markup).toContain("Download .cast");
  });

  it("disables download action and hides delete action when readOnly is true", () => {
    const dummyBridge: RelayerBridge = {
      getState: async () => ({} as never),
      runPreflight: async () => ({} as never),
      submitDecision: async () => {},
      submitAutomaticDecision: async () => {},
      submitLine: async () => {},
      resizeSession: async () => {},
      stopSession: async () => {},
      startSession: async () => {},
      restartSession: async () => {},
      getAgentProfiles: async () => ({} as never),
      saveAgentProfiles: async () => ({} as never),
      saveAgentProfilesAndRestart: async () => ({} as never),
      getFullSettings: async () => ({} as never),
      saveFullSettings: async () => ({} as never),
      stopRun: async () => ({} as never),
      getAuditSummary: async () => ({} as never),
      getAuditEntries: async () => [],
      verifyAuditJournal: async () => ({} as never),
      exportAuditReport: async () => "[]",
      getTelemetrySnapshot: async () => ({} as never),
      listRecordings: async () => sampleRecordings(),
      getRecording: async () => sampleRecordings()[0],
      readRecordingChunk: async () => ({} as never),
      exportRecording: async () => "",
      deleteRecording: async () => {},
      on: () => () => {},
    };

    const markup = renderToStaticMarkup(
      <RecordingsPanel bridge={dummyBridge} readOnly={true} onClose={() => {}} />,
    );

    expect(markup).toContain('disabled=""');
    expect(markup).toContain("Download .cast");
  });

  it("never lets a viewer delete or export, whatever the bridge offers", () => {
    // The panel test above renders before any recording is selected, so it
    // passed even with the read-only guard removed. The rule itself is checked
    // here, and the rendered rows below.
    const bridge = { deleteRecording: async () => {}, exportRecording: async () => "" };
    expect(recordingPermissions(bridge, true)).toEqual({ deletable: false, exportable: false });
    expect(recordingPermissions(bridge, false)).toEqual({ deletable: true, exportable: true });
    expect(recordingPermissions({}, false)).toEqual({ deletable: false, exportable: false });

    const recordings = sampleRecordings().map((recording) => ({ ...recording, active: false }));
    const render = (deletable: boolean) =>
      renderToStaticMarkup(
        <RecordingsContentView
          recordings={recordings}
          selectedID={recordings[0].id}
          frames={[]}
          deletable={deletable}
          confirmingDeleteID={null}
          onSelect={() => {}}
          onConfirmDelete={() => {}}
          onDelete={() => {}}
          onRefresh={() => {}}
        />,
      );
    expect(render(true)).toContain("Delete");
    expect(render(false)).not.toContain(">Delete<");
  });
});

