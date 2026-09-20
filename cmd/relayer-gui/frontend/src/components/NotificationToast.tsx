import React from "react";
import type { NotificationToastItem } from "../hooks/useNotifications";

interface NotificationToastProps {
  toasts: NotificationToastItem[];
  onDismiss: (id: string) => void;
}

export function NotificationToast({ toasts, onDismiss }: NotificationToastProps) {
  if (toasts.length === 0) {
    return null;
  }

  return (
    <div
      className="notification-toast-container"
      role="region"
      aria-label="Notifications"
      aria-live="polite"
    >
      {toasts.map((toast) => {
        let icon = "ℹ️";
        if (toast.severity === "critical") {
          icon = "🛡️";
        } else if (toast.severity === "warning") {
          icon = "⏳";
        }

        return (
          <div
            key={toast.id}
            className={`notification-toast notification-toast--${toast.severity}`}
            role="alert"
          >
            <span className="notification-toast__icon" aria-hidden="true">
              {icon}
            </span>
            <div className="notification-toast__content">
              <div className="notification-toast__title">{toast.title}</div>
              <div className="notification-toast__body">{toast.body}</div>
              {(toast.agentName || toast.reason) && (
                <div className="notification-toast__agent">
                  {toast.agentName ? `${toast.agentName}` : ""}
                  {toast.agentName && toast.reason ? " — " : ""}
                  {toast.reason || ""}
                </div>
              )}
            </div>
            <button
              type="button"
              className="notification-toast__close"
              aria-label="Fermer la notification"
              onClick={() => onDismiss(toast.id)}
            >
              ×
            </button>
          </div>
        );
      })}
    </div>
  );
}
