package adapters

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Model Context Protocol tool calls are recognized by their naming convention
// rather than by the frame an agent happens to draw around them. Every MCP tool
// is addressed as mcp__<server>__<tool>, and that token is agent independent,
// while the surrounding layout differs for every CLI. No MCP-capable CLI is
// installed on the machine this parser was written on and no fixture in testdata
// captures one, so matching layout would be guesswork with a high false positive
// rate. The parser therefore anchors on the name and treats everything around it
// as best effort.
//
// Every shape recognized below is a hand-written heuristic. None of it is
// observed or verified against a real agent.
const (
	mcpToolPrefix    = "mcp__"
	mcpNameSeparator = "__"

	// maxToolCallParams and maxToolCallValueRunes bound what agent-controlled
	// terminal text can push into an event: a tool call is displayed, so an
	// unbounded argument would be an unbounded badge.
	maxToolCallParams     = 12
	maxToolCallValueRunes = 256
	maxToolCallNameRunes  = 64

	// maxToolCallNameBytes is the length limit of the name grammar, which is one
	// leading character plus at most 63 more.
	maxToolCallNameBytes = 64

	// maxToolCallScanBytes bounds the region searched for parameters. This runs
	// on every detected prompt block, so the work after the anchor stays
	// proportional to the badge rather than to the scrollback.
	maxToolCallScanBytes  = 8192
	maxToolCallParamLines = 32

	// maxToolCallCandidates bounds how many names one block is read for. Each
	// candidate costs a scan of the line it sits on, so an unbounded count is
	// quadratic in the block: a single line carrying eight thousand quoted names
	// took most of a second before this cap existed. The scan runs backwards from
	// the end, so the cap drops the names furthest from the prompt first.
	maxToolCallCandidates = 32

	// maxToolCallJSONLines is how far below the name an argument object is still
	// considered to belong to it: the same line, or one of the next two.
	maxToolCallJSONLines = 3
)

// highRiskToolKeywords and lowRiskToolKeywords classify a tool by name, and the
// two lists are matched differently on purpose.
//
// The high list is substring matched, so it over-reports: a tool named
// "monkey_patch" is high risk because it contains "key". The low list cannot be
// matched that way, because for it a substring under-reports: "spreadsheet"
// carries "read" and "forget_session" carries "get", and either would turn a
// tool nothing is known about into a low-risk one. The low list therefore
// matches whole tokens only.
var (
	highRiskToolKeywords = []string{
		"delete", "remove", "drop", "write", "create", "update", "edit",
		"exec", "run", "shell", "command", "push", "deploy", "publish",
		"send", "email", "payment", "transfer", "credential", "token",
		"secret", "key",
	}
	lowRiskToolKeywords = []string{
		"get", "list", "read", "search", "find", "fetch", "view",
		"describe", "status",
	}
)

// ToolCallParam is one named argument of an MCP tool call. Value is bounded
// and sanitised: it is agent-controlled terminal text.
type ToolCallParam struct {
	Name      string
	Value     string
	Truncated bool
}

// ToolCall is one MCP tool invocation read out of terminal text.
type ToolCall struct {
	Server string
	Tool   string
	Params []ToolCallParam
	// Risk is derived from the tool name alone, never from its arguments.
	Risk RiskLevel
	// ParamsTruncated reports that parameters were dropped past the cap.
	ParamsTruncated bool
}

