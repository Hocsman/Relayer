// Timeline arithmetic for replaying a recorded session.
//
// castParser reads the file; this decides what the screen should show at a
// given moment. It is kept free of xterm and of React for one reason: the two
// ways a replay goes wrong — a frame played twice, or a seek that walks the
// whole recording and freezes the tab — are both arithmetic, and arithmetic can
// be tested. The player component is then a thin shell that writes whatever
// these functions hand it.
//
// Nothing here throws. A recording is the artefact of a run that may itself
// have crashed, so garbage timestamps and a nonsense speed are expected input,
// not exceptional ones.

import { framesUpTo, resizeFromFrame, totalDuration } from "./castParser";
import type { CastFrame } from "./castParser";

export interface PlaybackState {
  playing: boolean;
  elapsed: number;
  speed: number;
  index: number;
}

export const PLAYBACK_SPEEDS: readonly number[] = [0.5, 1, 2, 4, 8];

const DEFAULT_SPEED = 1;

// clampSpeed folds any number onto the supported set.
//
// The speed reaches this module from a control the operator clicks and from
// state that survives a reload, so it can be anything. A value between two
// supported speeds snaps to the nearer one, and a tie snaps to the slower: the
// operator watching an audit replay would rather fall behind than skip. Only a
// value that is not a number at all falls back to normal speed; an infinity is
// an intent, however clumsily expressed, and clamps to the nearest end.
export function clampSpeed(speed: number): number {
  if (typeof speed !== "number" || Number.isNaN(speed)) return DEFAULT_SPEED;
  const slowest = PLAYBACK_SPEEDS[0];
  const fastest = PLAYBACK_SPEEDS[PLAYBACK_SPEEDS.length - 1];
  if (speed <= slowest) return slowest;
  if (speed >= fastest) return fastest;

  let chosen = slowest;
  let bestDistance = Number.POSITIVE_INFINITY;
  for (const candidate of PLAYBACK_SPEEDS) {
    const distance = Math.abs(candidate - speed);
    if (distance < bestDistance) {
      bestDistance = distance;
      chosen = candidate;
    }
  }
  return chosen;
}

// advance is one animation tick.
//
// It moves the clock by deltaSeconds of wall time scaled by the playback speed
// and returns the frames that fell due, resuming the scan from the index the
// previous tick stopped at. Forward only: a frame is emitted exactly once, and
// a frame whose timestamp equals the new elapsed time counts as due now rather
// than next tick, so two frames sharing a timestamp still play together.
//
// Playback stops of its own accord once the last frame has been emitted, and
// the clock is pinned to the recording's duration so the scrubber rests at the
// end instead of drifting past it.
export function advance(
  state: PlaybackState,
  frames: CastFrame[],
  deltaSeconds: number,
): { state: PlaybackState; emit: CastFrame[] } {
  if (!state.playing) {
    return { state, emit: [] };
  }
  if (typeof deltaSeconds !== "number" || !Number.isFinite(deltaSeconds) || deltaSeconds <= 0) {
    // A clock that jumped backwards, or a tab that was just restored. Holding
    // the timeline still is the only safe reading.
    return { state, emit: [] };
  }

  const speed = clampSpeed(state.speed);
  const from = Number.isFinite(state.elapsed) && state.elapsed > 0 ? state.elapsed : 0;
  const index = Number.isFinite(state.index) && state.index > 0 ? Math.floor(state.index) : 0;
  const elapsed = from + deltaSeconds * speed;

  const due = framesUpTo(frames, elapsed, index);
  const finished = due.nextIndex >= frames.length;
  const duration = totalDuration(frames);

  return {
    state: {
      playing: !finished,
      elapsed: finished ? Math.max(duration, 0) : elapsed,
      speed,
      index: due.nextIndex,
    },
    emit: due.frames,
  };
}

// seekPlan rebuilds the screen at targetSeconds from the start of the
// recording.
//
// There is no cheaper way: restoring a terminal from a snapshot needs xterm's
// SerializeAddon, which is not a dependency of this project and is not worth
// becoming one for a replay view. So a seek replays the prefix with no delay —
// and that is exactly the operation that can hang a tab, because a long session
// is hundreds of thousands of frames.
//
// The cap is therefore not a safety net but the contract: at most `budget`
// frames come back, and `truncated` says the screen was rebuilt from the tail
// of the prefix rather than all of it. The tail is the right part to keep, since
// the earlier output has scrolled away anyway — except for geometry, so the last
// resize preceding the window is carried in ahead of it.
//
// The prefix is located by a scan that collects nothing. Building the whole
// prefix as an array first and then keeping its last `budget` entries honours
// the cap on what is written to the screen but not on what is allocated to get
// there: on a long recording that is a several-hundred-thousand-element array
// per seek, on the main thread, while the operator drags the scrubber.
export function seekPlan(
  frames: CastFrame[],
  targetSeconds: number,
  budget: number,
): { emit: CastFrame[]; index: number; truncated: boolean } {
  const target =
    typeof targetSeconds === "number" && Number.isFinite(targetSeconds) && targetSeconds > 0
      ? targetSeconds
      : 0;
  const cap =
    typeof budget === "number" && Number.isFinite(budget) && budget > 0 ? Math.floor(budget) : 0;

  const index = prefixEnd(frames, target);

  if (cap === 0) {
    return { emit: [], index, truncated: index > 0 };
  }
  if (index <= cap) {
    return { emit: frames.slice(0, index), index, truncated: false };
  }

  const start = index - cap;
  const window = frames.slice(start, index);
  const carried = cap >= 2 ? lastResizeBefore(frames, start) : null;
  const emit = carried ? [carried, ...window.slice(1)] : window;
  return { emit, index, truncated: true };
}

// prefixEnd is framesUpTo's index arithmetic without its array: the position
// one past the last frame due at `target`, walking from the start because a
// seek may go backwards. Frame times are only expected to rise, never
// guaranteed to — a recording is the artefact of a run that may have crashed —
// so this stays a linear scan rather than a binary search.
function prefixEnd(frames: CastFrame[], target: number): number {
  let index = 0;
  while (index < frames.length && frames[index].time <= target) {
    index += 1;
  }
  return index;
}

function lastResizeBefore(frames: CastFrame[], limit: number): CastFrame | null {
  for (let index = limit - 1; index >= 0; index -= 1) {
    const frame = frames[index];
    if (frame.kind === "r" && resizeFromFrame(frame)) {
      return frame;
    }
  }
  return null;
}

// formatTimecode renders a position on the timeline, not a duration in prose.
// The hour field appears only when there is one, so a two-minute session reads
// "1:07" and an overnight run still reads correctly at "9:04:31".
export function formatTimecode(seconds: number): string {
  const total =
    typeof seconds === "number" && Number.isFinite(seconds) && seconds > 0
      ? Math.floor(seconds)
      : 0;
  const hours = Math.floor(total / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  const remainder = total % 60;
  const pad = (value: number) => (value < 10 ? `0${value}` : `${value}`);
  if (hours > 0) {
    return `${hours}:${pad(minutes)}:${pad(remainder)}`;
  }
  return `${minutes}:${pad(remainder)}`;
}
