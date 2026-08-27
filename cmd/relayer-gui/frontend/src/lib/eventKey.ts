export function supervisionEventKey(runID: string, sessionID: string, eventID: string): string {
  return `${runID}\u0000${sessionID}\u0000${eventID}`;
}
