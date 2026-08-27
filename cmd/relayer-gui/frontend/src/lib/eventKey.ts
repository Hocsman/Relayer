export function supervisionEventKey(sessionID: string, eventID: string): string {
  return `${sessionID}\u0000${eventID}`;
}
