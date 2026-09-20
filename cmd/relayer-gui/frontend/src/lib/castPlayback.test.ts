import { describe, expect, it } from "vitest";
import {
  PLAYBACK_SPEEDS,
  advance,
  clampSpeed,
  formatTimecode,
  seekPlan,
} from "./castPlayback";
import type { PlaybackState } from "./castPlayback";
import type { CastFrame } from "./castParser";

function output(time: number, data = "x"): CastFrame {
  return { time, kind: "o", data };
}

function resize(time: number, data: string): CastFrame {
  return { time, kind: "r", data };
}

function playing(overrides: Partial<PlaybackState> = {}): PlaybackState {
  return { playing: true, elapsed: 0, speed: 1, index: 0, ...overrides };
}

const timeline: CastFrame[] = [
  output(0.1, "a"),
  output(0.5, "b"),
  output(0.5, "c"),
  resize(1, "80x24"),
  output(2, "d"),
];

describe("advance", () => {
  it("emits only the frames that fell due and resumes from the next index", () => {
    const first = advance(playing(), timeline, 0.2);
    expect(first.emit.map((frame) => frame.data)).toEqual(["a"]);
    expect(first.state.index).toBe(1);
    expect(first.state.elapsed).toBeCloseTo(0.2);
    expect(first.state.playing).toBe(true);

    const second = advance(first.state, timeline, 0.2);
    expect(second.emit).toEqual([]);
    expect(second.state.index).toBe(1);

    const third = advance(second.state, timeline, 0.2);
    expect(third.emit.map((frame) => frame.data)).toEqual(["b", "c"]);
    expect(third.state.index).toBe(3);
  });

  it("emits a frame whose timestamp equals the clock exactly once", () => {
    const first = advance(playing(), [output(0.5, "only")], 0.5);
    expect(first.emit.map((frame) => frame.data)).toEqual(["only"]);

    const second = advance({ ...first.state, playing: true }, [output(0.5, "only")], 0.5);
    expect(second.emit).toEqual([]);
  });

  it("scales wall time by the playback speed", () => {
    const table: { speed: number; delta: number; expected: string[] }[] = [
      { speed: 0.5, delta: 0.4, expected: ["a"] },
      { speed: 1, delta: 0.4, expected: ["a"] },
      { speed: 2, delta: 0.4, expected: ["a", "b", "c"] },
      { speed: 8, delta: 0.4, expected: ["a", "b", "c", "80x24", "d"] },
    ];
    for (const row of table) {
      const result = advance(playing({ speed: row.speed }), timeline, row.delta);
      expect(result.emit.map((frame) => frame.data)).toEqual(row.expected);
    }
  });

  it("stops at the end and pins the clock to the recording duration", () => {
    const result = advance(playing(), timeline, 60);
    expect(result.emit).toHaveLength(timeline.length);
    expect(result.state.playing).toBe(false);
    expect(result.state.elapsed).toBe(2);
    expect(result.state.index).toBe(timeline.length);
  });

  it("stops immediately on an empty recording", () => {
    const result = advance(playing(), [], 0.5);
    expect(result.emit).toEqual([]);
    expect(result.state.playing).toBe(false);
    expect(result.state.elapsed).toBe(0);
  });

  it("holds the timeline still when paused or when the clock misbehaves", () => {
    const paused = playing({ playing: false });
    expect(advance(paused, timeline, 1)).toEqual({ state: paused, emit: [] });

    const live = playing();
    for (const delta of [0, -1, Number.NaN, Number.POSITIVE_INFINITY]) {
      expect(advance(live, timeline, delta)).toEqual({ state: live, emit: [] });
    }
  });

  it("recovers from a corrupted elapsed time or index without throwing", () => {
    const result = advance(
      playing({ elapsed: Number.NaN, index: -4, speed: Number.NaN }),
      timeline,
      0.2,
    );
    expect(result.emit.map((frame) => frame.data)).toEqual(["a"]);
    expect(result.state.speed).toBe(1);
    expect(result.state.index).toBe(1);
  });
});

