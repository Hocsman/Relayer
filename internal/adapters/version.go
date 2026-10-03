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

// VendorExecutable names the one executable a vendor adapter's own
// distribution installs. It is the only binary the version probe may run:
// anything else — a shell wrapper (sh -c, cmd /c), a launcher (npx, node,
// python, docker), a differently named script — would report its own version
// as the agent's, or replay the configured command line.
func VendorExecutable(adapterID string) string {
	switch strings.ToLower(strings.TrimSpace(adapterID)) {
	case AiderID:
		return "aider"
	case ClaudeID:
		return "claude"
	case CodexID:
		return "codex"
	case GooseID:
		return "goose"
	case OpenInterpreterID:
		return "interpreter"
	default:
		return ""
	}
}

// ParseVersion extracts a semver-like version from the one line of output
// that names the product, and from no other line. --version output is not
// only the version: a launcher prints its own (npx node v22…), a shell rc
// greets, a wrapper banners. The first number in such output is not the
// agent's version, and a banner that became "the agent's version" was shown
// to every operator — and every viewer — as the installed version. An empty
// product anchors nothing: output with no product line yields no version.
func ParseVersion(output, product string) string {
	product = strings.ToLower(strings.TrimSpace(product))
	if product == "" {
		return ""
	}
	for _, line := range strings.Split(output, "\n") {
		if !strings.Contains(strings.ToLower(line), product) {
			continue
		}
		matches := versionRegex.FindStringSubmatch(line)
		if len(matches) < 2 {
			continue
		}
		v := strings.TrimSpace(matches[1])
		v = strings.TrimPrefix(v, "v")
		v = strings.TrimPrefix(v, "V")
		return v
	}
	return ""
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
