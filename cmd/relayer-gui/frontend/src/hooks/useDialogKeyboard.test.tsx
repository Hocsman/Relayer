/** @vitest-environment jsdom */
import { act, useRef, useState, type ReactNode } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { useDialogKeyboard } from "./useDialogKeyboard";

declare global {
  // eslint-disable-next-line no-var
  var IS_REACT_ACT_ENVIRONMENT: boolean;
}

let container: HTMLDivElement;
let root: Root;

beforeEach(() => {
  globalThis.IS_REACT_ACT_ENVIRONMENT = true;
  container = document.createElement("div");
  document.body.appendChild(container);
  root = createRoot(container);
});

afterEach(() => {
  act(() => root.unmount());
  container.remove();
});

function render(node: ReactNode) {
  act(() => root.render(node));
}

// The focusout recovery runs on a macrotask so it can read the focus the
// browser settles on after the current event.
async function settle() {
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 1));
  });
}

function press(key: string, options: KeyboardEventInit = {}) {
  act(() => {
    window.dispatchEvent(new KeyboardEvent("keydown", { key, bubbles: true, ...options }));
  });
}

/**
 * Dialog reproduces the shape every real caller has: an inline arrow for
 * onClose, so the prop identity changes on every render, and a parent that
 * re-renders while the dialog is open.
 */
function Dialog({
  onClose,
  active = true,
  closable = true,
  bumpRef,
  children,
}: {
  onClose?: () => void;
  active?: boolean;
  closable?: boolean;
  bumpRef?: { current: (() => void) | null };
  children?: ReactNode;
}) {
  const dialogRef = useRef<HTMLDivElement>(null);
  const [, setTick] = useState(0);
  if (bumpRef) bumpRef.current = () => setTick((value) => value + 1);
  useDialogKeyboard(dialogRef, {
    onClose: () => onClose?.(),
    closable,
    active,
  });
  if (!active) return null;
  return (
    <div ref={dialogRef} role="dialog">
      {children ?? (
        <>
          {/* eslint-disable-next-line jsx-a11y/no-autofocus */}
          <button type="button" autoFocus>
            first
          </button>
          <input aria-label="field" />
          <button type="button">last</button>
        </>
      )}
    </div>
  );
}

const byLabel = (label: string) =>
  document.querySelector<HTMLInputElement>(`[aria-label="${label}"]`)!;
const byText = (text: string) =>
  [...document.querySelectorAll("button")].find((b) => b.textContent === text)!;

describe("useDialogKeyboard", () => {
  // The regression that made the settings panel unusable: the effect depended
  // on onClose, every caller passes an inline arrow, and the cleanup restored
  // focus — so one keystroke re-ran the effect and threw focus onto the close
  // button. This test fails against that version.
  it("does not move focus when the parent re-renders", async () => {
    const bump = { current: null as (() => void) | null };
    render(<Dialog bumpRef={bump} />);

    // The dialog autofocuses a control of its own first, exactly as the real
    // panels do; the operator then moves to the field and types.
    expect(document.activeElement).toBe(byText("first"));
    const field = byLabel("field");
    act(() => field.focus());
    expect(document.activeElement).toBe(field);

    for (let i = 0; i < 5; i += 1) act(() => bump.current?.());
    await settle();

    expect(document.activeElement).toBe(field);
  });

  it("closes on Escape, and does not when the dialog is locked", () => {
    const onClose = vi.fn();
    render(<Dialog onClose={onClose} />);
    press("Escape");
    expect(onClose).toHaveBeenCalledTimes(1);

    render(<Dialog onClose={onClose} closable={false} />);
    press("Escape");
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  // The settings panel opens a confirmation on top of itself, and a stop
  // confirmation can be mounted beside the decision modal. Without an order,
  // one Escape answered every mounted dialog at once.
  it("gives Escape to the topmost dialog only", () => {
    const closeUnder = vi.fn();
    const closeOver = vi.fn();
    render(
      <>
        <Dialog onClose={closeUnder} />
        <Dialog onClose={closeOver}>
          <button type="button">only</button>
        </Dialog>
      </>,
    );
    press("Escape");
    expect(closeOver).toHaveBeenCalledTimes(1);
    expect(closeUnder).not.toHaveBeenCalled();
  });

  it("keeps Tab inside the dialog", () => {
    render(<Dialog />);
    const first = byText("first");
    const last = byText("last");

    act(() => last.focus());
    press("Tab");
    expect(document.activeElement).toBe(first);

    press("Tab", { shiftKey: true });
    expect(document.activeElement).toBe(last);
  });

  it("returns focus to whatever opened it", async () => {
    const opener = document.createElement("button");
    document.body.appendChild(opener);
    opener.focus();
    expect(document.activeElement).toBe(opener);

    render(<Dialog />);
    act(() => byLabel("field").focus());

    render(<Dialog active={false} />);
    await settle();

    expect(document.activeElement).toBe(opener);
    opener.remove();
  });

  // Every one of these dialogs disables the focused control while a decision is
  // in flight, which drops focus to <body>. From there the next Tab starts at
  // the top of the document instead of back in the dialog.
  it("recovers focus when the focused control is disabled underneath it", async () => {
    render(<Dialog />);
    const last = byText("last");
    act(() => last.focus());
    expect(document.activeElement).toBe(last);

    act(() => {
      last.disabled = true;
      last.blur();
    });
    await settle();

    expect(document.activeElement).not.toBe(document.body);
    expect(document.querySelector('[role="dialog"]')!.contains(document.activeElement)).toBe(true);
  });
});
