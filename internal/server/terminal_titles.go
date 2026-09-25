package server

import "strings"

// withoutHostReports removes from an agent's terminal output the control
// sequences that name the host rather than show anything: the window title
// (OSC 0, 1 and 2) and the current directory (OSC 7). Every client receives an
// agent's output, viewers included, and the web terminal displays neither.
// The ConPTY of every Windows session sets the window title to the agent's
// full path, the Windows user's name with it, so a viewer, who is given no
// host path anywhere else, read one in every snapshot.
//
// A sequence is removed up to its terminator, BEL or ESC \, and a sequence the
// bounded output ends inside of is removed to the end: the next snapshot
// carries it whole.
func withoutHostReports(output string) string {
	if !strings.Contains(output, "\x1b]") {
		return output
	}
	var kept strings.Builder
	kept.Grow(len(output))
	for {
		start := strings.Index(output, "\x1b]")
		if start < 0 {
			kept.WriteString(output)
			return kept.String()
		}
		body := output[start+2:]
		if !namesTheHost(body) {
			kept.WriteString(output[:start+2])
			output = body
			continue
		}
		kept.WriteString(output[:start])
		end := oscEnd(body)
		if end < 0 {
			return kept.String()
		}
		output = body[end:]
	}
}

// namesTheHost reports whether an OSC sequence's body is a window title or a
// current directory report.
func namesTheHost(body string) bool {
	for _, prefix := range []string{"0;", "1;", "2;", "7;"} {
		if strings.HasPrefix(body, prefix) {
			return true
		}
	}
	return false
}

// oscEnd is the length of body up to and including its terminator, or -1 when
// it has none.
func oscEnd(body string) int {
	for index := 0; index < len(body); index++ {
		switch body[index] {
		case '\a':
			return index + 1
		case 0x1b:
			if index+1 < len(body) && body[index+1] == '\\' {
				return index + 2
			}
		}
	}
	return -1
}
