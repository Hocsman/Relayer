import { describe, expect, it } from "vitest";
import {
  commandLines,
  isConfidential,
  promptContextLines,
  redactForDisplay,
  safeError,
  safeEventSummary,
  stripTerminalEscapes,
} from "./safety";
import type { SupervisionEvent } from "../types/relayer";

function event(overrides: Partial<SupervisionEvent> = {}): SupervisionEvent {
  return {
    runID: "run-1",
    id: "event-1",
    sessionID: "agent-a",
    agentID: "agent-a",
    adapter: "generic",
    type: "confirmation",
    summary: "Overwrite file? [Y/n]",
    sensitive: false,
    risk: "unknown",
    timestamp: "2026-01-01T00:00:00Z",
    evaluation: {
      action: "ask",
      proposedAction: "ask",
      reason: "default_action",
      automatic: false,
      dryRun: false,
    },
    deliveryStatus: "pending",
    ...overrides,
  };
}

describe("frontend redaction", () => {
  it.each([
    ["token=super-secret", "super-secret"],
    ["Authorization: Bearer abc.def.ghi", "abc.def.ghi"],
    ["https://alice:secret@example.test/private", "alice:secret"],
    ["eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.ABCDEFGHIJKLMNOP", "eyJhbGci"],
  ])("removes credentials from %s", (input, leakedValue) => {
    expect(redactForDisplay(input)).not.toContain(leakedValue);
  });

  it("never renders a sensitive summary", () => {
    const sensitive = event({
      type: "credential",
      sensitive: true,
      summary: "password=must-not-appear",
    });
    expect(safeEventSummary(sensitive)).toBe("Confidential input required");
    expect(safeEventSummary(sensitive)).not.toContain("must-not-appear");
  });

  it("masks a credential prompt even if the sensitive flag is lost", () => {
    const credential = event({ type: "credential", summary: "Enter your password" });
    expect(isConfidential(credential)).toBe(true);
    expect(safeEventSummary(credential)).toBe("Confidential input required");
  });

  // High risk is not a secret: the prompt keeps an honest label and shows its
  // command, redacted and bounded, instead of a masked placeholder.
  it("shows a high-risk prompt's command instead of masking it", () => {
    const risky = event({ risk: "high", summary: "Run command: npm test -- password=hunter2" });
    expect(isConfidential(risky)).toBe(false);
    const shown = safeEventSummary(risky);
    expect(shown).toContain("npm test");
    expect(shown).not.toContain("hunter2");
    expect(shown).not.toBe("Confidential input required");
  });
});

// The decision modal shows the tail of the pane so a decision is not made on a
// one-line summary. The bytes are already on the agent card, but this surface
// applies the same redaction as every other safe display.
describe("promptContextLines", () => {
  it("keeps only the last lines and drops trailing blanks", () => {
    const output = Array.from({ length: 40 }, (_, index) => `line ${index}`).join("\n") + "\n\n\n";
    const lines = promptContextLines(output, 5);
    expect(lines).toEqual(["line 35", "line 36", "line 37", "line 38", "line 39"]);
  });

  it("redacts a credential that scrolled into the tail", () => {
    const lines = promptContextLines("exporting\napi_key=sk-live-4b91ce\nready");
    expect(lines.join("\n")).not.toContain("sk-live-4b91ce");
    expect(lines.join("\n")).toContain("[REDACTED]");
  });

  it("truncates a single enormous line instead of widening the dialog", () => {
    const lines = promptContextLines("x".repeat(5000));
    expect(lines).toHaveLength(1);
    expect(lines[0].length).toBeLessThanOrEqual(201);
  });

  it("returns nothing for a pane that produced no output", () => {
    expect(promptContextLines("")).toEqual([]);
    expect(promptContextLines("\n\n  \n")).toEqual([]);
  });
});

// Go error strings are fragments by convention — lowercase, unpunctuated — and
// the engine's are written that way so ST1005 applies to the whole module. The
// interface is what turns one into a sentence, so that the operator never reads
// "the Relayer engine is stopped" as a paragraph.
describe("safeError sentence casing", () => {
  it("casts an engine fragment into a sentence", () => {
    expect(safeError(new Error("the Relayer engine is stopped")))
      .toBe("The Relayer engine is stopped.");
    expect(safeError(new Error("invalid terminal dimensions")))
      .toBe("Invalid terminal dimensions.");
  });

  it("leaves a message that is already a sentence alone", () => {
    expect(safeError(new Error("Configuration has changed. Reload first.")))
      .toBe("Configuration has changed. Reload first.");
    expect(safeError(new Error("Did the run stop?"))).toBe("Did the run stop?");
  });

  it("still redacts before casing, so a secret cannot be capitalised into view", () => {
    const cast = safeError(new Error("api_key=sk-live-4b91ce rejected"));
    expect(cast).not.toContain("sk-live-4b91ce");
    expect(cast).toContain("[REDACTED]");
    expect(cast.endsWith(".")).toBe(true);
  });

  it("uses the fallback when there is no message, without touching it", () => {
    expect(safeError(undefined, "The run change failed.")).toBe("The run change failed.");
    expect(safeError(new Error("   "), "The run change failed.")).toBe("The run change failed.");
  });
});

