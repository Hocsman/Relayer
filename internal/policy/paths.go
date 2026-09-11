package policy

import (
	"path/filepath"
	"regexp"
	"strings"
)

const (
	ReasonSensitivePath    = "sensitive_path_blocked"
	ReasonOutsideWorkspace = "outside_workspace_blocked"
)

var (
	sensitiveBasenameRegex  = regexp.MustCompile(`(?i)^(\.env(\.[a-z0-9_-]+)?|\.envrc|id_rsa.*|id_ed25519.*|id_ecdsa.*|id_dsa.*|authorized_keys|known_hosts|credentials|service-account.*\.json|client_secret.*\.json)$`)
	sensitivePathSubstrings = []string{
		"/.ssh/",
		"\\.ssh\\",
		"/.aws/",
		"\\.aws\\",
		"/.azure/",
		"\\.azure\\",
		"/.kube/",
		"\\.kube\\",
		"/.git/",
		"\\.git\\",
		"/etc/shadow",
		"/etc/sudoers",
		"/etc/ssl/private",
		"\\system32\\config\\sam",
		"\\system32\\config\\system",
		"/system32/config/sam",
		"/system32/config/system",
	}

	sensitiveTextRegex = regexp.MustCompile(`(?i)(\.env(\.[a-z0-9_-]+)?|\.envrc|\.ssh/(id_|authorized_keys)|id_rsa|id_ed25519|\.aws/(credentials|config)|\.kube/config|/etc/shadow|/etc/sudoers|\.git/config)`)
)

// IsSensitivePath returns true if the specified file or directory path matches
// known sensitive security credentials, private keys, environment secrets,
// or critical system files.
func IsSensitivePath(path string) bool {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return false
	}

	// Normalize separators for consistent pattern checking
	cleaned := filepath.Clean(trimmed)
	norm := filepath.ToSlash(cleaned)
	normLower := strings.ToLower(norm)

	// Check basename
	base := filepath.Base(cleaned)
	if sensitiveBasenameRegex.MatchString(base) {
		return true
	}

	// Add leading slash for substring matching if missing
	prefixed := "/" + strings.TrimPrefix(normLower, "/")

	// Check sensitive path substrings
	for _, sub := range sensitivePathSubstrings {
		subNorm := strings.ToLower(filepath.ToSlash(sub))
		if strings.Contains(prefixed, subNorm) {
			return true
		}
	}

	// Check if path ends with sensitive directories or files
	if strings.HasSuffix(prefixed, "/.ssh") || strings.HasSuffix(prefixed, "/.aws") ||
		strings.HasSuffix(prefixed, "/.kube") || strings.HasSuffix(prefixed, "/.azure") ||
		strings.HasSuffix(prefixed, "/.git/config") || prefixed == "/.git/config" {
		return true
	}

	return false
}

// ContainsSensitivePathReference returns true if the raw text contains explicit references
// to known sensitive filenames or paths.
func ContainsSensitivePathReference(text string) bool {
	return sensitiveTextRegex.MatchString(text)
}

// isRootedPath returns true if p is an absolute path or rooted path (starts with / or \).
func isRootedPath(p string) bool {
	if filepath.IsAbs(p) {
		return true
	}
	if len(p) > 0 && (p[0] == '/' || p[0] == '\\') {
		return true
	}
	return false
}

// IsPathInsideWorkspace determines whether candidatePath resides within workspaceRoot.
// Returns false if candidatePath attempts to escape workspaceRoot (via ../ or outside absolute paths).
func IsPathInsideWorkspace(candidatePath string, workspaceRoot string) bool {
	workspaceRoot = strings.TrimSpace(workspaceRoot)
	if workspaceRoot == "" {
		return true
	}
	candidatePath = strings.TrimSpace(candidatePath)
	if candidatePath == "" {
		return true
	}

	cleanWorkspace := filepath.Clean(workspaceRoot)
	cleanCandidate := filepath.Clean(candidatePath)

	var resolvedCandidate string
	if isRootedPath(cleanCandidate) {
		// If workspace has a volume name on Windows and candidate does not (or vice-versa)
		wsVol := filepath.VolumeName(cleanWorkspace)
		candVol := filepath.VolumeName(cleanCandidate)
		if candVol == "" && wsVol != "" {
			resolvedCandidate = wsVol + cleanCandidate
		} else {
			resolvedCandidate = cleanCandidate
		}
	} else {
		resolvedCandidate = filepath.Join(cleanWorkspace, cleanCandidate)
	}

	rel, err := filepath.Rel(cleanWorkspace, resolvedCandidate)
	if err != nil {
		// Incompatible drives or roots
		return false
	}

	// If relative path starts with ".." or is "..", it is outside the workspace
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || strings.HasPrefix(rel, "../") {
		return false
	}

	return true
}

