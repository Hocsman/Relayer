# Reproducible alpha demo for VHS: https://github.com/charmbracelet/vhs
#
# Run from the repository root with:
#   vhs docs/demo.tape
#
# The recording uses only Relayer's deterministic Bash mocks. It starts two
# Relayer-owned tmux sessions, briefly demonstrates native attach/detach,
# answers both synthetic prompts, and writes the GIF the README embeds plus an
# ignored video in the repository root. VHS currently parses relative Output
# paths more reliably than absolute paths.
#
# For the short GIF the README embeds, see docs/readme-demo.tape instead: this
# tape spends most of its runtime on setup and native tmux attach, which is the
# point when reproducing behaviour and the wrong thing to show a newcomer.

Output relayer-demo.mp4

Require bash
Require chmod
Require go
Require id
Require mktemp
Require mkdir
Require rm
Require rmdir
Require sed
Require sleep
Require tmux

Set Shell "bash"
Set FontSize 16
Set Width 1400
Set Height 900
Set TypingSpeed 35ms
Set PlaybackSpeed 1

Type "go version && tmux -V"
Enter
Sleep 2s

# Use one private workspace and install cleanup before starting tmux. The
# unrelated bootstrap session also demonstrates that Relayer leaves unowned
# sessions alone.
Type `RELAYER_DEMO_WORKDIR=$(mktemp -d /tmp/relayer-demo.XXXXXX); chmod 700 "$RELAYER_DEMO_WORKDIR"; RELAYER_DEMO_TMUX_DIR="$RELAYER_DEMO_WORKDIR/tmux"; mkdir -m 700 "$RELAYER_DEMO_TMUX_DIR"; export TMUX_TMPDIR="$RELAYER_DEMO_TMUX_DIR"; unset TMUX TMUX_PANE; cleanup_relayer_demo() { tmux kill-server >/dev/null 2>&1 || true; rm -f "$RELAYER_DEMO_WORKDIR/relayer" "$RELAYER_DEMO_WORKDIR/config.yaml"; rmdir "$RELAYER_DEMO_TMUX_DIR/tmux-$(id -u)" "$RELAYER_DEMO_TMUX_DIR" "$RELAYER_DEMO_WORKDIR" 2>/dev/null || true; }; trap cleanup_relayer_demo EXIT HUP INT TERM; tmux -f /dev/null new-session -d -s relayer-demo-bootstrap 'sleep 600'`
Enter
Sleep 1s

Type `go build -o "$RELAYER_DEMO_WORKDIR/relayer" ./cmd/relayer`
Enter
Sleep 3s

# Derive a temporary tmux/no-audit config from the tracked, synthetic default.
Type `sed -e 's/^backend: pty$/backend: tmux/' -e 's/^  enabled: true$/  enabled: false/' config.yaml > "$RELAYER_DEMO_WORKDIR/config.yaml"`
Enter
Sleep 1s

Type `"$RELAYER_DEMO_WORKDIR/relayer" --config "$RELAYER_DEMO_WORKDIR/config.yaml"`
Enter
Sleep 500ms

# Make multi-agent selection visible, then return to the first agent. Focus
# movement is deliberately demonstrated before either mock reaches its prompt.
Ctrl+Right
Sleep 300ms
Ctrl+Left
Sleep 300ms

# Enter opens the focused agent's native tmux client.
Enter
Sleep 1s

# Default tmux detach binding. Custom user bindings can differ.
Ctrl+B
Type "d"
Sleep 2s

# Both mocks use the same public Overwrite prompt. Events remain distinct by
# session and occurrence ID; the supervisor activates them one at a time.
Type "Y"
Enter
Sleep 1s
Type "n"
Enter
Sleep 3s

Ctrl+C
Sleep 2s

Type "tmux has-session -t relayer-demo-bootstrap && echo 'Unowned tmux session preserved'; cleanup_relayer_demo; trap - EXIT HUP INT TERM"
Enter
Sleep 1s
