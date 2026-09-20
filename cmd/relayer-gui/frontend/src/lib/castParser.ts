// asciicast v2 reader for deferred replay of a recorded session.
//
// The format is one JSON header line followed by one JSON array per event, so a
// recording streams and a truncated file is still partially readable. That
// property is the point: a run that crashed mid-session is exactly the one an
// operator wants to replay, so nothing here throws — a line that does not parse
// is counted and skipped, and the frames around it still play.
//
// This module stays free of xterm and of the DOM. Replay drives a terminal, but
// the timeline arithmetic is decided here, where it can be tested.

export interface CastHeader {
  version: number;
  width: number;
  height: number;
  timestamp?: number;
  title?: string;
}

export type CastFrameKind = "o" | "i" | "r" | "m";

export interface CastFrame {
  time: number;
  kind: CastFrameKind;
  data: string;
}

export interface ParsedCast {
  header: CastHeader;
  frames: CastFrame[];
  errors: string[];
}

const SUPPORTED_VERSION = 2;

// Errors name a line, never quote one. A malformed recording is still terminal
// output, and this list reaches the operator's screen.
const MAX_PARSE_ERRORS = 32;

const resizePattern = /^(\d+)x(\d+)$/;

const frameKinds: readonly string[] = ["o", "i", "r", "m"];

function emptyHeader(): CastHeader {
  return { version: 0, width: 0, height: 0 };
}

function finiteNumber(value: unknown): number {
  return typeof value === "number" && Number.isFinite(value) ? value : 0;
}

function parseHeader(line: string): { header: CastHeader; error: string } {
  let raw: unknown;
  try {
    raw = JSON.parse(line);
  } catch {
    return { header: emptyHeader(), error: "header is not valid json" };
  }
  if (typeof raw !== "object" || raw === null || Array.isArray(raw)) {
    return { header: emptyHeader(), error: "header is not an object" };
  }
  const record = raw as Record<string, unknown>;
  const version = finiteNumber(record.version);
  if (version !== SUPPORTED_VERSION) {
    return { header: emptyHeader(), error: "header version is not asciicast v2" };
  }
  const header: CastHeader = {
    version,
    width: finiteNumber(record.width),
    height: finiteNumber(record.height),
  };
  if (typeof record.timestamp === "number" && Number.isFinite(record.timestamp)) {
    header.timestamp = record.timestamp;
  }
  if (typeof record.title === "string") {
    header.title = record.title;
  }
  return { header, error: "" };
}

export function parseCastFrame(line: string): CastFrame | null {
  if (!line.trim()) return null;
  let raw: unknown;
  try {
    raw = JSON.parse(line);
  } catch {
    return null;
  }
  if (!Array.isArray(raw) || raw.length < 3) return null;
  const [time, kind, data] = raw as unknown[];
  if (typeof time !== "number" || !Number.isFinite(time)) return null;
  if (typeof kind !== "string" || !frameKinds.includes(kind)) return null;
  if (typeof data !== "string") return null;
  return { time, kind: kind as CastFrameKind, data };
}

export function parseCast(text: string): ParsedCast {
  const lines = text.split(/\r?\n/);
  const frames: CastFrame[] = [];
  const errors: string[] = [];
  let truncated = false;

  const note = (message: string) => {
    if (errors.length >= MAX_PARSE_ERRORS) {
      truncated = true;
      return;
    }
    errors.push(message);
  };

  let header: CastHeader | null = null;
  for (let index = 0; index < lines.length; index += 1) {
    const line = lines[index];
    if (!line.trim()) continue;

    if (!header) {
      const parsed = parseHeader(line);
      if (parsed.error) {
        return { header: parsed.header, frames, errors: [parsed.error] };
      }
      header = parsed.header;
      continue;
    }

    const frame = parseCastFrame(line);
    if (!frame) {
      note(`line ${index + 1} is not a valid frame`);
      continue;
    }
    frames.push(frame);
  }

  if (truncated) {
    errors.push("further malformed lines were skipped");
  }
  if (!header) {
    return { header: emptyHeader(), frames, errors: ["recording has no header"] };
  }
  return { header, frames, errors };
}

// framesUpTo returns the frames due at elapsedSeconds, resuming from fromIndex.
//
// This runs on every animation tick of the player, so it walks forward from
// where the last tick stopped and touches only the frames it hands back: the
// cost is the number of frames actually due, not the length of the recording.
export function framesUpTo(
  frames: CastFrame[],
  elapsedSeconds: number,
  fromIndex: number,
): { frames: CastFrame[]; nextIndex: number } {
  let index = fromIndex > 0 ? fromIndex : 0;
  const due: CastFrame[] = [];
  while (index < frames.length && frames[index].time <= elapsedSeconds) {
    due.push(frames[index]);
    index += 1;
  }
  return { frames: due, nextIndex: index };
}

export function totalDuration(frames: CastFrame[]): number {
  if (frames.length === 0) return 0;
  const last = frames[frames.length - 1].time;
  return last > 0 ? last : 0;
}

export function resizeFromFrame(frame: CastFrame): { columns: number; rows: number } | null {
  if (frame.kind !== "r") return null;
  const match = resizePattern.exec(frame.data);
  if (!match) return null;
  const columns = Number(match[1]);
  const rows = Number(match[2]);
  if (columns <= 0 || rows <= 0) return null;
  return { columns, rows };
}
