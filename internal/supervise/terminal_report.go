package supervise

// IsTerminalReport reports whether data is nothing but replies a terminal
// sends by itself, never keys a person pressed: focus reports (CSI I and
// CSI O, sent while the program asked for them with DECSET 1004), cursor
// position reports (CSI row ; column R, the answer to CSI 6 n), device status
// reports (CSI 0 n) and device attributes (CSI ? … c and CSI > … c, the
// answers to CSI c). None of them can answer a question: an agent reads them
// as the terminal's, not as the person's. Anything else, a key, a paste, a
// mouse report, is typing, and an empty write is not a report.
//
// The gateway admits such a write with AdmitReport rather than Admit, so a
// browser terminal that reports its focus on its own does not make the
// prompts it shows the terminal's.
func IsTerminalReport(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	for len(data) > 0 {
		size := terminalReportLength(data)
		if size == 0 {
			return false
		}
		data = data[size:]
	}
	return true
}

// terminalReportLength is the length of the report data begins with, or 0
// when it begins with anything else.
func terminalReportLength(data []byte) int {
	if len(data) < 3 || data[0] != 0x1b || data[1] != '[' {
		return 0
	}
	body := data[2:]
	switch body[0] {
	case 'I', 'O':
		// Focus in, focus out.
		return 3
	case '?', '>':
		// Device attributes: CSI ? digits [; digits]… c, CSI > … c.
		if size := parameters(body[1:]); size > 0 && size+1 < len(body) && body[size+1] == 'c' {
			return 2 + size + 2
		}
		return 0
	}
	size := parameters(body)
	if size == 0 || size >= len(body) {
		return 0
	}
	switch body[size] {
	case 'R':
		// Cursor position: CSI row ; column R.
		return 2 + size + 1
	case 'n':
		// Device status: CSI 0 n, "no malfunction".
		if string(body[:size]) == "0" {
			return 2 + size + 1
		}
	}
	return 0
}

// parameters is the length of the run of digits and semicolons data begins
// with, bounded so a flood of digits is not a report.
func parameters(data []byte) int {
	size := 0
	for size < len(data) && size < 32 && (data[size] == ';' || (data[size] >= '0' && data[size] <= '9')) {
		size++
	}
	if size == 32 {
		return 0
	}
	return size
}
