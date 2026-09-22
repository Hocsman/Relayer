import { sanitizeErrorEvent } from "../lib/safety";
import { supervisionEventKey } from "../lib/eventKey";
import type {
  AppState,
  HandView,
  PresenceView,
  SafeErrorEvent,
  SnapshotEvent,
  StatusEvent,
  SupervisionEvent,
} from "../types/relayer";

const MAX_OUTPUT_CHARS = 512 * 1024;
const MAX_PENDING_EVENTS = 64;
const MAX_ERRORS = 40;
const MAX_SHARED_SESSIONS = 32;

export interface RelayerUIState {
  connection: "loading" | "ready" | "failed";
  app: AppState | null;
  errors: SafeErrorEvent[];
  // Both are complete snapshots keyed by lowercase sessionID: the gateway never
  // sends a delta, so a replaced entry is always the whole truth.
  presence: Record<string, PresenceView>;
  hand: Record<string, HandView>;
  fatalError?: string;
}

export type RelayerAction =
  | { type: "loaded"; state: AppState }
  | { type: "loadFailed"; message: string }
  | { type: "snapshot"; snapshot: SnapshotEvent }
  | { type: "event"; event: SupervisionEvent }
  | { type: "status"; status: StatusEvent }
  | { type: "error"; error: SafeErrorEvent }
  | { type: "presence"; presence: PresenceView }
  | { type: "hand"; hand: HandView }
  | {
      type: "delivery";
      runID: string;
      sessionID: string;
      eventID: string;
      status: SupervisionEvent["deliveryStatus"];
    };

export const initialRelayerState: RelayerUIState = {
  connection: "loading",
  app: null,
  errors: [],
  presence: {},
  hand: {},
};

// Sharing snapshots are replaced wholesale rather than merged, and the map is
// bounded like every other untrusted-growth surface in this reducer: a gateway
// that names sessions this client has never seen must not grow it without end.
function withSharedEntry<T>(
  current: Record<string, T>,
  sessionID: string,
  value: T,
): Record<string, T> {
  const key = sessionID.toLocaleLowerCase();
  const next = { ...current, [key]: value };
  const keys = Object.keys(next);
  if (keys.length <= MAX_SHARED_SESSIONS) return next;

  for (const stale of keys.slice(0, keys.length - MAX_SHARED_SESSIONS)) {
    if (stale !== key) delete next[stale];
  }
  return next;
}

function boundedOutput(output: string): string {
  return output.length <= MAX_OUTPUT_CHARS ? output : output.slice(-MAX_OUTPUT_CHARS);
}

function actionable(event: SupervisionEvent): boolean {
  return event.type === "confirmation" || event.type === "permission" || event.type === "credential";
}

function normalizePending(runID: string, events: SupervisionEvent[]): SupervisionEvent[] {
  const byID = new Map<string, SupervisionEvent>();
  for (const event of events) {
    if (
      event.runID !== runID ||
      !actionable(event) ||
      event.deliveryStatus === "delivered"
    ) continue;
    byID.set(supervisionEventKey(event.runID, event.sessionID, event.id), { ...event });
  }
  return [...byID.values()]
    .sort((left, right) => left.timestamp.localeCompare(right.timestamp))
    .slice(-MAX_PENDING_EVENTS);
}

export function normalizeState(state: AppState): AppState {
  return {
    ...state,
    agents: state.agents.slice(0, 8).map((agent) => ({
      ...agent,
      revision: Math.max(0, Math.trunc(agent.revision || 0)),
      output: boundedOutput(agent.output || ""),
    })),
    pendingEvents: normalizePending(state.runID, state.pendingEvents || []),
  };
}