// DetectToolCall finds at most one MCP tool call in a block of normalized
// terminal text. It returns false when the block carries none.
//
// Several names in one block resolve to the most dangerous of them, and among
// equals to the last one, which is the call closest to the prompt the operator
// is being asked about. Resolving to the last name alone handed the agent a
// downgrade, because a name printed inside an unquoted argument sits AFTER the
// name of the call being made: "mcp__fs__delete_file path=mcp__docs__get_page"
// was read as a page fetch. A second name can now only raise the risk of a
// block, never lower it, which is what keeps an argument from talking a call
// down.
//
// A name that only appears as documentation, inside a code fence or inside
// quotes, is not a call.
func DetectToolCall(block string) (ToolCall, bool) {
	var (
		best  ToolCall
		found bool
	)
	limit := len(block)
	for examined := 0; examined < maxToolCallCandidates && limit > 0; examined++ {
		offset := strings.LastIndex(block[:limit], mcpToolPrefix)
		if offset < 0 {
			break
		}
		limit = offset
		// A prefix glued to an identifier is part of that identifier, not the
		// start of a tool name.
		if offset > 0 && isToolCallNameByte(block[offset-1]) {
			continue
		}
		end := offset + len(mcpToolPrefix)
		for end < len(block) && isToolCallNameByte(block[end]) {
			end++
		}
		server, tool, ok := splitToolCallName(block[offset+len(mcpToolPrefix) : end])
		if !ok {
			// Half a name is not a tool call. Reporting a partial one would be
			// worse than reporting nothing.
			continue
		}
		lineStart := strings.LastIndexByte(block[:offset], '\n') + 1
		lineEnd := len(block)
		if newline := strings.IndexByte(block[end:], '\n'); newline >= 0 {
			lineEnd = end + newline
		}
		line := block[lineStart:lineEnd]
		if ignoredContext(line, fenceDepthBefore(block, lineStart, false)) {
			continue
		}
		if quotedMatch(line, offset-lineStart, end-lineStart) {
			continue
		}
		call := ToolCall{
			Server: server,
			Tool:   tool,
			Risk:   ToolCallRisk(server, tool),
		}
		if found && toolCallRiskRank(call.Risk) <= toolCallRiskRank(best.Risk) {
			continue
		}
		call.Params, call.ParamsTruncated = parseToolCallParams(block[end:])
		best, found = call, true
	}
	return best, found
}

// toolCallRiskRank orders the levels for the comparison above. Unknown outranks
// low because the policy engine refuses an automatic allow for unknown as
// readily as for high, so a name nothing is known about is the more cautious
// reading of a block that carries several.
func toolCallRiskRank(risk RiskLevel) int {
	switch risk {
	case RiskHigh:
		return 2
	case RiskUnknown:
		return 1
	default:
		return 0
	}
}

// ToolCallRisk classifies a tool by name only.
//
// The server participates in the high-risk check because a server named
// "payments" or "deploy" raises the stakes of everything it exposes. The
// low-risk check reads the tool alone, so a read-looking tool on such a server
// stays high rather than talking itself down.
func ToolCallRisk(server, tool string) RiskLevel {
	tool = strings.ToLower(strings.TrimSpace(tool))
	if tool == "" {
		return RiskUnknown
	}
	full := strings.ToLower(strings.TrimSpace(server)) + "_" + tool
	for _, keyword := range highRiskToolKeywords {
		if strings.Contains(full, keyword) {
			return RiskHigh
		}
	}
	for _, keyword := range lowRiskToolKeywords {
		if hasToolCallToken(tool, keyword) {
			return RiskLow
		}
	}
	return RiskUnknown
}

// hasToolCallToken reports whether name carries keyword as a whole token, the
// separators being the punctuation the name grammar allows.
func hasToolCallToken(name, keyword string) bool {
	separator := func(char rune) bool {
		return char == '_' || char == '.' || char == '-'
	}
	for _, token := range strings.FieldsFunc(name, separator) {
		if token == keyword {
			return true
		}
	}
	return false
}

// splitToolCallName separates <server>__<tool>. Both halves must match the
// grammar completely, so a name that was cut by a line break or glued to
// punctuation is rejected rather than half parsed.
func splitToolCallName(name string) (string, string, bool) {
	separator := strings.Index(name, mcpNameSeparator)
	if separator < 0 {
		return "", "", false
	}
	server := name[:separator]
	tool := name[separator+len(mcpNameSeparator):]
	if !validToolCallName(server) || !validToolCallName(tool) {
		return "", "", false
	}
	return server, tool, true
}

// validToolCallName implements ^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$ directly. The
// grammar is ASCII only, so a byte loop is exactly equivalent to a rune loop
// and avoids compiling a regexp on a hot path.
func validToolCallName(name string) bool {
	if name == "" || len(name) > maxToolCallNameBytes {
		return false
	}
	if !isToolCallAlnum(name[0]) {
		return false
	}
	for index := 1; index < len(name); index++ {
		if !isToolCallNameByte(name[index]) {
			return false
		}
	}
	return true
}

func isToolCallAlnum(char byte) bool {
	return (char >= 'a' && char <= 'z') ||
		(char >= 'A' && char <= 'Z') ||
		(char >= '0' && char <= '9')
}

func isToolCallNameByte(char byte) bool {
	return isToolCallAlnum(char) || char == '_' || char == '.' || char == '-'
}