describe("seekPlan", () => {
  it("rebuilds the whole prefix when it fits in the budget", () => {
    const plan = seekPlan(timeline, 1, 100);
    expect(plan.emit.map((frame) => frame.data)).toEqual(["a", "b", "c", "80x24"]);
    expect(plan.index).toBe(4);
    expect(plan.truncated).toBe(false);
  });

  it("treats a target at or before zero as the start of the recording", () => {
    for (const target of [0, -5, Number.NaN]) {
      const plan = seekPlan(timeline, target, 100);
      expect(plan.emit).toEqual([]);
      expect(plan.index).toBe(0);
      expect(plan.truncated).toBe(false);
    }
  });

  it("caps the rebuild at the budget and reports the truncation", () => {
    const long: CastFrame[] = Array.from({ length: 500 }, (_, index) =>
      output(index, `f${index}`),
    );
    const plan = seekPlan(long, 499, 10);

    expect(plan.emit).toHaveLength(10);
    expect(plan.truncated).toBe(true);
    // The index still points past the whole prefix, so resuming playback
    // continues from the sought position and not from the truncated window.
    expect(plan.index).toBe(500);
    expect(plan.emit[plan.emit.length - 1].data).toBe("f499");
  });

  it("carries the last resize preceding the truncated window", () => {
    const long: CastFrame[] = [
      resize(0, "200x50"),
      ...Array.from({ length: 100 }, (_, index) => output(index + 1, `f${index}`)),
    ];
    const plan = seekPlan(long, 500, 4);

    expect(plan.emit).toHaveLength(4);
    expect(plan.emit[0]).toEqual(resize(0, "200x50"));
    expect(plan.emit.slice(1).map((frame) => frame.data)).toEqual(["f97", "f98", "f99"]);
    expect(plan.truncated).toBe(true);
  });

  it("emits nothing for a budget that cannot hold a single frame", () => {
    for (const budget of [0, -1, Number.NaN]) {
      const plan = seekPlan(timeline, 2, budget);
      expect(plan.emit).toEqual([]);
      expect(plan.index).toBe(timeline.length);
      expect(plan.truncated).toBe(true);
    }
  });

  it("reports no truncation when the prefix is empty whatever the budget", () => {
    const plan = seekPlan(timeline, 0, 0);
    expect(plan.emit).toEqual([]);
    expect(plan.truncated).toBe(false);
  });

  // The budget caps what is written to the screen; it has to cap what is built
  // to get there too. Locating the prefix by collecting it allocates an array
  // the size of the recording for every nudge of the scrubber, only to discard
  // all but the last `budget` entries. The allocation itself is not observable
  // from a test, so what is pinned here is that the cheaper scan still answers
  // exactly as the collecting one did on an input large enough to matter.
  it("keeps the plan bounded and exact on a recording of real length", () => {
    const long: CastFrame[] = [];
    for (let index = 0; index < 50_000; index += 1) {
      if (index === 20_000) {
        long.push(resize(index / 1000, "200x50"));
        continue;
      }
      long.push(output(index / 1000, `f${index}`));
    }

    const plan = seekPlan(long, 49, 10);

    expect(plan.index).toBe(49_001);
    expect(plan.emit).toHaveLength(10);
    // The last resize before the window is carried in ahead of the tail, so the
    // rebuilt screen has the geometry the session was running at.
    expect(plan.emit[0]).toEqual(resize(20, "200x50"));
    expect(plan.emit.slice(1).map((frame) => frame.data)).toEqual([
      "f48992",
      "f48993",
      "f48994",
      "f48995",
      "f48996",
      "f48997",
      "f48998",
      "f48999",
      "f49000",
    ]);
    expect(plan.truncated).toBe(true);
  });
});

describe("formatTimecode", () => {
  it("renders positions on the timeline", () => {
    const table: [number, string][] = [
      [0, "0:00"],
      [0.4, "0:00"],
      [0.999, "0:00"],
      [1, "0:01"],
      [9.7, "0:09"],
      [59, "0:59"],
      [60, "1:00"],
      [67, "1:07"],
      [599, "9:59"],
      [3599, "59:59"],
      [3600, "1:00:00"],
      [32671, "9:04:31"],
      [-12, "0:00"],
      [Number.NaN, "0:00"],
      [Number.POSITIVE_INFINITY, "0:00"],
    ];
    for (const [seconds, expected] of table) {
      expect(formatTimecode(seconds)).toBe(expected);
    }
  });
});

describe("clampSpeed", () => {
  it("returns every supported speed unchanged", () => {
    for (const speed of PLAYBACK_SPEEDS) {
      expect(clampSpeed(speed)).toBe(speed);
    }
  });

  it("folds anything else onto the supported set", () => {
    const table: [number, number][] = [
      [0, 0.5],
      [-3, 0.5],
      [0.1, 0.5],
      [0.9, 1],
      [1.4, 1],
      [1.6, 2],
      [3, 2],
      [5, 4],
      [7, 8],
      [100, 8],
      [Number.NaN, 1],
      [Number.POSITIVE_INFINITY, 8],
      [Number.NEGATIVE_INFINITY, 0.5],
    ];
    for (const [input, expected] of table) {
      expect(clampSpeed(input)).toBe(expected);
    }
  });
});
