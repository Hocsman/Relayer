import { useEffect, type RefObject } from "react";

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
 * It also catches the case that made this visible: several of these dialogs
 * disable the control that currently has focus while a decision is in flight,
 * which drops focus to <body>. From there the next Tab starts at the top of the
 * document rather than back in the dialog, so focus is pulled back in.
 */
export function useDialogKeyboard(
  containerRef: RefObject<HTMLElement | null>,
  options: { onClose?: () => void; closable?: boolean; active?: boolean } = {},
): void {
  const { onClose, closable = true, active = true } = options;

  useEffect(() => {
    const container = containerRef.current;
    if (!container || !active) return;

    const previouslyFocused = document.activeElement as HTMLElement | null;

    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        if (closable && onClose) {
          event.preventDefault();
          onClose();
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
      const active = document.activeElement;

      if (!container.contains(active)) {
        event.preventDefault();
        (event.shiftKey ? last : first).focus();
        return;
      }
      if (event.shiftKey && active === first) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && active === last) {
        event.preventDefault();
        first.focus();
      }
    };

    // Disabling the focused control is how focus escapes to <body> here, and it
    // happens on a path the operator did not choose, so recovery is silent.
    const onFocusOut = () => {
      window.setTimeout(() => {
        const active = document.activeElement;
        if (active && active !== document.body && container.contains(active)) return;
        if (!container.isConnected) return;
        const focusable = focusableWithin(container);
        (focusable[0] ?? container).focus();
      }, 0);
    };

    window.addEventListener("keydown", onKeyDown, true);
    container.addEventListener("focusout", onFocusOut);
    return () => {
      window.removeEventListener("keydown", onKeyDown, true);
      container.removeEventListener("focusout", onFocusOut);
      if (previouslyFocused && previouslyFocused.isConnected) {
        previouslyFocused.focus();
      }
    };
  }, [containerRef, onClose, closable, active]);
}
