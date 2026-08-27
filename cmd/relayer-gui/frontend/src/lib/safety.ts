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
    return "Saisie confidentielle requise";
  }
  const summary = redactForDisplay(event.summary.trim());
  return summary || "Validation interactive requise";
}

export function safeError(error: unknown, fallback = "Une opération a échoué."): string {
  if (error instanceof Error && error.message.trim()) {
    return redactForDisplay(error.message.trim());
  }
  if (typeof error === "string" && error.trim()) {
    return redactForDisplay(error.trim());
  }
  return fallback;
}

export function sanitizeErrorEvent(event: SafeErrorEvent): SafeErrorEvent {
  return {
    code: redactForDisplay(event.code || "unknown_error"),
    message: redactForDisplay(event.message || "Une erreur interne est survenue."),
    sessionID: event.sessionID,
    timestamp: event.timestamp,
  };
}
