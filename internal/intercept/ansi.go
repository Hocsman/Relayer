package intercept

import "strings"

const maxANSICarrySize = 4 * 1024

// splitIncompleteANSI retains only a trailing, incomplete CSI/OSC sequence.
// Complete escape sequences remain in complete and are removed by stripansi.
func splitIncompleteANSI(input string) (complete string, carry string) {
	for offset := 0; offset < len(input); {
		if input[offset] != 0x1b {
			offset++
			continue
		}

		end, ok := ansiSequenceEnd(input, offset)
		if !ok {
			return input[:offset], input[offset:]
		}
		offset = end
	}
	return input, ""
}

func ansiSequenceEnd(input string, start int) (int, bool) {
	if start+1 >= len(input) {
		return 0, false
	}

	switch input[start+1] {
	case '[': // Control Sequence Introducer (CSI)
		for index := start + 2; index < len(input); index++ {
			if input[index] >= 0x40 && input[index] <= 0x7e {
				return index + 1, true
			}
		}
		return 0, false
	case ']': // Operating System Command (OSC), terminated by BEL or ST.
		for index := start + 2; index < len(input); index++ {
			if input[index] == 0x07 {
				return index + 1, true
			}
			if input[index] == 0x1b {
				if index+1 >= len(input) {
					return 0, false
				}
				if input[index+1] == '\\' {
					return index + 2, true
				}
			}
		}
		return 0, false
	default:
		// Most non-CSI escapes are two-byte sequences.
		return start + 2, true
	}
}

// sanitizeTerminalText neutralizes controls that could move the application's
// cursor. Carriage returns used by progress bars become line breaks.
func sanitizeTerminalText(input string) string {
	input = strings.ReplaceAll(input, "\r\n", "\n")
	input = strings.ReplaceAll(input, "\r", "\n")
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || (r >= 0x20 && (r < 0x7f || r >= 0xa0)) {
			return r
		}
		return -1
	}, input)
}
