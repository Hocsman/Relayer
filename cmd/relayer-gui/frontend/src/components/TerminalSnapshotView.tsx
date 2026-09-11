import { useEffect, useRef, useState } from "react";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import { SearchAddon } from "@xterm/addon-search";
import "@xterm/xterm/css/xterm.css";

interface TerminalSnapshotViewProps {
  runID: string;
  sessionID: string;
  label: string;
  output: string;
  revision: number;
  onResize(runID: string, sessionID: string, columns: number, rows: number): Promise<void>;
}

const RELAYER_TERMINAL_THEME = {
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

export function TerminalSnapshotView({
  runID,
  sessionID,
  label,
  output,
  revision,
  onResize,
}: TerminalSnapshotViewProps) {
  const containerRef = useRef<HTMLDivElement>(null);
  const termRef = useRef<Terminal | null>(null);
  const fitAddonRef = useRef<FitAddon | null>(null);
  const searchAddonRef = useRef<SearchAddon | null>(null);
  const searchInputRef = useRef<HTMLInputElement>(null);
  const followRef = useRef(true);
  const resizeRef = useRef(onResize);
  const lastSizeRef = useRef({ columns: 0, rows: 0 });
  const lastOutputRef = useRef<string>("");
  const currentIdentityRef = useRef<string>("");
  const [following, setFollowing] = useState(true);
  const [showSearch, setShowSearch] = useState(false);
  const [searchQuery, setSearchQuery] = useState("");

  resizeRef.current = onResize;

  // Initialize xterm.js instance and attach to container DOM
  useEffect(() => {
    const container = containerRef.current;
    if (!container) return;

    const term = new Terminal({
      cursorBlink: true,
      convertEol: true,
      fontSize: 12,
      fontFamily: '"SFMono-Regular", Consolas, "Liberation Mono", ui-monospace, monospace',
      lineHeight: 1.4,
      theme: RELAYER_TERMINAL_THEME,
      scrollback: 5000,
      allowProposedApi: true,
    });

    const fitAddon = new FitAddon();
    term.loadAddon(fitAddon);

    const searchAddon = new SearchAddon();
    term.loadAddon(searchAddon);

    termRef.current = term;
    fitAddonRef.current = fitAddon;
    searchAddonRef.current = searchAddon;
    term.open(container);

    try {
      fitAddon.fit();
    } catch {
      // Container may have 0 dimensions initially
    }

    const scrollDispose = term.onScroll(() => {
      const isBottom = term.buffer.active.viewportY >= term.buffer.active.baseY;
      followRef.current = isBottom;
      setFollowing(isBottom);
    });

    let resizeTimeout = 0;
    const reportSize = () => {
      window.clearTimeout(resizeTimeout);
      resizeTimeout = window.setTimeout(() => {
        if (!containerRef.current || !termRef.current || !fitAddonRef.current) return;
        try {
          fitAddonRef.current.fit();
          const columns = termRef.current.cols;
          const rows = termRef.current.rows;
          if (
            columns > 0 &&
            rows > 0 &&
            (columns !== lastSizeRef.current.columns || rows !== lastSizeRef.current.rows)
          ) {
            lastSizeRef.current = { columns, rows };
            void resizeRef.current(runID, sessionID, columns, rows);
          }
        } catch {
          // Ignore layout transitions
        }
      }, 120);
    };

    const observer = new ResizeObserver(reportSize);
    observer.observe(container);
    reportSize();

    return () => {
      observer.disconnect();
      window.clearTimeout(resizeTimeout);
      scrollDispose.dispose();
      searchAddon.dispose();
      term.dispose();
      termRef.current = null;
      fitAddonRef.current = null;
      searchAddonRef.current = null;
      lastOutputRef.current = "";
    };
  }, [runID, sessionID]);

  // Synchronize terminal output with incoming stream
  useEffect(() => {
    const term = termRef.current;
    if (!term) return;

    const identity = `${runID}\u0000${sessionID}`;
    const identityChanged = currentIdentityRef.current !== identity;
    currentIdentityRef.current = identity;

    if (identityChanged) {
      term.reset();
      lastOutputRef.current = "";
    }

    if (!output) {
      if (lastOutputRef.current) {
        term.reset();
        lastOutputRef.current = "";
      }
      return;
    }

    // Incremental write when stream appends, or reset if output was replaced/cleared
    if (lastOutputRef.current && output.startsWith(lastOutputRef.current)) {
      const delta = output.slice(lastOutputRef.current.length);
      if (delta) {
        term.write(delta);
      }
    } else {
      term.reset();
      term.write(output);
    }

    lastOutputRef.current = output;

    if (followRef.current) {
      term.scrollToBottom();
    }
  }, [output, revision, runID, sessionID]);

  const resumeFollowing = () => {
    const term = termRef.current;
    if (!term) return;
    followRef.current = true;
    setFollowing(true);
    term.scrollToBottom();
  };

  const handleOpenSearch = () => {
    setShowSearch(true);
    window.setTimeout(() => {
      searchInputRef.current?.focus();
      searchInputRef.current?.select();
    }, 50);
  };

  const handleCloseSearch = () => {
    setShowSearch(false);
    setSearchQuery("");
    searchAddonRef.current?.clearDecorations();
    containerRef.current?.focus();
  };

  const handleSearchChange = (query: string) => {
    setSearchQuery(query);
    if (query.trim()) {
      searchAddonRef.current?.findNext(query, { incremental: true });
    } else {
      searchAddonRef.current?.clearDecorations();
    }
  };

  const handleFindNext = () => {
    if (searchQuery.trim()) {
      searchAddonRef.current?.findNext(searchQuery);
    }
  };

  const handleFindPrev = () => {
    if (searchQuery.trim()) {
      searchAddonRef.current?.findPrevious(searchQuery);
    }
  };

  const handleKeyDown = (event: React.KeyboardEvent) => {
    if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === "f") {
      event.preventDefault();
      event.stopPropagation();
      handleOpenSearch();
    }
  };

  return (
    <div className="terminal-shell" onKeyDown={handleKeyDown}>
      <div
        ref={containerRef}
        className="terminal-snapshot"
        role="log"
        tabIndex={0}
        aria-label={label}
        aria-live="off"
        data-revision={revision}
      >
        {!output && <p className="terminal-snapshot__empty">Waiting for output…</p>}
      </div>

      {showSearch ? (
        <div className="terminal-search-bar" role="search">
          <input
            ref={searchInputRef}
            type="text"
            className="terminal-search-input"
            placeholder="Find in terminal… (Enter / Shift+Enter)"
            value={searchQuery}
            onChange={(e) => handleSearchChange(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") {
                e.preventDefault();
                if (e.shiftKey) handleFindPrev();
                else handleFindNext();
              } else if (e.key === "Escape") {
                e.preventDefault();
                handleCloseSearch();
              }
            }}
          />
          <button
            type="button"
            className="terminal-search-btn"
            title="Previous match (Shift+Enter)"
            aria-label="Previous match"
            onClick={handleFindPrev}
          >
            ▲
          </button>
          <button
            type="button"
            className="terminal-search-btn"
            title="Next match (Enter)"
            aria-label="Next match"
            onClick={handleFindNext}
          >
            ▼
          </button>
          <button
            type="button"
            className="terminal-search-btn terminal-search-btn--close"
            title="Close search (Esc)"
            aria-label="Close search"
            onClick={handleCloseSearch}
          >
            ✕
          </button>
        </div>
      ) : (
        <button
          type="button"
          className="terminal-search-trigger"
          title="Search in terminal (Ctrl+F)"
          aria-label="Search terminal output"
          onClick={handleOpenSearch}
        >
          🔍
        </button>
      )}

      {!following && (
        <button className="follow-button" type="button" onClick={resumeFollowing}>
          <span aria-hidden="true">↓</span> Resume live
        </button>
      )}
    </div>
  );
}
