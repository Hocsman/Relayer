import { describe, expect, it } from "vitest";
import {
  framesUpTo,
  parseCast,
  parseCastFrame,
  resizeFromFrame,
  totalDuration,
} from "./castParser";
import type { CastFrame } from "./castParser";

function frame(time: number, data = "x"): CastFrame {
  return { time, kind: "o", data };
}

const recording = [
  '{"version": 2, "width": 120, "height": 30, "timestamp": 1700000000, "title": "deploy"}',
  '[0.1, "o", "$ relayer run\\r\\n"]',
  '[0.5, "r", "80x24"]',
  '[1.25, "i", "y\\r"]',
  '[2, "m", "paused"]',
].join("\n");

describe("parseCast", () => {
  it("reads a header and every frame kind", () => {
    const cast = parseCast(recording);
    expect(cast.errors).toEqual([]);
    expect(cast.header).toEqual({
      version: 2,
      width: 120,
      height: 30,
      timestamp: 1700000000,
      title: "deploy",
    });
    expect(cast.frames).toEqual([
      { time: 0.1, kind: "o", data: "$ relayer run\r\n" },
      { time: 0.5, kind: "r", data: "80x24" },
      { time: 1.25, kind: "i", data: "y\r" },
      { time: 2, kind: "m", data: "paused" },
    ]);
  });

  it("tolerates blank lines and a trailing newline", () => {
    const padded = `\n${recording.replace("\n[0.5", "\n\n  \n[0.5")}\n\n`;
    expect(parseCast(padded).frames).toEqual(parseCast(recording).frames);
    expect(parseCast(padded).errors).toEqual([]);
  });

  it.each([
    ['{"version": 1, "width": 80, "height": 24}', "header version is not asciicast v2"],
    ['{"version": 3, "width": 80, "height": 24}', "header version is not asciicast v2"],
    ['{"width": 80, "height": 24}', "header version is not asciicast v2"],
    ["{not json", "header is not valid json"],
    ["[2, 80, 24]", "header is not an object"],
  ])("rejects a header that is %s", (header, message) => {
    const cast = parseCast(`${header}\n[0.1, "o", "ignored"]\n`);
    expect(cast.errors).toEqual([message]);
    expect(cast.frames).toEqual([]);
    expect(cast.header.version).toBe(0);
  });

  it.each(["", "\n", "   \n\n"])("reports an empty recording (%j)", (text) => {
    const cast = parseCast(text);
    expect(cast.errors).toEqual(["recording has no header"]);
    expect(cast.frames).toEqual([]);
  });

  it("collects malformed frames instead of throwing", () => {
    const cast = parseCast(
      [
        '{"version": 2, "width": 80, "height": 24}',
        '[0.1, "o", "kept"]',
        "[0.2, broken",
        '[0.3, "z", "unknown kind"]',
        '["late", "o", "time is not a number"]',
        '[0.4, "o", 42]',
        '[0.5, "o"]',
        '{"version": 2}',
        '[0.6, "o", "kept too"]',
      ].join("\n"),
    );
    expect(cast.frames).toEqual([
      { time: 0.1, kind: "o", data: "kept" },
      { time: 0.6, kind: "o", data: "kept too" },
    ]);
    expect(cast.errors).toEqual([
      "line 3 is not a valid frame",
      "line 4 is not a valid frame",
      "line 5 is not a valid frame",
      "line 6 is not a valid frame",
      "line 7 is not a valid frame",
      "line 8 is not a valid frame",
    ]);
  });

  it("bounds the error list on a file that is not a recording at all", () => {
    const junk = Array.from({ length: 200 }, () => "garbage").join("\n");
    const cast = parseCast(`{"version": 2, "width": 80, "height": 24}\n${junk}`);
    expect(cast.errors).toHaveLength(33);
    expect(cast.errors[32]).toBe("further malformed lines were skipped");
  });

  it("never quotes the offending line back to the operator", () => {
    const cast = parseCast('{"version": 2, "width": 80, "height": 24}\napi_key=sk-live-4b91ce');
    expect(cast.errors.join("\n")).not.toContain("sk-live-4b91ce");
  });
});

