import type { SupervisionEvent } from "../types/relayer";

export function deliveryRequiresResync(event: SupervisionEvent): boolean {
  return (
    event.deliveryStatus === "uncertain" ||
    event.deliveryStatus === "failed"
  );
}