// ExtractPaths extracts candidate file or directory paths from a shell command line or text.
func ExtractPaths(text string) []string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}

	var candidates []string
	tokens := tokenizeArguments(trimmed)

	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]

		// Handle flags with attached value: --file=path, -f=path, --output=path
		if eqIdx := strings.IndexByte(tok, '='); eqIdx > 0 && strings.HasPrefix(tok, "-") {
			val := tok[eqIdx+1:]
			val = strings.Trim(val, `"'`)
			if isCandidatePath(val) {
				candidates = append(candidates, val)
			}
			continue
		}

		// Handle redirection tokens: >, >>, <
		if tok == ">" || tok == ">>" || tok == "<" || tok == "1>" || tok == "2>" {
			if i+1 < len(tokens) {
				next := strings.Trim(tokens[i+1], `"'`)
				if next != "" {
					candidates = append(candidates, next)
				}
				i++
			}
			continue
		}

		// Strip leading redirection if joined: >file or <file
		if strings.HasPrefix(tok, ">>") {
			val := strings.Trim(tok[2:], `"' `)
			if val != "" {
				candidates = append(candidates, val)
			}
			continue
		}
		if strings.HasPrefix(tok, ">") || strings.HasPrefix(tok, "<") {
			val := strings.Trim(tok[1:], `"' `)
			if val != "" {
				candidates = append(candidates, val)
			}
			continue
		}

		// Handle flags with separate value: -f path, -o path, -c path, --config path, --file path
		if (tok == "-f" || tok == "-o" || tok == "-c" || tok == "--file" || tok == "--output" || tok == "--config") && i+1 < len(tokens) {
			next := strings.Trim(tokens[i+1], `"'`)
			if isCandidatePath(next) {
				candidates = append(candidates, next)
			}
			i++
			continue
		}

		// Skip normal flags (-a, --verbose)
		if strings.HasPrefix(tok, "-") {
			continue
		}

		val := strings.Trim(tok, `"'`)
		if isCandidatePath(val) {
			candidates = append(candidates, val)
		}
	}

	return deduplicatePaths(candidates)
}

func isCandidatePath(token string) bool {
	if token == "" || token == "." || token == "|" || token == "||" || token == "&&" || token == ";" {
		return false
	}

	// Always treat known sensitive tokens as candidate paths
	if IsSensitivePath(token) {
		return true
	}

	// Contains path separator
	if strings.ContainsAny(token, `/\`) {
		return true
	}

	// Starts with ~ or .
	if strings.HasPrefix(token, "~") || strings.HasPrefix(token, "..") || strings.HasPrefix(token, "./") {
		return true
	}

	// Windows drive letter C:
	if len(token) >= 2 && token[1] == ':' {
		return true
	}

	// File with known extension
	ext := filepath.Ext(token)
	if len(ext) > 1 && len(ext) <= 8 && !strings.ContainsAny(ext, `*?[]`) {
		return true
	}

	return false
}

func tokenizeArguments(command string) []string {
	var tokens []string
	var current strings.Builder
	inSingle := false
	inDouble := false
	escaped := false

	for _, r := range command {
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
		if (r == ' ' || r == '\t' || r == '\n' || r == '\r') && !inSingle && !inDouble {
			if current.Len() > 0 {
				tokens = append(tokens, current.String())
				current.Reset()
			}
			continue
		}
		current.WriteRune(r)
	}
	if current.Len() > 0 {
		tokens = append(tokens, current.String())
	}
	return tokens
}

func deduplicatePaths(paths []string) []string {
	seen := make(map[string]bool, len(paths))
	var result []string
	for _, p := range paths {
		cleaned := filepath.Clean(p)
		if !seen[cleaned] {
			seen[cleaned] = true
			result = append(result, cleaned)
		}
	}
	return result
}