describe("parseCastFrame", () => {
  it.each([
    ["", "a blank line"],
    ["   ", "whitespace"],
    ["[", "truncated json"],
    ['{"time": 1}', "an object"],
    ['[0.1, "o"]', "too few elements"],
    ['[0.1, "q", "data"]', "an unknown kind"],
    ['["0.1", "o", "data"]', "a string time"],
    ['[0.1, 0, "data"]', "a numeric kind"],
    ['[0.1, "o", null]', "null data"],
  ])("returns null for %s (%s)", (line) => {
    expect(parseCastFrame(line)).toBeNull();
  });

  it("reads a well formed frame", () => {
    expect(parseCastFrame('[3.5, "o", "hello"]')).toEqual({ time: 3.5, kind: "o", data: "hello" });
  });
});

// framesUpTo is called once per animation tick, so its contract is the whole
// playback clock: every frame due exactly once, in order, never re-scanned.
describe("framesUpTo", () => {
  const timeline = [frame(0), frame(1), frame(1), frame(2.5), frame(4)];

  it("includes a frame whose timestamp equals the elapsed time", () => {
    const due = framesUpTo(timeline, 1, 0);
    expect(due.frames.map((entry) => entry.time)).toEqual([0, 1, 1]);
    expect(due.nextIndex).toBe(3);
  });

  it("resumes from a nonzero index without replaying earlier frames", () => {
    const first = framesUpTo(timeline, 1, 0);
    const second = framesUpTo(timeline, 4, first.nextIndex);
    expect(second.frames.map((entry) => entry.time)).toEqual([2.5, 4]);
    expect(second.nextIndex).toBe(timeline.length);
  });

  it("returns nothing when no frame is due yet", () => {
    const due = framesUpTo(timeline, -0.5, 0);
    expect(due.frames).toEqual([]);
    expect(due.nextIndex).toBe(0);
  });

  it("returns nothing once the recording is exhausted", () => {
    const due = framesUpTo(timeline, 999, timeline.length);
    expect(due.frames).toEqual([]);
    expect(due.nextIndex).toBe(timeline.length);
  });

  it("returns nothing for an empty recording", () => {
    expect(framesUpTo([], 10, 0)).toEqual({ frames: [], nextIndex: 0 });
  });

  it("hands every frame back exactly once across successive ticks", () => {
    const seen: number[] = [];
    let index = 0;
    for (const elapsed of [0, 0.9, 1, 2, 2.5, 3, 4]) {
      const due = framesUpTo(timeline, elapsed, index);
      seen.push(...due.frames.map((entry) => entry.time));
      index = due.nextIndex;
    }
    expect(seen).toEqual(timeline.map((entry) => entry.time));
  });
});

describe("totalDuration", () => {
  it("is zero for an empty recording", () => {
    expect(totalDuration([])).toBe(0);
  });

  it("is the timestamp of the last frame", () => {
    expect(totalDuration([frame(0), frame(1), frame(7.5)])).toBe(7.5);
  });

  it("never reports a negative duration", () => {
    expect(totalDuration([frame(-3)])).toBe(0);
  });
});

describe("resizeFromFrame", () => {
  it("reads the new geometry from a resize frame", () => {
    expect(resizeFromFrame({ time: 1, kind: "r", data: "120x30" })).toEqual({
      columns: 120,
      rows: 30,
    });
  });

  it.each([
    ["another frame kind", { time: 1, kind: "o", data: "80x24" }],
    ["a missing dimension", { time: 1, kind: "r", data: "80" }],
    ["an empty dimension", { time: 1, kind: "r", data: "80x" }],
    ["a third dimension", { time: 1, kind: "r", data: "80x24x2" }],
    ["a zero column count", { time: 1, kind: "r", data: "0x24" }],
    ["a zero row count", { time: 1, kind: "r", data: "80x0" }],
    ["a negative dimension", { time: 1, kind: "r", data: "-80x24" }],
    ["padded dimensions", { time: 1, kind: "r", data: "80 x 24" }],
    ["no data", { time: 1, kind: "r", data: "" }],
  ] as [string, CastFrame][])("returns null for %s", (_label, candidate) => {
    expect(resizeFromFrame(candidate)).toBeNull();
  });
});
