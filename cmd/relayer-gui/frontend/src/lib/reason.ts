// The reason codes the supervision core puts on a prompt's evaluation, in the
// words an operator reads. The core already passes every reason through an
// allowlist (safeReason) and replaces anything else with "unknown", so this
// table is presentation only: it must never be what keeps agent text off the
// screen.
//
// The first group is the web's equivalent of the TUI's LIMIT • ASK tag: the
// policy would have answered, and a guard handed the prompt to a person.
const reasonWords: Record<string, string> = {
  consecutive_auto_limit: "Automatic answer limit reached · a person decides",
  rate_limit_exceeded: "Automatic answer rate limit reached · a person decides",
  repeat_after_delivery: "Same question again right after an answer · a person decides",
  operator_attached: "An operator holds the terminal · a person decides",
  typed_at_terminal: "Keys were typed at the terminal while it was shown · answer it there",
  fallback_unsupported: "The adapter cannot encode the policy's answer · a person decides",
  fallback_stale: "The prompt changed before the answer arrived",
  audit_unavailable: "Audit journal unavailable · nothing more is sent",
  sensitive_event: "Confidential prompt · never answered automatically",
  dry_run: "Dry run · the policy only proposes",
  risk_not_low: "Risk is not low · a person decides",
  engine_unavailable: "Policy engine unavailable · a person decides",
  invalid_event: "Unrecognised prompt · a person decides",
  non_actionable: "Nothing to answer",
  default_action: "Default policy action",
  rule_match: "Matched a policy rule",
  sensitive_path_blocked: "Touches a sensitive path · blocked by a guardrail",
  outside_workspace_blocked: "Outside the workspace · blocked by a guardrail",
  destructive_command_blocked: "Destructive command · blocked by a guardrail",
  exfiltration_attempt_blocked: "Possible data exfiltration · blocked by a guardrail",
  guardrail_pattern_blocked: "Matched a guardrail pattern",
  delivery_uncertain: "Delivery indeterminate · the session is frozen",
  runtime_stopped: "The run stopped before the answer was sent",
  delivery_applied: "Answer delivered",
  agent_withdrew_occurrence: "The agent withdrew the prompt",
};

const MAX_RAW_REASON = 64;

// reasonText returns one line of plain text for a reason code, or undefined
// when there is nothing to say. A code this page does not know is shown as the
// code itself: a server newer than the page may add one, and the raw code is
// more use than silence about why a prompt was asked. It is reduced to code
// characters and bounded, so the fallback can never become a sentence or markup
// that somebody else wrote.
export function reasonText(code: string): string | undefined {
  const trimmed = code.trim();
  if (Object.prototype.hasOwnProperty.call(reasonWords, trimmed)) return reasonWords[trimmed];
  const raw = trimmed.replace(/[^A-Za-z0-9_.-]/g, "").slice(0, MAX_RAW_REASON);
  return raw || undefined;
}