// The tail of the pane is the terminal's own bytes. Shown raw, the escape
// sequences of a Claude Code screen read as "[K" and "[?25h", and one between
// the halves of a secret kept it out of every redaction.
describe("stripTerminalEscapes", () => {
  const ESC = "\u001b";
  // The string terminator: ESC and a backslash.
  const ST = ESC + String.fromCharCode(92);

  it.each([
    ["colours", `${ESC}[38;5;246mrm -f x${ESC}[39m`, "rm -f x"],
    ["erase and show cursor", `line${ESC}[K${ESC}[?25h`, "line"],
    ["an OSC title ended by BEL", `${ESC}]0;C:/Users/someone/agent.exe\u0007ready`, "ready"],
    ["an OSC hyperlink ended by ST", `${ESC}]8;;https://example.test${ST}link${ESC}]8;;${ST}`, "link"],
    ["a charset designation", `${ESC}(Bplain${ESC}(0`, "plain"],
    ["save and restore cursor", `${ESC}7text${ESC}8`, "text"],
    ["an ESC that ends a line", `a${ESC}\nb`, "a\nb"],
    ["an ESC that ends the text", `a${ESC}`, "a"],
    ["an ESC and the one letter a terminal reads with it", `a${ESC}bc`, "ac"],
    ["a C1 control sequence", "a\u009b31mb", "ab"],
    ["a C1 string introducer, ended by BEL", "a\u009d0;title\u0007b", "ab"],
    ["a C1 string introducer, ended by the C1 terminator", "a\u009d0;title\u009cb", "ab"],
    ["a C1 control that is not a sequence", "a\u0085b\u0080c", "abc"],
    ["a cursor-forward of zero, which a terminal reads as one", `a${ESC}[0Cb`, "a b"],
    ["control characters but not a tab or a line break", "a\u0000b\u0007c\td\ne", "abc\td\ne"],
  ])("removes %s", (_name, input, expected) => {
    expect(stripTerminalEscapes(input)).toBe(expected);
  });

  it("keeps the words of a screen drawn with cursor-forward", () => {
    expect(stripTerminalEscapes(`echo${ESC}[1CPROBE_BASH_OK${ESC}[1C>${ESC}[1Cbash_probe.txt`)).toBe(
      "echo PROBE_BASH_OK > bash_probe.txt",
    );
    expect(stripTerminalEscapes(`a${ESC}[3Cb${ESC}[Cc`)).toBe("a   b c");
  });

  it("bounds a cursor-forward so it cannot widen the dialog", () => {
    expect(stripTerminalEscapes(`a${ESC}[99999Cb`)).toBe(`a${" ".repeat(200)}b`);
  });

  it("does not let an unterminated string swallow the rest of the screen", () => {
    const tail = "x".repeat(5000);
    expect(stripTerminalEscapes(`${ESC}]0;title-without-end${tail}`).length).toBeGreaterThan(2500);
  });

  it("leaves ordinary text alone", () => {
    const text = "Do you want to proceed?\n❯ 1. Yes\n  2. No — [y/n] ~/work $ \u00e9\u4e2d";
    expect(stripTerminalEscapes(text)).toBe(text);
  });
});

describe("promptContextLines with a terminal's own bytes", () => {
  const ESC = "\u001b";

  it("shows the text an agent wrote, not its escape sequences", () => {
    const output = `Run${ESC}[1Cshell${ESC}[1Ccommand\n${ESC}[38;5;246mecho${ESC}[1Chi${ESC}[39m${ESC}[K\n${ESC}[?25h`;
    expect(promptContextLines(output)).toEqual(["Run shell command", "echo hi"]);
  });

  it("redacts a secret that an escape sequence split in two", () => {
    const lines = promptContextLines(`api_key${ESC}[0m=sk-live-4b91ce and ${ESC}[1mAuthorization: Bearer${ESC}[0m abc.def.ghi`);
    const text = lines.join("\n");
    expect(text).not.toContain("sk-live-4b91ce");
    expect(text).not.toContain("abc.def.ghi");
    expect(text).toContain("[REDACTED]");
  });

  // A token redacted whole is the same text whether the cut falls before or after
  // it; one cut in half first is a prefix no pattern recognises. A JWT is the
  // case that tells the two orders apart: its first 50 characters are not a JWT.
  it("redacts before it cuts, so the cut cannot leave half a secret", () => {
    const jwt = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV";
    const lines = promptContextLines(`${"x".repeat(150)} ${jwt}`);
    expect(lines.join("\n")).not.toContain("eyJhbGciOiJIUzI1NiJ9");
    expect(lines.join("\n")).toContain("[REDACTED]");
  });

  // The URL pattern tries every start of a run of scheme characters, so a long
  // run of them was quadratic: 64 KiB took two seconds and 256 KiB over thirty,
  // on the main thread, on every render, from text an agent chose to print.
  it("does not stall on a long unbroken line", () => {
    const started = performance.now();
    const lines = promptContextLines(`${"a".repeat(256 * 1024)}\n${"0123456789abcdef".repeat(16 * 1024)}`);
    expect(performance.now() - started).toBeLessThan(1000);
    expect(lines).toHaveLength(2);
    expect(lines[0].length).toBeLessThanOrEqual(201);
  });

  it("cuts a line at a character, not through an emoji", () => {
    const [line] = promptContextLines("\u{1F600}".repeat(150));
    expect(Array.from(line)).toHaveLength(150);
    const [cut] = promptContextLines("\u{1F600}".repeat(250));
    expect(Array.from(cut)).toHaveLength(200);
    expect(cut.endsWith("…")).toBe(true);
    expect(cut).not.toMatch(/[\ud800-\udbff](?![\udc00-\udfff])/);
  });
});

