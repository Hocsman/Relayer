// Package version exposes build metadata injected into release binaries.
package version

import (
	"fmt"
	"strings"
	"unicode"
)

const Program = "relayer"

// Version and Commit are variables so release builds can set them with
// -ldflags -X. Development builds remain explicit instead of pretending to be
// a released semantic version.
var (
	Version = "dev"
	Commit  = "unknown"
)

// String returns the stable, single-line representation used by --version.
func String() string {
	return fmt.Sprintf(
		"%s %s (commit %s)",
		Program,
		clean(Version, "dev"),
		clean(Commit, "unknown"),
	)
}

func clean(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsFunc(value, unicode.IsControl) {
		return fallback
	}
	return value
}
