package tmuxbackend

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode"
)

const (
	namePrefix        = "relayer"
	maximumSlugLength = 24
)

// SessionName returns a stable name inside one Relayer run. The run component
// isolates concurrent Relayer processes, while the hash prevents slug
// collisions such as "agent a" and "agent-a".
func SessionName(runID, agentID string) string {
	run := slug(runID, 16)
	if run == "" {
		run = shortHash(runID)
	}
	agentSlug := slug(agentID, maximumSlugLength)
	if agentSlug == "" {
		agentSlug = "agent"
	}
	return strings.Join([]string{namePrefix, run, agentSlug, shortHash(agentID)}, "-")
}

func newRunID() (string, error) {
	bytes := make([]byte, 8)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

func slug(value string, limit int) string {
	var result strings.Builder
	separator := false
	for _, character := range strings.ToLower(strings.TrimSpace(value)) {
		if unicode.IsLetter(character) || unicode.IsDigit(character) {
			if separator && result.Len() > 0 && result.Len() < limit {
				result.WriteByte('-')
			}
			separator = false
			if character <= unicode.MaxASCII && result.Len() < limit {
				result.WriteRune(character)
			}
			continue
		}
		separator = result.Len() > 0
	}
	return strings.Trim(result.String(), "-")
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:4])
}