describe("commandLines", () => {
  it("is nothing for a prompt without a command", () => {
    expect(commandLines(undefined)).toEqual([]);
    expect(commandLines("")).toEqual([]);
    expect(commandLines("\n  \n")).toEqual([]);
  });

  it("keeps the line breaks of a command", () => {
    expect(commandLines("rm -rf build\r\nls -la\n")).toEqual(["rm -rf build", "ls -la"]);
  });

  it("redacts and strips again what the core already did", () => {
    const lines = commandLines(`curl -H "Authorization: Bearer abc.def.ghi" ${"\u001b"}[31mhttps://example.test${"\u001b"}[0m`);
    expect(lines.join("\n")).not.toContain("abc.def.ghi");
    expect(lines.join("\n")).not.toContain("\u001b");
    expect(lines.join("\n")).toContain("https://example.test");
  });

  it("does not cut the core's own marker or a command's last words", () => {
    const withMarker = [...Array.from({ length: 8 }, (_, index) => `echo ${index}`), "…"].join("\n");
    expect(commandLines(withMarker)).toHaveLength(9);
    expect(commandLines("x".repeat(500))[0].length).toBe(240);
  });

  it("marks the lines it drops, as the core marks its own cut", () => {
    const many = Array.from({ length: 14 }, (_, index) => `echo ${index}`).join("\n");
    const lines = commandLines(many);
    expect(lines).toHaveLength(13);
    expect(lines[11]).toBe("echo 11");
    expect(lines[12]).toBe("…");
    expect(commandLines(Array.from({ length: 12 }, (_, index) => `echo ${index}`).join("\n"))).toHaveLength(12);
  });

  it("drops the blank lines around a command and normalises a lone carriage return", () => {
    expect(commandLines("\n\n  \necho a\recho b\n\n")).toEqual(["echo a", "echo b"]);
  });

  it("cuts a line at a character, not through an emoji", () => {
    const [line] = commandLines("\u{1F600}".repeat(130));
    expect(Array.from(line)).toHaveLength(130);
    const [cut] = commandLines("\u{1F600}".repeat(300));
    expect(Array.from(cut)).toHaveLength(240);
    expect(cut).not.toMatch(/[\ud800-\udbff](?![\udc00-\udfff])/);
  });

  // A person reads this text to decide what runs. A right-to-left override
  // reorders what follows it on screen, and "rm" followed by a zero-width space is
  // not the word it looks like.
  it("shows a format character as a visible mark", () => {
    const mark = String.fromCharCode(0xfffd);
    const override = String.fromCharCode(0x202e);
    const zeroWidth = String.fromCharCode(0x200b);
    expect(commandLines(`echo ok${override}txt.exe`)).toEqual([`echo ok${mark}txt.exe`]);
    expect(commandLines(`rm${zeroWidth} -rf x`)).toEqual([`rm${mark} -rf x`]);
    expect(commandLines(`${zeroWidth}${zeroWidth}`)).toEqual([`${mark}${mark}`]);
  });

  it("does not stall on a long unbroken command", () => {
    const started = performance.now();
    const lines = commandLines("a".repeat(256 * 1024));
    expect(performance.now() - started).toBeLessThan(1000);
    expect(lines).toHaveLength(1);
  });
});

describe("redactForDisplay on a long text", () => {
  it("is bounded before it is redacted", () => {
    const started = performance.now();
    const shown = redactForDisplay("a".repeat(256 * 1024));
    expect(performance.now() - started).toBeLessThan(1000);
    expect(shown).toHaveLength(240);
  });

  it("still redacts a secret that sits where the text is cut", () => {
    const shown = redactForDisplay(`${"x".repeat(230)} password=hunter2hunter2`);
    expect(shown).not.toContain("hunter2");
  });
});