export function relayerReducer(state: RelayerUIState, action: RelayerAction): RelayerUIState {
  switch (action.type) {
    case "loaded":
      return {
        ...state,
        connection: "ready",
        fatalError: undefined,
        app: normalizeState(action.state),
        errors: state.app && state.app.runID !== action.state.runID
          ? state.errors.filter((error) => error.runID === "" || error.runID === action.state.runID)
          : state.errors,
        // A new run has new sessions: a roster or hand carried across would
        // describe terminals that no longer exist.
        presence: state.app && state.app.runID !== action.state.runID ? {} : state.presence,
        hand: state.app && state.app.runID !== action.state.runID ? {} : state.hand,
      };
    case "loadFailed":
      return { ...state, connection: "failed", fatalError: action.message };
    case "snapshot": {
      if (!state.app || action.snapshot.runID !== state.app.runID) return state;
      const current = state.app.agents.find(
        (agent) => agent.sessionID === action.snapshot.sessionID,
      );
      if (!current || action.snapshot.revision < current.revision) return state;
      return {
        ...state,
        app: {
          ...state.app,
          agents: state.app.agents.map((agent) =>
            agent.sessionID === action.snapshot.sessionID
              ? {
                  ...agent,
                  ...action.snapshot,
                  inputFrozen: action.snapshot.inputFrozen ?? agent.inputFrozen,
                  output: boundedOutput(action.snapshot.output || ""),
                }
              : agent,
          ),
        },
      };
    }
    case "event": {
      if (
        !state.app ||
        action.event.runID !== state.app.runID ||
        !actionable(action.event)
      ) return state;
      const withoutOccurrence = state.app.pendingEvents.filter(
        (event) =>
          event.id !== action.event.id || event.sessionID !== action.event.sessionID,
      );
      const pendingEvents = normalizePending(state.app.runID, [
        ...withoutOccurrence,
        action.event,
      ]);
      return {
        ...state,
        app: {
          ...state.app,
          pendingEvents,
          agents: state.app.agents.map((agent) =>
            agent.sessionID === action.event.sessionID && action.event.deliveryStatus !== "delivered"
              ? { ...agent, status: "waiting" }
              : agent,
          ),
        },
      };
    }
    case "status": {
      if (!state.app || action.status.runID !== state.app.runID) return state;
      if (action.status.scope === "run") {
        return {
          ...state,
          app: { ...state.app, runStatus: action.status.status as AppState["runStatus"] },
        };
      }
      if (action.status.scope === "audit") {
        return {
          ...state,
          app: {
            ...state.app,
            audit: { ...state.app.audit, status: action.status.status as AppState["audit"]["status"] },
            agents: action.status.status === "failed"
              ? state.app.agents.map((agent) => ({ ...agent, inputFrozen: true }))
              : state.app.agents,
          },
        };
      }
      if (!action.status.sessionID) return state;
      const nextStatus = action.status.status as AppState["agents"][number]["status"];
      // A session that ended can answer none of its prompts. Keeping them
      // offered Review on a dead agent, and answering one revived its card.
      const ended = nextStatus === "exited" || nextStatus === "failed";
      const sessionID = action.status.sessionID;
      return {
        ...state,
        app: {
          ...state.app,
          pendingEvents: ended
            ? state.app.pendingEvents.filter((event) => event.sessionID !== sessionID)
            : state.app.pendingEvents,
          agents: state.app.agents.map((agent) =>
            agent.sessionID === action.status.sessionID
              ? {
                  ...agent,
                  status: nextStatus,
                  running:
                    nextStatus === "running"
                      ? true
                      : nextStatus === "exited" || nextStatus === "failed"
                        ? false
                        : agent.running,
                }
              : agent,
          ),
        },
      };
    }
    case "presence": {
      if (state.app && action.presence.runID !== state.app.runID) return state;
      return {
        ...state,
        presence: withSharedEntry(state.presence, action.presence.sessionID, action.presence),
        app: state.app
          ? {
              ...state.app,
              agents: state.app.agents.map((agent) =>
                agent.sessionID.toLocaleLowerCase() === action.presence.sessionID.toLocaleLowerCase()
                  ? { ...agent, observerCount: action.presence.observerCount }
                  : agent,
              ),
            }
          : state.app,
      };
    }
    case "hand": {
      if (state.app && action.hand.runID !== state.app.runID) return state;
      return {
        ...state,
        hand: withSharedEntry(state.hand, action.hand.sessionID, action.hand),
        app: state.app
          ? {
              ...state.app,
              agents: state.app.agents.map((agent) =>
                agent.sessionID.toLocaleLowerCase() === action.hand.sessionID.toLocaleLowerCase()
                  ? {
                      ...agent,
                      attached: action.hand.state !== "free",
                      holderIdentity: action.hand.holderIdentity,
                    }
                  : agent,
              ),
            }
          : state.app,
      };
    }
    case "error":
      if (state.app && action.error.runID !== state.app.runID) return state;
      return {
        ...state,
        app: state.app &&
          action.error.code === "delivery_uncertain" &&
          action.error.sessionID
          ? {
              ...state.app,
              agents: state.app.agents.map((agent) =>
                agent.sessionID.toLocaleLowerCase() === action.error.sessionID?.toLocaleLowerCase()
                  ? { ...agent, inputFrozen: true }
                  : agent,
              ),
            }
          : state.app,
        errors: [sanitizeErrorEvent(action.error), ...state.errors].slice(0, MAX_ERRORS),
      };
    case "delivery": {
      if (!state.app || action.runID !== state.app.runID) return state;
      const pendingEvents = state.app.pendingEvents
        .map((event) =>
          event.id === action.eventID && event.sessionID === action.sessionID
            ? { ...event, deliveryStatus: action.status }
            : event,
        )
        .filter((event) => event.deliveryStatus !== "delivered");
      return { ...state, app: { ...state.app, pendingEvents } };
    }
    default:
      return state;
  }
}
