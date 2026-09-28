import { useCallback, useEffect, useState } from "react";
import type { RelayerBridge, UpdateInfo } from "../types/relayer";

const enabledKey = "relayer:updateCheck";
const dismissedKey = "relayer:updateDismissed";

function readStorage(key: string): string | null {
  try {
    return window.localStorage?.getItem(key) ?? null;
  } catch {
    return null;
  }
}

function writeStorage(key: string, value: string) {
  try {
    window.localStorage?.setItem(key, value);
  } catch {
    // A preference that cannot be stored lasts until the window closes.
  }
}

// useUpdateCheck asks the desktop, once per launch, whether a newer release
// is out, unless the user turned the check off. The check is on by default;
// the web gateway's bridge has none, so there it never runs.
export function useUpdateCheck(bridge: RelayerBridge) {
  const supported = typeof bridge.checkForUpdate === "function";
  const [enabled, setEnabledState] = useState(() => readStorage(enabledKey) !== "false");
  const [update, setUpdate] = useState<UpdateInfo>();
  const [dismissed, setDismissed] = useState(() => readStorage(dismissedKey) ?? "");

  useEffect(() => {
    if (!supported || !enabled) {
      return;
    }
    let cancelled = false;
    bridge.checkForUpdate!()
      .then((info) => {
        if (!cancelled) setUpdate(info);
      })
      .catch(() => {
        // Offline, rate limited or blocked: nothing to offer, and nothing an
        // operator supervising agents needs to be interrupted about.
      });
    return () => {
      cancelled = true;
    };
  }, [bridge, supported, enabled]);

  const setEnabled = useCallback((value: boolean) => {
    setEnabledState(value);
    writeStorage(enabledKey, String(value));
    if (!value) setUpdate(undefined);
  }, []);

  const dismiss = useCallback(() => {
    if (!update?.latest) return;
    setDismissed(update.latest);
    writeStorage(dismissedKey, update.latest);
  }, [update]);

  const open = useCallback(() => bridge.openUpdate?.() ?? Promise.resolve(), [bridge]);

  const offered =
    enabled && update?.available && update.latest && update.latest !== dismissed ? update : undefined;

  return { supported, enabled, setEnabled, update: offered, dismiss, open };
}
