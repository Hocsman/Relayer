package toolcatalog

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// InstallStatus describes only local executable discovery. It does not imply
// that a vendor-specific adapter or authentication is available.
type InstallStatus string

const (
	InstallUnknown      InstallStatus = "unknown"
	InstallInstalled    InstallStatus = "installed"
	InstallNotInstalled InstallStatus = "not_installed"
)

// Detection is local installation metadata. Version is optional and is
// populated only by an explicitly injected, non-executing metadata source.
type Detection struct {
	Status       InstallStatus
	Executable   string
	Path         string
	Version      string
	VersionKnown bool
}

// Detector discovers one of a set of executable candidates without retaining
// or mutating the caller's slice.
type Detector interface {
	Detect(context.Context, []string) (Detection, error)
}

// LookPathFunc resolves an executable name without executing it.
type LookPathFunc func(string) (string, error)

// VersionLookupFunc reads version metadata for an already resolved path. It is
// intentionally injected: toolcatalog never invokes an executable with a
// --version flag or any other command to obtain this value.
type VersionLookupFunc func(string) (version string, ok bool)

// PathDetector performs passive PATH discovery. A nil LookPath uses
// exec.LookPath. VersionLookup must itself be a trusted, non-executing metadata
// source; when omitted, version remains unknown.
type PathDetector struct {
	LookPath      LookPathFunc
	VersionLookup VersionLookupFunc
}

// DefaultDetector returns the passive production detector.
func DefaultDetector() Detector {
	return PathDetector{LookPath: exec.LookPath}
}

// Detect resolves installation state for a profile. An explicit executable
// replaces the descriptor candidates. A profile with no candidate and no
// override is unknown rather than misleadingly reported as not installed.
func Detect(
	ctx context.Context,
	id ProfileID,
	executableOverride string,
	detector Detector,
) (Detection, error) {
	descriptor, ok := Lookup(id)
	if !ok {
		return Detection{}, fmt.Errorf("unknown tool profile %q", strings.TrimSpace(string(id)))
	}
	if ctx == nil {
		return Detection{}, errors.New("detection context must not be nil")
	}
	if err := rejectNUL("executable override", executableOverride); err != nil {
		return Detection{}, err
	}

	var candidates []string
	if strings.TrimSpace(executableOverride) != "" {
		candidates = []string{executableOverride}
	} else {
		candidates = append([]string(nil), descriptor.Executables...)
	}
	if len(candidates) == 0 {
		return Detection{Status: InstallUnknown}, nil
	}
	if detector == nil {
		return Detection{}, errors.New("executable detector must not be nil")
	}

	result, err := detector.Detect(ctx, append([]string(nil), candidates...))
	if err != nil {
		return Detection{}, fmt.Errorf("detect tool profile %q: %w", descriptor.ID, err)
	}
	if err := validateDetection(result); err != nil {
		return Detection{}, fmt.Errorf("detect tool profile %q: %w", descriptor.ID, err)
	}
	return result, nil
}

// Detect passively searches PATH in candidate order and never starts a child
// process. Unexpected lookup failures, including exec.ErrDot, are surfaced.
func (detector PathDetector) Detect(ctx context.Context, candidates []string) (Detection, error) {
	if ctx == nil {
		return Detection{}, errors.New("detection context must not be nil")
	}
	lookup := detector.LookPath
	if lookup == nil {
		lookup = exec.LookPath
	}
	if len(candidates) == 0 {
		return Detection{Status: InstallUnknown}, nil
	}

	for index, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return Detection{}, err
		}
		if strings.TrimSpace(candidate) == "" {
			return Detection{}, fmt.Errorf("executable candidate %d is blank", index)
		}
		if err := rejectNUL(fmt.Sprintf("executable candidate %d", index), candidate); err != nil {
			return Detection{}, err
		}

		path, err := lookup(candidate)
		if err != nil {
			if errors.Is(err, exec.ErrNotFound) {
				continue
			}
			return Detection{}, fmt.Errorf("look up executable %q: %w", candidate, err)
		}
		if strings.TrimSpace(path) == "" {
			return Detection{}, fmt.Errorf("look up executable %q returned an empty path", candidate)
		}

		result := Detection{
			Status:     InstallInstalled,
			Executable: candidate,
			Path:       path,
		}
		if detector.VersionLookup != nil {
			version, known := detector.VersionLookup(path)
			version = strings.TrimSpace(version)
			if known && version != "" {
				result.Version = version
				result.VersionKnown = true
			}
		}
		return result, nil
	}

	return Detection{Status: InstallNotInstalled}, nil
}

func validateDetection(detection Detection) error {
	switch detection.Status {
	case InstallUnknown, InstallNotInstalled:
		if detection.Executable != "" || detection.Path != "" || detection.Version != "" || detection.VersionKnown {
			return fmt.Errorf("status %q must not include executable metadata", detection.Status)
		}
	case InstallInstalled:
		if strings.TrimSpace(detection.Executable) == "" {
			return errors.New("installed detection has a blank executable")
		}
		if strings.TrimSpace(detection.Path) == "" {
			return errors.New("installed detection has a blank path")
		}
		for _, field := range []struct {
			name  string
			value string
		}{
			{name: "detected executable", value: detection.Executable},
			{name: "detected path", value: detection.Path},
			{name: "detected version", value: detection.Version},
		} {
			if err := rejectNUL(field.name, field.value); err != nil {
				return err
			}
		}
		if detection.VersionKnown != (strings.TrimSpace(detection.Version) != "") {
			return errors.New("installed detection has inconsistent version metadata")
		}
	default:
		return fmt.Errorf("invalid installation status %q", detection.Status)
	}
	return nil
}
