import type { SafeErrorEvent, SupervisionEvent } from "../types/relayer";

const REDACTED = "[REDACTED]";
const MAX_SAFE_MESSAGE = 240;

const jwtPattern = /\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b/g;
const credentialURLPattern = /([a-z][a-z0-9+.-]*:\/\/)[^\s/@:]+:[^\s/@]+@/gi;
const bearerPattern = /\b(Bearer)\s+[A-Za-z0-9._~+\/-]+=*/gi;
const assignmentPattern = /(password|passphrase|token|api[_-]?key|secret|private[_-]?key|credential|otp|pin|authorization)\s*[:=]\s*([^\s,;]+)/gi;

export function redactForDisplay(value: string): string {
  return value
    .replace(jwtPattern, REDACTED)
    .replace(credentialURLPattern, `$1${REDACTED}@`)
    .replace(bearerPattern, `$1 ${REDACTED}`)
    .replace(assignmentPattern, `$1=${REDACTED}`)
    .slice(0, MAX_SAFE_MESSAGE);
}

export function safeEventSummary(event: SupervisionEvent): string {
  if (event.sensitive || event.type === "credential") {
    return "Confidential input required";
  }
  const summary = redactForDisplay(event.summary.trim());
  return summary || "Interactive confirmation required";
}

// asSentence casts an engine message into something an operator reads.
//
// Go error strings are fragments by convention — lowercase, unpunctuated, meant
// to be wrapped and concatenated — and the engine's are written that way. The
// interface is where they become a sentence, which is why this lives here and
// not in the error value: presentation belongs to the layer that presents.
function asSentence(message: string): string {
  const first = message.charAt(0);
  const cased = first.toLocaleUpperCase() === first ? message : first.toLocaleUpperCase() + message.slice(1);
  return /[.!?…]$/.test(cased) ? cased : `${cased}.`;
}

export function safeError(error: unknown, fallback = "An operation failed."): string {
  if (error instanceof Error && error.message.trim()) {
    return asSentence(redactForDisplay(error.message.trim()));
  }
  if (typeof error === "string" && error.trim()) {
    return asSentence(redactForDisplay(error.trim()));
  }
  return fallback;
}

export function sanitizeErrorEvent(event: SafeErrorEvent): SafeErrorEvent {
  return {
    runID: event.runID,
    code: redactForDisplay(event.code || "unknown_error"),
    message: redactForDisplay(event.message || "An internal error occurred."),
    sessionID: event.sessionID,
    timestamp: event.timestamp,
  };
}

// promptContextLines returns the tail of a pane's output for the decision
// modal.
//
// The bytes are the ones already on the agent card, so this exposes nothing
// new; it puts them where the decision is actually made, instead of behind a
// dialog the operator has to close to read what led to the prompt. The tail is
// bounded in both directions so a single enormous line cannot push the controls
// off the screen.
export function promptContextLines(output: string, maximumLines = 12): string[] {
  const lines = output.replace(/\r/g, "").split("\n");
  while (lines.length > 0 && lines[lines.length - 1].trim() === "") {
    lines.pop();
  }
  return lines
    .slice(Math.max(0, lines.length - maximumLines))
    .map((line) => redactForDisplay(line.length > 200 ? `${line.slice(0, 200)}…` : line));
}
