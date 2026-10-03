package adapters

import (
	"fmt"
	"regexp"
	"strings"
)

// VerifiedVersions defines the list of tool versions empirically verified for each vendor adapter.
// Running another version might lead to unhandled prompts or changed interaction patterns.
var VerifiedVersions = map[string][]string{
	AiderID:           {"0.86.2"},
	ClaudeID:          {"2.1.286", "2.1.285", "2.1.59"},
	GooseID:           {"1.52.0"},
	OpenInterpreterID: {"0.4.3"},
	CodexID:           {"0.148.0", "0.148.0-alpha.21"},
}

// IsVendorAdapter returns true if the adapterID belongs to a vendor-specific adapter
// that has an empirically captured prompt baseline.
func IsVendorAdapter(adapterID string) bool {
	switch strings.ToLower(strings.TrimSpace(adapterID)) {
	case AiderID, ClaudeID, CodexID, GooseID, OpenInterpreterID:
		return true
	default:
		return false
	}
}

// GetVerifiedVersions returns the list of verified versions for the given adapter.
func GetVerifiedVersions(adapterID string) []string {
	normalized := strings.ToLower(strings.TrimSpace(adapterID))
	versions, ok := VerifiedVersions[normalized]
	if !ok {
		return nil
	}
	return append([]string(nil), versions...)
}

var versionRegex = regexp.MustCompile(`(?i)(?:v)?([0-9]+\.[0-9]+(?:\.[0-9]+)?(?:-[a-zA-Z0-9.]+)?)\b`)

// ParseVersion extracts a semver-like version string from arbitrary CLI output (e.g. from --version).
func ParseVersion(output string) string {
	matches := versionRegex.FindStringSubmatch(output)
	if len(matches) < 2 {
		return ""
	}
	v := strings.TrimSpace(matches[1])
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimPrefix(v, "V")
	return v
}

// CheckVersion evaluates whether the installed version is verified for the given adapter.
// Returns (unverified, reason).
// For generic/custom adapters, unverified is always false.
func CheckVersion(adapterID string, installedVersion string) (unverified bool, reason string) {
	if !IsVendorAdapter(adapterID) {
		return false, ""
	}
	verifiedList := GetVerifiedVersions(adapterID)
	if len(verifiedList) == 0 {
		return false, ""
	}

	installed := strings.TrimSpace(installedVersion)
	if installed == "" {
		return true, fmt.Sprintf("no version detected for the %s adapter (verified versions: %s)", adapterID, strings.Join(verifiedList, ", "))
	}

	// Normalize
	installed = strings.TrimPrefix(installed, "v")
	installed = strings.TrimPrefix(installed, "V")

	for _, v := range verifiedList {
		if strings.EqualFold(installed, v) {
			return false, ""
		}
	}

	return true, fmt.Sprintf("version %s is not verified for the %s adapter (verified versions: %s)", installed, adapterID, strings.Join(verifiedList, ", "))
}
