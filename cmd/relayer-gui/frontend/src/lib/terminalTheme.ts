import type { ITheme } from "@xterm/xterm";

// The palette every Relayer terminal surface renders with — live panes, and the
// replay of a recorded session. It lives here so a recorded run cannot drift
// from the run an operator watched live: the same bytes must produce the same
// colours whichever view they land in.
export const RELAYER_TERMINAL_THEME: ITheme = {
  background: "#080b11",
  foreground: "#bfcadb",
  cursor: "#42d9e8",
  cursorAccent: "#080b11",
  selectionBackground: "rgba(66, 217, 232, 0.3)",
  black: "#080b12",
  red: "#ff5e73",
  green: "#49d79a",
  yellow: "#ff9f52",
  blue: "#42d9e8",
  magenta: "#9a8cff",
  cyan: "#42d9e8",
  white: "#e8edf7",
  brightBlack: "#8792a4",
  brightRed: "#ff7588",
  brightGreen: "#5fe3a8",
  brightYellow: "#ffb273",
  brightBlue: "#5ce0ed",
  brightMagenta: "#aba0ff",
  brightCyan: "#67e5f2",
  brightWhite: "#ffffff",
};
