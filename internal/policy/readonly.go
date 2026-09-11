package policy

import (
	"path/filepath"
	"strings"
)

var (
	safePipeCommands = map[string]bool{
		"grep":    true,
		"egrep":   true,
		"fgrep":   true,
		"findstr": true,
		"head":    true,
		"tail":    true,
		"wc":      true,
		"cat":     true,
		"sort":    true,
		"uniq":    true,
		"more":    true,
		"less":    true,
		"column":  true,
		"cut":     true,
		"tr":      true,
		"jq":      true,
	}

	dangerousPipeTargets = map[string]bool{
		"sh":         true,
		"bash":       true,
		"zsh":        true,
		"ksh":        true,
		"csh":        true,
		"cmd":        true,
		"cmd.exe":    true,
		"powershell": true,
		"pwsh":       true,
		"python":     true,
		"python3":    true,
		"perl":       true,
		"ruby":       true,
		"node":       true,
		"tee":        true,
		"dd":         true,
		"xargs":      true,
		"awk":        true,
		"sed":        true,
	}

	safeGitSubcommands = map[string]bool{
		"status":       true,
		"diff":         true,
		"log":          true,
		"show":         true,
		"branch":       true,
		"tag":          true,
		"rev-parse":    true,
		"ls-files":     true,
		"describe":     true,
		"remote":       true,
		"stash":        true,
		"check-ignore": true,
	}

	safeInspectCommands = map[string]bool{
		"ls":       true,
		"dir":      true,
		"cat":      true,
		"head":     true,
		"tail":     true,
		"more":     true,
		"less":     true,
		"pwd":      true,
		"echo":     true,
		"which":    true,
		"where":    true,
		"type":     true,
		"file":     true,
		"stat":     true,
		"wc":       true,
		"uname":    true,
		"whoami":   true,
		"id":       true,
		"printenv": true,
		"findstr":  true,
		"grep":     true,
		"egrep":    true,
		"fgrep":    true,
		"rg":       true,
	}
)

// IsReadOnlyCommand determines if a shell command line is guaranteed to be a read-only
// query or test execution that does not modify the filesystem or execute arbitrary code.
func IsReadOnlyCommand(command string) bool {
	command = strings.TrimSpace(command)
	if command == "" {
		return false
	}

	// 1. Check for write redirections: >, >>, &>, 1>, 2>
	if hasWriteRedirection(command) {
		return false
	}

	// 2. Check for sequential/logical chaining: ;, &&, ||
	// Must handle quotes when checking for operators
	if subcommands, chained := splitChainedCommands(command); chained {
		if len(subcommands) == 0 {
			return false
		}
		for _, sub := range subcommands {
			if !IsReadOnlyCommand(sub) {
				return false
			}
		}
		return true
	}

	// 3. Check for pipeline: |
	if pipeStages, isPiped := splitPipedCommands(command); isPiped {
		if len(pipeStages) < 2 {
			return false
		}
		// First stage must be read-only
		if !IsReadOnlyCommand(pipeStages[0]) {
			return false
		}
		// Subsequent stages must be safe filters
		for _, stage := range pipeStages[1:] {
			if !isSafePipeStage(stage) {
				return false
			}
		}
		return true
	}

	// 4. Evaluate single atomic command
	return isAtomicReadOnlyCommand(command)
}

func hasWriteRedirection(command string) bool {
	tokens := tokenizeArguments(command)
	for _, tok := range tokens {
		if tok == ">" || tok == ">>" || tok == "&>" || tok == "1>" || tok == "2>" ||
			tok == ">&" || tok == ">|" {
			return true
		}
		if strings.HasPrefix(tok, ">") || strings.HasPrefix(tok, ">>") ||
			strings.HasPrefix(tok, "1>") || strings.HasPrefix(tok, "2>") {
			return true
		}
	}
	return false
}

