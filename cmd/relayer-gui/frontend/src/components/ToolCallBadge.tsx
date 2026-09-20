import type { ToolCallView } from "../types/relayer";

interface ToolCallBadgeProps {
  toolCall?: ToolCallView;
}

// An MCP tool is addressed as mcp__<server>__<tool>. Showing the two halves
// separately is what lets an operator tell "the filesystem server's delete" from
// "some server called delete" at a glance.
//
// Everything rendered here is agent-controlled terminal text. React escapes it,
// and it is shown so the operator can judge the call before answering -- it is
// never an instruction, and nothing in it is executed or followed.
export function ToolCallBadge({ toolCall }: ToolCallBadgeProps) {
  if (!toolCall) return null;

  const params = toolCall.params ?? [];

  return (
    <section
      className={`tool-call tool-call--${toolCall.risk}`}
      aria-label={`Tool call ${toolCall.server} ${toolCall.tool}`}
    >
      <header className="tool-call__header">
        <span className="tool-call__icon" aria-hidden="true">
          🔧
        </span>
        <span className="tool-call__name">
          <span className="tool-call__server">{toolCall.server}</span>
          <span className="tool-call__separator" aria-hidden="true">
            /
          </span>
          <strong className="tool-call__tool">{toolCall.tool}</strong>
        </span>
        <span className={`tool-call__risk risk-text risk-text--${toolCall.risk}`}>
          {toolCall.risk}
        </span>
      </header>

      {params.length > 0 && (
        <dl className="tool-call__params">
          {params.map((param) => (
            <div className="tool-call__param" key={param.name}>
              <dt className="tool-call__param-name">{param.name}</dt>
              <dd className="tool-call__param-value">
                {param.value}
                {param.truncated && (
                  <span className="tool-call__ellipsis" title="Value truncated for display">
                    {" "}
                    …
                  </span>
                )}
              </dd>
            </div>
          ))}
        </dl>
      )}

      {params.length === 0 && (
        <p className="tool-call__empty">No arguments were readable from the prompt.</p>
      )}

      {toolCall.paramsTruncated && (
        <p className="tool-call__notice" role="status">
          Further arguments were not shown.
        </p>
      )}

      {/* Stated on the badge itself, not only in the documentation: an operator
          reading it must not take it for an interception point. */}
      <p className="tool-call__notice tool-call__notice--caveat">
        Read from terminal output, so it may be incomplete. Relayer does not intercept the
        call itself.
      </p>
    </section>
  );
}
