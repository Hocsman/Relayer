import { sanitizeErrorEvent } from "../lib/safety";
import { supervisionEventKey } from "../lib/eventKey";
import type {
  AppState,
  SafeErrorEvent,
  SnapshotEvent,
  StatusEvent,
  SupervisionEvent,
} from "../types/relayer";

const MAX_OUTPUT_CHARS = 512 * 1024;
const MAX_PENDING_EVENTS = 64;
const MAX_ERRORS = 40;

export interface RelayerUIState {
  connection: "loading" | "ready" | "failed";
  app: AppState | null;
  errors: SafeErrorEvent[];
  fatalError?: string;
}

export type RelayerAction =
  | { type: "loaded"; state: AppState }
  | { type: "loadFailed"; message: string }
  | { type: "snapshot"; snapshot: SnapshotEvent }
  | { type: "event"; event: SupervisionEvent }
  | { type: "status"; status: StatusEvent }
  | { type: "error"; error: SafeErrorEvent }
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
};

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
          },
        };
      }
      if (!action.status.sessionID) return state;
      return {
        ...state,
        app: {
          ...state.app,
          agents: state.app.agents.map((agent) =>
            agent.sessionID === action.status.sessionID
              ? { ...agent, status: action.status.status as AppState["agents"][number]["status"] }
              : agent,
          ),
        },
      };
    }
    case "error":
      if (state.app && action.error.runID !== state.app.runID) return state;
      return {
        ...state,
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