// parseToolCallParams reads the arguments an agent is most likely to print near
// the name: a JSON object on the same or a following line, or key: value and
// key=value lines. It never fails: the tool call is the finding, the arguments
// are decoration.
func parseToolCallParams(rest string) ([]ToolCallParam, bool) {
	if len(rest) > maxToolCallScanBytes {
		rest = rest[:maxToolCallScanBytes]
	}
	if params, truncated, ok := toolCallJSONParams(rest); ok && len(params) > 0 {
		return params, truncated
	}
	return toolCallLineParams(rest)
}

// toolCallJSONParams decodes the first balanced object printed close enough to
// the name to belong to it. Decoding goes through encoding/json: the values are
// agent-controlled and a hand-rolled parser is exactly where that bites.
func toolCallJSONParams(rest string) ([]ToolCallParam, bool, bool) {
	region := firstToolCallLines(rest, maxToolCallJSONLines)
	start := strings.IndexByte(region, '{')
	if start < 0 {
		return nil, false, false
	}
	object, ok := balancedToolCallObject(region, start)
	if !ok {
		return nil, false, false
	}
	decoder := json.NewDecoder(strings.NewReader(object))
	if _, err := decoder.Token(); err != nil {
		return nil, false, false
	}
	params := make([]ToolCallParam, 0, maxToolCallParams)
	for decoder.More() {
		if len(params) == maxToolCallParams {
			return params, true, true
		}
		token, err := decoder.Token()
		if err != nil {
			return nil, false, false
		}
		key, isString := token.(string)
		if !isString {
			return nil, false, false
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, false, false
		}
		name, _ := sanitizeToolCallText(key, maxToolCallNameRunes)
		if name == "" {
			continue
		}
		value, truncated := sanitizeToolCallText(toolCallJSONValue(raw), maxToolCallValueRunes)
		params = append(params, ToolCallParam{Name: name, Value: value, Truncated: truncated})
	}
	return params, false, true
}

// toolCallJSONValue renders one argument for display. A JSON string is shown
// unquoted; anything else keeps its source text, which the sanitiser then
// compacts.
func toolCallJSONValue(raw json.RawMessage) string {
	text := string(raw)
	if strings.HasPrefix(text, `"`) {
		var decoded string
		if err := json.Unmarshal(raw, &decoded); err == nil {
			return decoded
		}
	}
	return text
}

// balancedToolCallObject returns the object starting at start, tracking string
// state so that a brace inside a quoted argument does not close it. Unbalanced
// input simply yields no object.
func balancedToolCallObject(region string, start int) (string, bool) {
	depth := 0
	inString := false
	escaped := false
	for index := start; index < len(region); index++ {
		char := region[index]
		if inString {
			switch {
			case escaped:
				escaped = false
			case char == '\\':
				escaped = true
			case char == '"':
				inString = false
			}
			continue
		}
		switch char {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return region[start : index+1], true
			}
		}
	}
	return "", false
}

// toolCallLineParams reads key: value and key=value lines below the name. It
// stops at the first line that carries text but is not a pair, because that
// line is prose and the parameter block has ended.
func toolCallLineParams(rest string) ([]ToolCallParam, bool) {
	var params []ToolCallParam
	lines := strings.Split(rest, "\n")
	if len(lines) > maxToolCallParamLines {
		lines = lines[:maxToolCallParamLines]
	}
	for _, line := range lines {
		line = trimToolCallDecoration(line)
		if !containsToolCallAlnum(line) {
			// Blank rows and frame borders sit between the name and its
			// arguments; they end nothing.
			continue
		}
		name, value, ok := splitToolCallPair(line)
		if !ok {
			break
		}
		if len(params) == maxToolCallParams {
			return params, true
		}
		sanitized, truncated := sanitizeToolCallText(value, maxToolCallValueRunes)
		params = append(params, ToolCallParam{Name: name, Value: sanitized, Truncated: truncated})
	}
	return params, false
}

func splitToolCallPair(line string) (string, string, bool) {
	separator := strings.IndexAny(line, ":=")
	if separator < 0 {
		return "", "", false
	}
	key := strings.TrimSpace(line[:separator])
	if !validToolCallName(key) {
		return "", "", false
	}
	name, _ := sanitizeToolCallText(key, maxToolCallNameRunes)
	if name == "" {
		return "", "", false
	}
	return name, line[separator+1:], true
}

// trimToolCallDecoration removes the frame and bullet characters agents draw to
// the left of an argument line.
func trimToolCallDecoration(line string) string {
	return strings.TrimLeft(line, " \t\r|│┃├└─╰╭·•*-()[]{},:")
}

func containsToolCallAlnum(line string) bool {
	for index := 0; index < len(line); index++ {
		if isToolCallAlnum(line[index]) {
			return true
		}
	}
	return false
}

