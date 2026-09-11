import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { TerminalSnapshotView } from "./TerminalSnapshotView";

describe("TerminalSnapshotView", () => {
  it("renders the terminal shell and snapshot container with accessibility attributes", () => {
    const markup = renderToStaticMarkup(
      <TerminalSnapshotView
        runID="run-1"
        sessionID="agent-1"
        label="Output from Test Agent"
        output=""
        revision={3}
        onResize={async () => {}}
      />
    );

    expect(markup).toContain('class="terminal-shell"');
    expect(markup).toContain('class="terminal-snapshot"');
    expect(markup).toContain('role="log"');
    expect(markup).toContain('aria-label="Output from Test Agent"');
    expect(markup).toContain('data-revision="3"');
    expect(markup).toContain("Waiting for output");
  });

  it("omits the empty placeholder when output is present", () => {
    const markup = renderToStaticMarkup(
      <TerminalSnapshotView
        runID="run-1"
        sessionID="agent-1"
        label="Output from Test Agent"
        output="\x1b[32m[OK]\x1b[0m Ready"
        revision={4}
        onResize={async () => {}}
      />
    );

    expect(markup).toContain('class="terminal-shell"');
    expect(markup).not.toContain("Waiting for output");
  });
});
