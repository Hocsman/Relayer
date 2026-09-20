import type { HandView, PresenceMember, PresenceView } from "../types/relayer";

interface PresenceStripProps {
  presence?: PresenceView;
  hand?: HandView;
  selfConnID?: string;
}

// Two initials are enough to recognise a colleague in a roster of at most a
// dozen, and they are far cheaper than avatars the gateway would have to serve.
function initials(identity: string): string {
  const cleaned = identity.trim();
  if (!cleaned) return "?";
  const parts = cleaned.split(/[\s._-]+/).filter(Boolean);
  if (parts.length >= 2) {
    return (parts[0][0] + parts[1][0]).toLocaleUpperCase();
  }
  return cleaned.slice(0, 2).toLocaleUpperCase();
}

function memberTitle(member: PresenceMember, isSelf: boolean): string {
  const role = member.role === "viewer" ? "read-only" : "operator";
  const who = isSelf ? `${member.identity} (you)` : member.identity;
  if (member.holdsHand) return `${who} — ${role}, has the terminal`;
  if (member.requestingHand) return `${who} — ${role}, asking for the terminal`;
  return `${who} — ${role}, watching`;
}

// The strip answers one question at a glance: who else is on this terminal, and
// who may currently type into it. It is an indicator, never a control.
export function PresenceStrip({ presence, hand, selfConnID }: PresenceStripProps) {
  const members = presence?.members ?? [];
  if (members.length === 0 && (!hand || hand.state === "free")) {
    return null;
  }

  const holderIsSelf = Boolean(hand?.holderConnID && hand.holderConnID === selfConnID);
  const handLabel =
    !hand || hand.state === "free"
      ? "Terminal free"
      : holderIsSelf
        ? "You have the terminal"
        : `${hand.holderIdentity || "another operator"} has the terminal`;

  const others = members.filter((member) => member.connID !== selfConnID);

  return (
    <div className="presence-strip">
      <span
        className={
          hand && hand.state !== "free" && !holderIsSelf
            ? "presence-strip__hand presence-strip__hand--locked"
            : "presence-strip__hand"
        }
      >
        <span aria-hidden="true">{hand && hand.state !== "free" ? "✋" : "⌨️"}</span>
        {handLabel}
      </span>

      {others.length > 0 && (
        <span className="presence-strip__observers">
          {others.map((member) => (
            <span
              key={member.connID}
              className={
                member.holdsHand
                  ? "presence-strip__avatar presence-strip__avatar--holder"
                  : member.requestingHand
                    ? "presence-strip__avatar presence-strip__avatar--requesting"
                    : "presence-strip__avatar"
              }
              title={memberTitle(member, false)}
            >
              {initials(member.identity)}
            </span>
          ))}
        </span>
      )}

      <span className="presence-strip__count">
        {others.length === 0
          ? "No one else watching"
          : others.length === 1
            ? "1 other watching"
            : `${others.length} others watching`}
      </span>
    </div>
  );
}
