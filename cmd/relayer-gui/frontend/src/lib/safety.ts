import type { SafeErrorEvent, SupervisionEvent } from "../types/relayer";

const REDACTED = "[REDACTED]";
const MAX_SAFE_MESSAGE = 240;

const jwtPattern = /\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b/g;
const credentialURLPattern = /([a-z][a-z0-9+.-]*:\/\/)[^\s/@:]+:[^\s/@]+@/gi;
const bearerPattern = /\b(Bearer)\s+[A-Za-z0-9._~+\/-]+=*/gi;
const assignmentPattern = /(password|passphrase|token|api[_-]?key|secret|private[_-]?key|credential|otp|pin|authorization)\s*[:=]\s*([^\s,;]+)/gi;

function redactSecrets(value: string): string {
  return value
    .replace(jwtPattern, REDACTED)
    .replace(credentialURLPattern, `$1${REDACTED}@`)
    .replace(bearerPattern, `$1 ${REDACTED}`)
    .replace(assignmentPattern, `$1=${REDACTED}`);
}

// MAX_REDACTION_WINDOW bounds what a redaction is run on. The URL pattern tries
// every start of a run of scheme characters, so its cost grows with the square
// of the longest run: 64 KiB of "a" took two seconds, 256 KiB over thirty, and an
// agent chooses what it prints. Everything shown is cut far below this, and a
// secret that straddles the cut is redacted whole first, since the window is
// several times what is shown.
const MAX_REDACTION_WINDOW = 4096;
const MAX_REDACTION_LINE = 1024;

function boundedWindow(value: string, limit: number): string {
  return value.length > limit ? value.slice(0, limit) : value;
}

// cutAt cuts a text to at most limit characters, counting what a reader counts
// (a code point, not a UTF-16 unit, so an emoji is never split in two), and
// marks the cut.
function cutAt(value: string, limit: number): string {
  if (value.length <= limit) return value;
  const points = Array.from(value);
  return points.length > limit ? `${points.slice(0, limit - 1).join("")}…` : value;
}

export function redactForDisplay(value: string): string {
  return redactSecrets(boundedWindow(value, MAX_REDACTION_WINDOW)).slice(0, MAX_SAFE_MESSAGE);
}

// The sequences a terminal reads as instructions, in the order that keeps one
// from eating another: a string a terminal ends at BEL or ST (a window title,
// a hyperlink), introduced by ESC and a character or by its C1 form, may hold
// anything, so it goes first, bounded in case nothing ever ends it; a CSI sequence, the ones that move the cursor and set colours;
// then every other escape, an optional run of intermediate bytes and a final one
// (a charset designation, "save cursor"), and a lone ESC; then the C1 form of
// CSI and the control characters that are not a line break or a tab.
const terminalStringPattern = /(?:\u001b[\]PX^_]|[\u0090\u0098\u009d\u009e\u009f])[^\u0007\u001b\u009c]{0,2048}(?:\u0007|\u001b\\|\u009c)?/g;
const cursorForwardPattern = /\u001b\[(\d*)C/g;
const csiPattern = /\u001b\[[0-?]*[ -/]*[@-~]/g;
const otherEscapePattern = /\u001b[ -/]*[0-~]?/g;
const c1CsiPattern = /\u009b[0-?]*[ -/]*[@-~]/g;
const controlPattern = /[\u0000-\u0008\u000b-\u001f\u007f-\u009f]/g;
const formatCharacterPattern = /\p{Cf}/gu;

// stripTerminalEscapes is the text of terminal output with its escape sequences
// taken out. A cursor-forward by n columns is n spaces rather than nothing: an
// agent that draws its text with a cursor, as Claude Code does, spaces its words
// that way, and removing the sequences glues them ("echoPROBE_BASH_OK"). Other
// cursor movements are dropped, so a screen the agent painted by positioning
// the cursor reads as the text it wrote, not as the grid a terminal shows.
export function stripTerminalEscapes(value: string): string {
  return value
    .replace(terminalStringPattern, "")
    .replace(cursorForwardPattern, (_match, columns: string) => " ".repeat(Math.min(Math.max(Number(columns || "1"), 1), 200)))
    .replace(csiPattern, "")
    .replace(otherEscapePattern, "")
    .replace(c1CsiPattern, "")
    .replace(controlPattern, "");
}

// commandLines is the command a prompt asks about as the modal shows it: the
// core has already redacted and bounded it, and this is the same care again for
// a value that crossed a boundary. Its line breaks are kept, since flattening
// "rm -rf a" and "ls" onto one line reads as one command with arguments. A viewer
// is sent none, and a confidential prompt shows none.
//
// A format character — a bidi override, a zero-width space — is drawn as a
// visible mark: text a person reads to decide what runs must show what the shell
// will read, and "echo ok" followed by a right-to-left override reorders what
// comes after it on screen, and "rm", a zero-width space and " -rf x" read as the
// command "rm -rf x" and are not it.
export function commandLines(command: string | undefined, maximumLines = 12): string[] {
  if (!command) return [];
  const lines = redactSecrets(stripTerminalEscapes(boundedWindow(command, MAX_REDACTION_WINDOW).replace(/\r\n?/g, "\n")).replace(formatCharacterPattern, "�"))
    .split("\n")
    .map((line) => line.replace(/\s+$/, ""));
  while (lines.length > 0 && lines[0] === "") lines.shift();
  while (lines.length > 0 && lines[lines.length - 1] === "") lines.pop();
  const shown = lines.slice(0, maximumLines).map((line) => cutAt(line, 240));
  return lines.length > maximumLines ? [...shown, "…"] : shown;
}

// isConfidential is the one masking rule of the interface: a prompt is masked
// only when its text is a secret — the core marked it sensitive, or it asks
// for a credential. High risk is not a secret: a high-risk prompt keeps an
// honest label, shows its bounded, redacted summary and takes a normal,
// visible answer.
export function isConfidential(event: SupervisionEvent): boolean {
  return event.sensitive || event.type === "credential";
}

export function safeEventSummary(event: SupervisionEvent): string {
  if (isConfidential(event)) {
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
  // The output is the terminal's own bytes, escape sequences included; those
  // go first, before the tail is cut and before anything is redacted, so that
  // a secret an escape sequence split in two is one text again when it is
  // redacted.
  const lines = stripTerminalEscapes(output.replace(/\r/g, "")).split("\n");
  while (lines.length > 0 && lines[lines.length - 1].trim() === "") {
    lines.pop();
  }
  return lines
    .slice(Math.max(0, lines.length - maximumLines))
    .map((line) => cutAt(redactSecrets(boundedWindow(line, MAX_REDACTION_LINE)), 200));
}
