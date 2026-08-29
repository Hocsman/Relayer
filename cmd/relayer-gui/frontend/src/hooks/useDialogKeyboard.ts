import { useEffect, useRef, type RefObject } from "react";

const FOCUSABLE = [
  "a[href]",
  "button:not([disabled])",
  "input:not([disabled])",
  "select:not([disabled])",
  "textarea:not([disabled])",
  "[tabindex]:not([tabindex='-1'])",
].join(",");

function focusableWithin(container: HTMLElement): HTMLElement[] {
  return Array.from(container.querySelectorAll<HTMLElement>(FOCUSABLE)).filter(
    (element) => element.offsetParent !== null || element === document.activeElement,
  );
}

// Dialogs stack: the settings panel opens a restart confirmation on top of
// itself, and the stop confirmation can be open while a decision modal is
// mounted. Without an order, every mounted dialog answers the same Escape and
// each one drags focus back into itself, so two open dialogs fight forever.
// Last registered is topmost.
const openDialogs: HTMLElement[] = [];

function isTopmost(container: HTMLElement): boolean {
  return openDialogs[openDialogs.length - 1] === container;
}

/**
 * useDialogKeyboard gives a modal the keyboard behaviour its aria-modal already
 * claims: Escape closes it, Tab cannot leave it, and focus returns where it came
 * from.
 *
 * Without the trap, aria-modal="true" tells assistive technology the rest of the
 * page is inert while Tab walks straight out of the dialog into the controls
 * behind it — including, in this application, the stop control and the agent
 * cards the dialog is asking a question about.
 *
 * Three details are load-bearing, and each one was a bug first:
 *
 *   - `onClose` and `closable` are read through refs and are NOT dependencies.
 *     Every caller passes an inline arrow, so depending on them re-ran the whole
 *     effect on every render — and the cleanup restores focus, so typing one
 *     character in the settings panel threw focus onto its close button.
 *   - `previouslyFocused` is captured during render, not in the effect. React
 *     applies `autoFocus` in the commit phase, before effects run, so an
 *     effect-time capture records a control inside the dialog rather than the
 *     one that opened it.
 *   - Restoring is keyed to `active`, not to unmount. The decision modal is
 *     rendered unconditionally and returns null, so it never unmounts.
 */
export function useDialogKeyboard(
  containerRef: RefObject<HTMLElement | null>,
  options: { onClose?: () => void; closable?: boolean; active?: boolean } = {},
): void {
  const { onClose, closable = true, active = true } = options;

  const onCloseRef = useRef(onClose);
  const closableRef = useRef(closable);
  onCloseRef.current = onClose;
  closableRef.current = closable;

  const previouslyFocused = useRef<HTMLElement | null>(null);
  if (active && previouslyFocused.current === null && typeof document !== "undefined") {
    previouslyFocused.current = document.activeElement as HTMLElement | null;
  }

  useEffect(() => {
    if (!active) return;
    return () => {
      const previous = previouslyFocused.current;
      previouslyFocused.current = null;
      if (previous && previous.isConnected) previous.focus();
    };
  }, [active]);

  useEffect(() => {
    const container = containerRef.current;
    if (!container || !active) return;

    openDialogs.push(container);

    const onKeyDown = (event: KeyboardEvent) => {
      if (!isTopmost(container)) return;

      if (event.key === "Escape") {
        if (closableRef.current && onCloseRef.current) {
          event.preventDefault();
          event.stopPropagation();
          onCloseRef.current();
        }
        return;
      }
      if (event.key !== "Tab") return;

      const focusable = focusableWithin(container);
      if (focusable.length === 0) {
        event.preventDefault();
        return;
      }
      const first = focusable[0];
      const last = focusable[focusable.length - 1];
      const activeElement = document.activeElement;

      if (!container.contains(activeElement)) {
        event.preventDefault();
        (event.shiftKey ? last : first).focus();
        return;
      }
      if (event.shiftKey && activeElement === first) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && activeElement === last) {
        event.preventDefault();
        first.focus();
      }
    };

    // Disabling the focused control is how focus escapes to <body> here, and it
    // happens on a path the operator did not choose, so recovery is silent.
    // Only the topmost dialog recovers, or two of them trade focus endlessly.
    const onFocusOut = () => {
      window.setTimeout(() => {
        if (!container.isConnected || !isTopmost(container)) return;
        const current = document.activeElement;
        if (current && current !== document.body && container.contains(current)) return;
        const focusable = focusableWithin(container);
        if (focusable.length > 0) {
          focusable[0].focus();
          return;
        }
        // A section is not focusable on its own, so give it a programmatic-only
        // tabstop rather than leaving focus on <body>.
        container.tabIndex = -1;
        container.focus();
      }, 0);
    };

    window.addEventListener("keydown", onKeyDown, true);
    container.addEventListener("focusout", onFocusOut);
    return () => {
      const index = openDialogs.indexOf(container);
      if (index >= 0) openDialogs.splice(index, 1);
      window.removeEventListener("keydown", onKeyDown, true);
      container.removeEventListener("focusout", onFocusOut);
    };
  }, [containerRef, active]);
}
