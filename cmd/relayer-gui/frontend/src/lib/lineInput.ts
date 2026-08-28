export interface UncontrolledLineInput {
  value: string;
}

export interface LineInputAvailability {
  running: boolean;
  attached: boolean;
  status: string;
  inputFrozen?: boolean;
}

export function lineInputDisabled(
  agent: LineInputAvailability,
  waiting: boolean,
  submitting: boolean,
): boolean {
  return (
    waiting ||
    submitting ||
    Boolean(agent.inputFrozen) ||
    !agent.running ||
    agent.attached ||
    (agent.status !== "running" && agent.status !== "detached")
  );
}

export function discardUnavailableLine(
  input: UncontrolledLineInput | null,
  disabled: boolean,
  identityChanged: boolean,
): void {
  if (input && (disabled || identityChanged)) {
    input.value = "";
  }
}

// The browser field is cleared synchronously before the asynchronous native
// delivery is awaited. The value is never returned or placed in UI state.
export async function submitUncontrolledLine(
  input: UncontrolledLineInput,
  deliver: (line: string) => Promise<void>,
): Promise<void> {
  let line = input.value;
  input.value = "";
  try {
    const pending = deliver(line);
    line = "";
    await pending;
  } finally {
    line = "";
  }
}
