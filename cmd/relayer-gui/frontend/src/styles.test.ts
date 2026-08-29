import postcss from "postcss";
import { describe, expect, it } from "vitest";
// Vite's ?raw import keeps this test free of Node type declarations, which the
// application itself never needs.
import source from "./styles.css?raw";

const root = postcss.parse(source);

const rules: { selectors: string[]; selector: string; props: string[] }[] = [];
root.walkRules((rule) => {
  rules.push({
    selectors: rule.selectors,
    selector: rule.selector.replace(/\s+/g, " "),
    props: rule.nodes.filter((node) => node.type === "decl").map((node) => node.prop),
  });
});

// :not(:disabled) contains the substring ":disabled" and is the correct guard,
// so it has to come out before asking whether a selector targets disabled.
const withoutNot = (selector: string) => selector.replace(/:not\([^)]*\)/g, "");

describe("styles.css", () => {
  // Deleting a rule body while leaving its selectors and their trailing comma
  // makes those selectors join the NEXT rule. It still parses, so nothing
  // complains, and the interface quietly starts styling the wrong things: this
  // exact mistake made every enabled move-up arrow render at 28% opacity with a
  // not-allowed cursor.
  it("has no rule that styles :hover and a real :disabled together", () => {
    const merged = rules.filter(
      (rule) =>
        rule.selectors.some((selector) => withoutNot(selector).includes(":hover")) &&
        rule.selectors.some((selector) => withoutNot(selector).includes(":disabled")),
    );
    expect(merged.map((rule) => rule.selector)).toEqual([]);
  });

  it("has no empty selector in a selector list", () => {
    const empty = rules.filter((rule) => rule.selectors.some((selector) => selector.trim() === ""));
    expect(empty.map((rule) => rule.selector)).toEqual([]);
  });

  // A hover that fires on a disabled control invites a click that does nothing.
  it("guards every hover rule that shares a base class with a disabled rule", () => {
    const disabledBases = new Set(
      rules
        .flatMap((rule) => rule.selectors)
        .filter((selector) => withoutNot(selector).includes(":disabled"))
        .map((selector) => withoutNot(selector).split(":")[0].trim()),
    );
    const unguarded = rules
      .flatMap((rule) => rule.selectors)
      .filter((selector) => {
        if (!selector.includes(":hover") || selector.includes(":not(:disabled)")) return false;
        return disabledBases.has(selector.split(":")[0].trim());
      });
    expect(unguarded).toEqual([]);
  });

  // The three text tiers are the palette's whole hierarchy. Redefining one
  // elsewhere in the file silently splits it in two.
  it("declares each palette token exactly once", () => {
    const declarations: Record<string, number> = {};
    root.walkDecls(/^--/, (declaration) => {
      // Only the palette. --wails-draggable is a per-element property the
      // runtime reads to make a region drag the window, so it is declared on
      // every such region by design.
      const parent = declaration.parent;
      if (!parent || parent.type !== "rule" || (parent as { selector: string }).selector !== ":root") {
        return;
      }
      declarations[declaration.prop] = (declarations[declaration.prop] ?? 0) + 1;
    });
    const duplicated = Object.entries(declarations).filter(([, count]) => count > 1);
    expect(duplicated).toEqual([]);
  });

  // outline:none in a base rule removes the focus ring in every state, not just
  // on pointer focus. It is only acceptable inside a :focus rule that puts a
  // visible indicator back.
  it("never removes the focus outline outside a :focus rule", () => {
    const offenders = rules
      .filter((rule) => rule.props.includes("outline"))
      .filter((rule) => !rule.selectors.every((selector) => selector.includes(":focus")))
      .map((rule) => rule.selector);
    expect(offenders).toEqual([]);
  });
});