func splitChainedCommands(command string) ([]string, bool) {
	// Look for ;, &&, || outside of quotes
	var parts []string
	var current strings.Builder
	inSingle := false
	inDouble := false
	escaped := false
	runes := []rune(command)
	chained := false

	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if escaped {
			current.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' && !inSingle {
			escaped = true
			current.WriteRune(r)
			continue
		}
		if r == '\'' && !inDouble {
			inSingle = !inSingle
			current.WriteRune(r)
			continue
		}
		if r == '"' && !inSingle {
			inDouble = !inDouble
			current.WriteRune(r)
			continue
		}
		if !inSingle && !inDouble {
			if r == ';' {
				chained = true
				if trimmed := strings.TrimSpace(current.String()); trimmed != "" {
					parts = append(parts, trimmed)
				}
				current.Reset()
				continue
			}
			if i+1 < len(runes) && ((r == '&' && runes[i+1] == '&') || (r == '|' && runes[i+1] == '|')) {
				chained = true
				if trimmed := strings.TrimSpace(current.String()); trimmed != "" {
					parts = append(parts, trimmed)
				}
				current.Reset()
				i++ // skip second char
				continue
			}
		}
		current.WriteRune(r)
	}
	if chained {
		if trimmed := strings.TrimSpace(current.String()); trimmed != "" {
			parts = append(parts, trimmed)
		}
		return parts, true
	}
	return nil, false
}

func splitPipedCommands(command string) ([]string, bool) {
	var parts []string
	var current strings.Builder
	inSingle := false
	inDouble := false
	escaped := false
	runes := []rune(command)
	isPiped := false

	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if escaped {
			current.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' && !inSingle {
			escaped = true
			current.WriteRune(r)
			continue
		}
		if r == '\'' && !inDouble {
			inSingle = !inSingle
			current.WriteRune(r)
			continue
		}
		if r == '"' && !inSingle {
			inDouble = !inDouble
			current.WriteRune(r)
			continue
		}
		if !inSingle && !inDouble {
			if r == '|' {
				// Make sure it's not ||
				if i+1 < len(runes) && runes[i+1] == '|' {
					// handled by chained commands
					return nil, false
				}
				isPiped = true
				if trimmed := strings.TrimSpace(current.String()); trimmed != "" {
					parts = append(parts, trimmed)
				}
				current.Reset()
				continue
			}
		}
		current.WriteRune(r)
	}
	if isPiped {
		if trimmed := strings.TrimSpace(current.String()); trimmed != "" {
			parts = append(parts, trimmed)
		}
		return parts, true
	}
	return nil, false
}

func isSafePipeStage(stage string) bool {
	tokens := tokenizeArguments(strings.TrimSpace(stage))
	if len(tokens) == 0 {
		return false
	}
	prog := strings.ToLower(filepath.Base(tokens[0]))
	if dangerousPipeTargets[prog] {
		return false
	}
	return safePipeCommands[prog]
}

func isAtomicReadOnlyCommand(command string) bool {
	tokens := tokenizeArguments(strings.TrimSpace(command))
	if len(tokens) == 0 {
		return false
	}

	// Strip wrapper prefixes like sudo, env FOO=BAR, etc.
	idx := 0
	for idx < len(tokens) {
		tok := strings.ToLower(tokens[idx])
		if tok == "sudo" {
			idx++
			continue
		}
		if tok == "env" {
			idx++
			// skip environment assignments FOO=BAR
			for idx < len(tokens) && strings.Contains(tokens[idx], "=") && !strings.HasPrefix(tokens[idx], "-") {
				idx++
			}
			continue
		}
		break
	}
	if idx >= len(tokens) {
		return false
	}

	baseTokens := tokens[idx:]
	prog := strings.ToLower(filepath.Base(baseTokens[0]))
	prog = strings.TrimSuffix(prog, ".exe")

	// 1. Git subcommands
	if prog == "git" {
		return isSafeGitCommand(baseTokens[1:])
	}

	// 2. Safe inspection commands
	if safeInspectCommands[prog] {
		return true
	}

	// 3. Find command: check for -delete or -exec
	if prog == "find" {
		for _, arg := range baseTokens[1:] {
			argLower := strings.ToLower(arg)
			if argLower == "-delete" || argLower == "-exec" || argLower == "-execdir" {
				return false
			}
		}
		return true
	}

	// 4. Test and lint runners
	if isSafeTestOrLintCommand(prog, baseTokens) {
		return true
	}

	return false
}

