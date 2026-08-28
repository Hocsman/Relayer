# The short demo the README embeds: https://github.com/charmbracelet/vhs
#
# Run from the repository root with:
#   vhs docs/readme-demo.tape
#
# It records only Relayer's deterministic Bash mocks, so it calls no AI service
# and needs no credentials. Setup is hidden: a newcomer should see what
# supervising an agent looks like, not how the recording was staged.
#
# docs/demo.tape is the longer reproducible recording, covering the tmux backend
# and native attach.

Output docs/demo.gif

Require bash
Require go
Require mktemp

Set Shell "bash"
Set FontSize 16
Set Width 1200
Set Height 700
Set TypingSpeed 45ms
Set PlaybackSpeed 1

# Build into a private directory and start from a clean prompt. None of this is
# what the demo is about, so none of it is recorded.
Hide
Type `RELAYER_DEMO=$(mktemp -d /tmp/relayer-readme-demo.XXXXXX); chmod 700 "$RELAYER_DEMO"; go build -o "$RELAYER_DEMO/relayer" ./cmd/relayer; cp config.yaml "$RELAYER_DEMO/config.yaml"; cd "$RELAYER_DEMO"; clear`
Enter
Sleep 12s
Show

# Two synthetic agents, no API key, no network.
Type "./relayer"
Enter
Sleep 5s

# Both agents work, then both stop and ask the same question. Relayer holds
# them there: the supervisor line counts what is waiting.
Sleep 4s

# Answer the first one. The response is encoded by the adapter that produced
# the prompt and delivered to that exact occurrence.
Type "y"
Sleep 800ms
Enter
Sleep 2500ms

# The second agent is promoted automatically and now holds the field.
Type "y"
Sleep 800ms
Enter
Sleep 3s

Ctrl+C
Sleep 2s

Hide
Type `cd - >/dev/null; rm -rf "$RELAYER_DEMO"`
Enter
Sleep 1s
Show
