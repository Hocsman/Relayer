package fixturecapture

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Hocsman/Relayer/internal/audit"
)

var (
	emailPattern       = regexp.MustCompile(`(?i)\b[a-z0-9.!#$%&'*+/=?^_` + "`" + `{|}~-]+@[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+\b`)
	unixHomePattern    = regexp.MustCompile(`(?:/(?:Users|home)/[^/\s\x00-\x1f]+|/root)([/\s]|$)`)
	windowsHomePattern = regexp.MustCompile(`(?i)[a-z]:\\Users\\[^\\/\s\x00-\x1f]+([\\/\s]|$)`)
	privateKeyPattern  = regexp.MustCompile(`(?i)-----BEGIN (?:RSA |EC |DSA |OPENSSH )?PRIVATE KEY-----`)
	awsKeyPattern      = regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`)
	slackTokenPattern  = regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`)
)

// Anonymizer centralizes the only transformation permitted before fixture
// persistence. Credential-shaped content is rejected; identity-bearing email
// addresses and home-directory prefixes are replaced with stable markers.
type Anonymizer struct {
	homePaths []string
}

// NewDefaultAnonymizer includes the current user's home path without exposing
// it in the resulting value or artifact.
func NewDefaultAnonymizer() (*Anonymizer, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory for fixture anonymization: %w", err)
	}
	return NewAnonymizer([]string{home})
}

// NewAnonymizer defensively copies explicit home-directory prefixes. It is
// exported so tests and offline fixture validators do not depend on host data.
func NewAnonymizer(homePaths []string) (*Anonymizer, error) {
	result := &Anonymizer{}
	seen := make(map[string]struct{}, len(homePaths))
	for index, path := range homePaths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if strings.IndexByte(path, 0) >= 0 {
			return nil, fmt.Errorf("home path %d contains a NUL byte", index)
		}
		if _, exists := seen[path]; exists {
			continue
		}
		seen[path] = struct{}{}
		result.homePaths = append(result.homePaths, path)
	}
	sort.Slice(result.homePaths, func(left, right int) bool {
		return len(result.homePaths[left]) > len(result.homePaths[right])
	})
	return result, nil
}

// Anonymize returns independent storage safe for a fixture, or fails closed
// when a token, JWT, authorization value, API key, URL credential, or another
// credential-shaped value is observed. It never returns partially sanitized
// bytes together with an error.
func (anonymizer *Anonymizer) Anonymize(input []byte) ([]byte, error) {
	if anonymizer == nil {
		return nil, errors.New("fixture anonymizer must not be nil")
	}
	if !utf8.Valid(input) {
		return nil, errors.New("terminal output is not valid UTF-8")
	}
	// Detection adapters consume normalized terminal text. Strip ANSI before
	// every privacy check too, otherwise a color escape inserted inside a token
	// label, JWT, email, or path could bypass a text-level redactor.
	escapeFree := stripTerminalEscapes(string(input))
	collapsed := strings.Map(func(character rune) rune {
		if unicode.IsControl(character) {
			return -1
		}
		return character
	}, escapeFree)
	raw := normalizeTerminalText(escapeFree)
	if containsCredential(escapeFree) || containsCredential(collapsed) || containsCredential(raw) {
		return nil, ErrSensitiveContent
	}

	sanitized := raw
	for _, home := range anonymizer.homePaths {
		sanitized = strings.ReplaceAll(sanitized, home, "[HOME]")
		// Terminal tools sometimes render Windows paths with the opposite slash.
		if strings.Contains(home, `\`) {
			sanitized = strings.ReplaceAll(sanitized, strings.ReplaceAll(home, `\`, `/`), "[HOME]")
		}
	}
	sanitized = unixHomePattern.ReplaceAllString(sanitized, "[HOME]$1")
	sanitized = windowsHomePattern.ReplaceAllString(sanitized, "[HOME]$1")
	sanitized = emailPattern.ReplaceAllString(sanitized, "[EMAIL]")

	// Validate the final representation too. This guards transformations from
	// ever turning adjacent fragments into credential-shaped content.
	if containsCredential(sanitized) {
		return nil, ErrSensitiveContent
	}
	return bytes.Clone([]byte(sanitized)), nil
}

func containsCredential(value string) bool {
	return audit.Redact(value) != value || privateKeyPattern.MatchString(value) || awsKeyPattern.MatchString(value) || slackTokenPattern.MatchString(value)
}

func stripTerminalEscapes(value string) string {
	input := []byte(value)
	var output bytes.Buffer
	for index := 0; index < len(input); {
		if input[index] != 0x1b {
			output.WriteByte(input[index])
			index++
			continue
		}
		index++
		if index >= len(input) {
			break
		}
		switch input[index] {
		case '[':
			index++
			for index < len(input) {
				character := input[index]
				index++
				if character >= 0x40 && character <= 0x7e {
					break
				}
			}
		case ']', 'P', 'X', '^', '_':
			index++
			for index < len(input) {
				if input[index] == 0x07 {
					index++
					break
				}
				if input[index] == 0x1b && index+1 < len(input) && input[index+1] == '\\' {
					index += 2
					break
				}
				index++
			}
		default:
			// A two-byte escape has one final byte.
			index++
		}
	}
	return output.String()
}

func normalizeTerminalText(value string) string {
	var (
		output strings.Builder
		line   []rune
		cursor int
	)
	flush := func(newline bool) {
		output.WriteString(string(line))
		if newline {
			output.WriteByte('\n')
		}
		line = line[:0]
		cursor = 0
	}
	for _, character := range value {
		switch character {
		case '\r':
			cursor = 0
		case '\n':
			flush(true)
		case '\b':
			if cursor > 0 {
				cursor--
			}
		default:
			if unicode.IsControl(character) && character != '\t' {
				continue
			}
			if cursor < len(line) {
				line[cursor] = character
			} else {
				line = append(line, character)
			}
			cursor++
		}
	}
	flush(false)
	return output.String()
}

func (anonymizer *Anonymizer) fixture(tool, adapter string, backend Backend, outcome Outcome, exitCode *int, truncated bool, raw []byte) (Fixture, error) {
	sanitized, err := anonymizer.Anonymize(raw)
	if err != nil {
		return Fixture{}, err
	}
	fixture := Fixture{
		SchemaVersion: SchemaVersion,
		Tool:          tool,
		Adapter:       adapter,
		Backend:       backend,
		Outcome:       outcome,
		Truncated:     truncated,
	}
	if exitCode != nil {
		code := *exitCode
		fixture.ExitCode = &code
	}
	for sequence, offset := 0, 0; offset < len(sanitized); sequence++ {
		end := min(offset+artifactChunkBytes, len(sanitized))
		for end < len(sanitized) && end > offset && sanitized[end]&0xc0 == 0x80 {
			end--
		}
		fixture.Chunks = append(fixture.Chunks, Chunk{Sequence: sequence, Data: string(sanitized[offset:end])})
		offset = end
	}
	if fixture.Chunks == nil {
		fixture.Chunks = []Chunk{}
	}
	return fixture, nil
}