func firstToolCallLines(text string, count int) string {
	end := 0
	for remaining := count; remaining > 0; remaining-- {
		newline := strings.IndexByte(text[end:], '\n')
		if newline < 0 {
			return text
		}
		end += newline + 1
	}
	return text[:end]
}

// sanitizeToolCallText bounds and cleans one agent-controlled fragment: escape
// sequences and control bytes are dropped, runs of whitespace collapse to a
// single space, and the result is capped in runes. The boolean reports that the
// cap cut something off.
func sanitizeToolCallText(value string, maxRunes int) (string, bool) {
	var builder strings.Builder
	runes := 0
	pendingSpace := false
	for index := 0; index < len(value); {
		char := value[index]
		if char == 0x1b {
			index = toolCallEscapeEnd(value, index)
			continue
		}
		if char < utf8.RuneSelf {
			if char <= ' ' || char == 0x7f {
				// Every control byte is dropped; the whitespace ones leave a
				// separator behind, and only if something has been kept already.
				if char == ' ' || (char >= '\t' && char <= '\r') {
					pendingSpace = builder.Len() > 0
				}
				index++
				continue
			}
			if pendingSpace {
				if runes+1 >= maxRunes {
					return builder.String(), true
				}
				builder.WriteByte(' ')
				runes++
				pendingSpace = false
			}
			if runes >= maxRunes {
				return builder.String(), true
			}
			builder.WriteByte(char)
			runes++
			index++
			continue
		}
		decoded, size := utf8.DecodeRuneInString(value[index:])
		if decoded == utf8.RuneError && size == 1 {
			// Binary output is not text; the byte is dropped rather than turned
			// into a replacement character.
			index++
			continue
		}
		if pendingSpace {
			if runes+1 >= maxRunes {
				return builder.String(), true
			}
			builder.WriteByte(' ')
			runes++
			pendingSpace = false
		}
		if runes >= maxRunes {
			return builder.String(), true
		}
		builder.WriteRune(decoded)
		runes++
		index += size
	}
	return builder.String(), false
}

// toolCallEscapeEnd returns the offset just past the escape sequence starting at
// start. Detection text is normally already stripped, so this is a second line
// of defence rather than the primary one.
func toolCallEscapeEnd(value string, start int) int {
	if start+1 >= len(value) {
		return len(value)
	}
	switch value[start+1] {
	case '[':
		for index := start + 2; index < len(value); index++ {
			if value[index] >= 0x40 && value[index] <= 0x7e {
				return index + 1
			}
		}
		return len(value)
	case ']':
		for index := start + 2; index < len(value); index++ {
			if value[index] == 0x07 {
				return index + 1
			}
			if value[index] == 0x1b && index+1 < len(value) && value[index+1] == '\\' {
				return index + 2
			}
		}
		return len(value)
	default:
		return start + 2
	}
}

// attachToolCall enriches freshly detected occurrences with the MCP tool call
// their prompt block is about, when one can be read out of it.
//
// It runs in the processor rather than in each adapter because a tool call is
// not an adapter's concern: every agent that speaks MCP prints the same
// tool-name convention, and duplicating the parse across six adapters would
// mean six places to get the risk rule wrong.
//
// Sensitive occurrences are skipped. A credential prompt's surrounding text is
// exactly what must not be parsed, retained or shown, and a badge built from it
// would put that content on screen next to the masked field that was hiding it.
func attachToolCall(events []Event, state *DetectionState) {
	if state == nil || state.detectionText == "" {
		return
	}

	var (
		call   ToolCall
		parsed bool
		done   bool
	)
	for index := range events {
		if events[index].Sensitive || !events[index].Actionable() {
			continue
		}
		if !done {
			call, parsed = DetectToolCall(state.detectionText)
			done = true
		}
		if !parsed {
			return
		}
		attached := call
		attached.Params = append([]ToolCallParam(nil), call.Params...)
		events[index].ToolCall = &attached

		// The identity reaches the audit journal; the arguments never do.
		// allowedMetadataKey in internal/audit admits exactly these three keys
		// and drops anything else, so a value added here without a matching
		// rule there would be silently discarded rather than leak.
		if events[index].Metadata == nil {
			events[index].Metadata = make(map[string]string, 3)
		}
		events[index].Metadata["mcp_server"] = call.Server
		events[index].Metadata["mcp_tool"] = call.Tool
		events[index].Metadata["mcp_params"] = strconv.Itoa(len(call.Params))
	}
}
