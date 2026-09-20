import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { ToolCallBadge } from "./ToolCallBadge";
import type { ToolCallView } from "../types/relayer";

function toolCall(overrides: Partial<ToolCallView> = {}): ToolCallView {
  return {
    server: "github",
    tool: "create_issue",
    risk: "high",
    params: [
      { name: "repo", value: "Hocsman/Relayer" },
      { name: "title", value: "Crash on start" },
    ],
    ...overrides,
  };
}

describe("ToolCallBadge", () => {
  it("renders nothing when the prompt is not a tool call", () => {
    expect(renderToStaticMarkup(<ToolCallBadge />)).toBe("");
    expect(renderToStaticMarkup(<ToolCallBadge toolCall={undefined} />)).toBe("");
  });

  it("shows the server and the tool separately", () => {
    const markup = renderToStaticMarkup(<ToolCallBadge toolCall={toolCall()} />);
    expect(markup).toContain("github");
    expect(markup).toContain("create_issue");
    expect(markup).toContain("tool-call__server");
    expect(markup).toContain("tool-call__tool");
  });

  it("carries the risk into the class so a dangerous call is visibly dangerous", () => {
    expect(renderToStaticMarkup(<ToolCallBadge toolCall={toolCall()} />)).toContain(
      "tool-call--high",
    );
    expect(
      renderToStaticMarkup(<ToolCallBadge toolCall={toolCall({ risk: "low" })} />),
    ).toContain("tool-call--low");
    expect(
      renderToStaticMarkup(<ToolCallBadge toolCall={toolCall({ risk: "unknown" })} />),
    ).toContain("tool-call--unknown");
  });

  it("lists the arguments an operator is being asked to approve", () => {
    const markup = renderToStaticMarkup(<ToolCallBadge toolCall={toolCall()} />);
    expect(markup).toContain("repo");
    expect(markup).toContain("Hocsman/Relayer");
    expect(markup).toContain("title");
    expect(markup).toContain("Crash on start");
  });

  it("says so when no argument could be read, rather than looking argument-free", () => {
    const markup = renderToStaticMarkup(
      <ToolCallBadge toolCall={toolCall({ params: [] })} />,
    );
    expect(markup).toContain("No arguments were readable");
  });

  it("reports a truncated value and dropped arguments", () => {
    const markup = renderToStaticMarkup(
      <ToolCallBadge
        toolCall={toolCall({
          params: [{ name: "body", value: "a".repeat(40), truncated: true }],
          paramsTruncated: true,
        })}
      />,
    );
    expect(markup).toContain("Value truncated for display");
    expect(markup).toContain("Further arguments were not shown");
  });

  it("always states that it does not intercept the call", () => {
    // An operator who reads the badge as an enforcement point would trust it to
    // stop something it never stops.
    const markup = renderToStaticMarkup(<ToolCallBadge toolCall={toolCall()} />);
    expect(markup).toContain("does not intercept");
  });

  it("escapes agent-controlled text rather than rendering it as markup", () => {
    // Every field here is terminal output written by the agent.
    const markup = renderToStaticMarkup(
      <ToolCallBadge
        toolCall={toolCall({
          server: "<img src=x onerror=alert(1)>",
          tool: "</section><script>alert(2)</script>",
          params: [{ name: "path", value: "<b>bold</b>" }],
        })}
      />,
    );
    expect(markup).not.toContain("<script>");
    expect(markup).not.toContain("<img src=x");
    expect(markup).not.toContain("<b>bold</b>");
    expect(markup).toContain("&lt;");
  });
});