func isSafeGitCommand(args []string) bool {
	if len(args) == 0 {
		return false
	}
	// Skip global git flags like -C dir, --no-pager
	i := 0
	for i < len(args) {
		arg := args[i]
		if arg == "-C" && i+1 < len(args) {
			i += 2
			continue
		}
		if strings.HasPrefix(arg, "--no-pager") || strings.HasPrefix(arg, "--git-dir") || strings.HasPrefix(arg, "--work-tree") {
			i++
			continue
		}
		if strings.HasPrefix(arg, "-") {
			i++
			continue
		}
		break
	}
	if i >= len(args) {
		return false
	}

	sub := strings.ToLower(args[i])
	if !safeGitSubcommands[sub] {
		return false
	}

	// Special check for mutating flags in subcommands
	if sub == "branch" {
		for _, arg := range args[i+1:] {
			if strings.HasPrefix(arg, "-d") || strings.HasPrefix(arg, "-D") ||
				strings.HasPrefix(arg, "-m") || strings.HasPrefix(arg, "-M") ||
				arg == "--delete" || arg == "--move" {
				return false
			}
		}
	}
	if sub == "tag" {
		for _, arg := range args[i+1:] {
			if strings.HasPrefix(arg, "-d") || arg == "--delete" {
				return false
			}
		}
	}
	if sub == "remote" {
		// Only "remote -v" or "remote show ..."
		for _, arg := range args[i+1:] {
			argLower := strings.ToLower(arg)
			if argLower == "add" || argLower == "remove" || argLower == "rm" ||
				argLower == "set-url" || argLower == "rename" {
				return false
			}
		}
	}
	if sub == "stash" {
		// Only "stash list" or "stash show"
		if len(args) > i+1 {
			stashSub := strings.ToLower(args[i+1])
			if stashSub != "list" && stashSub != "show" {
				return false
			}
		}
	}

	return true
}

func isSafeTestOrLintCommand(prog string, tokens []string) bool {
	// go test, go vet
	if prog == "go" && len(tokens) >= 2 {
		sub := strings.ToLower(tokens[1])
		if sub == "vet" || sub == "doc" {
			return true
		}
		if sub == "test" {
			// Check for binary writing flags -o or -c
			for _, arg := range tokens[2:] {
				if arg == "-c" || arg == "-o" || strings.HasPrefix(arg, "-o=") {
					return false
				}
			}
			return true
		}
		return false
	}

	// npm / pnpm / yarn
	if prog == "npm" || prog == "pnpm" || prog == "yarn" {
		if len(tokens) < 2 {
			return false
		}
		// npm test, npm run test, yarn test, pnpm test
		joined := strings.ToLower(strings.Join(tokens[1:], " "))
		if strings.HasPrefix(joined, "test") || strings.HasPrefix(joined, "run test") ||
			strings.HasPrefix(joined, "run lint") || strings.HasPrefix(joined, "lint") {
			// Reject write/fix flags
			for _, arg := range tokens[2:] {
				argLower := strings.ToLower(arg)
				if argLower == "--fix" || argLower == "-u" || argLower == "--update-snapshots" ||
					argLower == "--write" || argLower == "-w" {
					return false
				}
			}
			return true
		}
		return false
	}

	// pytest, cargo test, cargo check
	if prog == "pytest" {
		return true
	}
	if prog == "cargo" && len(tokens) >= 2 {
		sub := strings.ToLower(tokens[1])
		if sub == "test" || sub == "check" {
			return true
		}
		return false
	}

	// python -m unittest / pytest
	if (prog == "python" || prog == "python3") && len(tokens) >= 3 {
		if tokens[1] == "-m" {
			mod := strings.ToLower(tokens[2])
			if mod == "unittest" || mod == "pytest" {
				return true
			}
		}
	}

	return false
}
