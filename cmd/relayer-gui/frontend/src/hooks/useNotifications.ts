import { useCallback, useEffect, useState } from "react";
import type { NotificationEvent, RelayerBridge } from "../types/relayer";

export interface NotificationToastItem {
  id: string;
  title: string;
  body: string;
  agentName?: string;
  sessionID?: string;
  eventID?: string;
  kind: string;
  severity: string;
  reason?: string;
  timestamp: number;
}

export function useNotifications(bridge: RelayerBridge) {
  const [toasts, setToasts] = useState<NotificationToastItem[]>([]);
  const [permissionState, setPermissionState] = useState<NotificationPermission>(() => {
    if (typeof window !== "undefined" && "Notification" in window) {
      return Notification.permission;
    }
    return "denied";
  });

  const [soundEnabled, setSoundEnabledState] = useState<boolean>(() => {
    if (typeof window !== "undefined" && window.localStorage) {
      const stored = window.localStorage.getItem("relayer:soundEnabled");
      return stored === null ? true : stored === "true";
    }
    return true;
  });

  const setSoundEnabled = useCallback((enabled: boolean) => {
    setSoundEnabledState(enabled);
    if (typeof window !== "undefined" && window.localStorage) {
      window.localStorage.setItem("relayer:soundEnabled", String(enabled));
    }
  }, []);

  const playChime = useCallback((severity: string) => {
    try {
      const AudioCtx =
        window.AudioContext ||
        (window as unknown as { webkitAudioContext: typeof AudioContext }).webkitAudioContext;
      if (!AudioCtx) return;
      const ctx = new AudioCtx();
      const osc = ctx.createOscillator();
      const gain = ctx.createGain();

      osc.type = severity === "critical" ? "sawtooth" : "sine";
      osc.frequency.setValueAtTime(severity === "critical" ? 880 : 587.33, ctx.currentTime);
      if (severity === "critical") {
        osc.frequency.exponentialRampToValueAtTime(440, ctx.currentTime + 0.25);
      } else {
        osc.frequency.setValueAtTime(880, ctx.currentTime + 0.1);
      }

      gain.gain.setValueAtTime(0.12, ctx.currentTime);
      gain.gain.exponentialRampToValueAtTime(0.001, ctx.currentTime + 0.25);

      osc.connect(gain);
      gain.connect(ctx.destination);

      osc.start();
      osc.stop(ctx.currentTime + 0.25);
      osc.onended = () => {
        void ctx.close();
      };
    } catch {
      // AudioContext might be blocked before explicit user interaction
    }
  }, []);

  const requestPermission = useCallback(async () => {
    if (typeof window === "undefined" || !("Notification" in window)) {
      return "denied" as NotificationPermission;
    }
    try {
      const result = await Notification.requestPermission();
      setPermissionState(result);
      return result;
    } catch {
      return "denied" as NotificationPermission;
    }
  }, []);

  useEffect(() => {
    if (!bridge) return;

    const unsubscribe = bridge.on("relayer:notification", (notif: NotificationEvent) => {
      // 1. Audio chime
      if (
        soundEnabled &&
        (notif.severity === "warning" ||
          notif.severity === "critical" ||
          notif.kind === "pending_decision")
      ) {
        playChime(notif.severity);
      }

      // 2. Native browser notification if window is in the background
      if (
        typeof window !== "undefined" &&
        "Notification" in window &&
        Notification.permission === "granted" &&
        (document.hidden || !document.hasFocus())
      ) {
        try {
          const nativeNotif = new Notification(notif.title || "Relayer Alert", {
            body: notif.body || notif.reason || "Agent requires human attention",
            tag: notif.eventID || notif.sessionID,
          });
          nativeNotif.onclick = () => {
            window.focus();
            nativeNotif.close();
          };
        } catch {
          // ignore notification instantiation failure
        }
      }

      // 3. Toast item in queue
      const toastId =
        notif.eventID || `toast-${Date.now()}-${Math.random().toString(36).slice(2, 6)}`;
      setToasts((current) => {
        if (notif.eventID && current.some((t) => t.eventID === notif.eventID)) {
          return current;
        }
        const item: NotificationToastItem = {
          id: toastId,
          title: notif.title || "Relayer Alert",
          body: notif.body || notif.reason || "Arbitration required",
          agentName: notif.agentName,
          sessionID: notif.sessionID,
          eventID: notif.eventID,
          kind: notif.kind,
          severity: notif.severity || "info",
          reason: notif.reason,
          timestamp: Date.now(),
        };
        return [item, ...current].slice(0, 5);
      });
    });

    return () => {
      unsubscribe();
    };
  }, [bridge, soundEnabled, playChime]);

  // Auto-dismiss after 8 seconds
  useEffect(() => {
    if (toasts.length === 0) return;
    const timer = setInterval(() => {
      const now = Date.now();
      setToasts((current) => current.filter((t) => now - t.timestamp < 8000));
    }, 1000);
    return () => clearInterval(timer);
  }, [toasts.length]);

  const dismissToast = useCallback((id: string) => {
    setToasts((current) => current.filter((t) => t.id !== id));
  }, []);

  return {
    toasts,
    dismissToast,
    permissionState,
    requestPermission,
    soundEnabled,
    setSoundEnabled,
  };
}
