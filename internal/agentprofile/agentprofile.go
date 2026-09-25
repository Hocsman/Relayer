// Package agentprofile is the agent editor both front ends share: what an
// editor is shown of each agent in the configuration, and how the profiles it
// sends back become the agents that are written.
//
// The Desktop GUI and the web gateway each had their own copy. The desktop's
// kept every field the form cannot show; the gateway's rebuilt each agent from
// the form alone, so every save there dropped each agent's environment
// variables and turned a shell agent into `command: [<id>]`. One resolver
// means one set of rules for what a save may change.
package agentprofile

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/toolcatalog"
)

const (
	// MinProfiles and MaxProfiles bound the agents a save may configure.
	MinProfiles = 1
	MaxProfiles = 8

	maximumArgs  = 64
	maximumName  = 80
	maximumValue = 4096
)

// Read-only reasons, which the interface explains to the user.
const (
	ReasonShell          = "advanced_shell"
	ReasonEnvironment    = "advanced_environment"
	ReasonInvalidCommand = "invalid_command"
	ReasonAdapter        = "advanced_adapter"
	ReasonLegacyFields   = "legacy_profile_fields"
)

var (
	idPattern         = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
	keyLikeArgPattern = regexp.MustCompile(`(?i)^(?:sk|pk|api)[-_][a-z0-9_-]{12,}$`)
)

// ErrInvalid is the error of every refused save. Its detail names an agent by
// its position and says what is wrong, never a value: argv and environment
// values can be credentials.
var ErrInvalid = errors.New("one or more agent profiles are invalid")

// Profile is what an editor is shown of one agent. It deliberately has no
// environment values and no shell body, and never carries an existing agent's
// argv: a credential passed as an argument is found by no heuristic reliably,
// so the command stays in Go until the user replaces it as a whole.
//
// Its fields are those of each front end's own DTO, in the same order, so a
// front end converts it with a plain type conversion.
type Profile struct {
	ID              string
	Name            string
	PresetID        string
	Cwd             string
	Backend         string
	Adapter         string
	Argv            []string
	ExecutableLabel string
	ArgumentCount   int
	Locked          bool
	ReadOnlyReason  string
	PreserveOnSave  bool
}

// Input is one profile an editor sends back. Preserve says the agent is the
// existing one of that ID, whose command, adapter and every field the form does
// not show are kept; without it the profile is a new agent that inherits
// nothing from the file, whatever its ID.
type Input struct {
	ID       string
	Name     string
	PresetID string
	Cwd      string
	Backend  string
	Adapter  string
	Argv     []string
	Preserve bool
}

// View describes spec for an editor.
func View(spec agent.Spec) Profile {
	profile := Profile{
		ID:      spec.ID,
		Name:    spec.Name,
		Cwd:     spec.Cwd,
		Backend: spec.Backend,
	}
	reason := LockedReason(spec)
	if reason != ReasonAdapter {
		// Known adapter IDs are safe bridge metadata. An unknown advanced ID is
		// intentionally kept on the Go side with the rest of its locked spec.
		profile.Adapter = EffectiveAdapter(spec)
	}
	if reason != "" {
		profile.PresetID = string(toolcatalog.Custom)
		profile.Locked = true
		profile.PreserveOnSave = true
		profile.ReadOnlyReason = reason
		if spec.Shell != "" {
			profile.ExecutableLabel = "explicit shell"
		} else if len(spec.Command) > 0 {
			profile.ExecutableLabel = ExecutableLabel(ProfileForExecutable(spec.Command[0]))
		}
		return profile
	}
	profile.PresetID = string(ProfileForExecutable(spec.Command[0]))
	// Existing argv may contain credentials that no heuristic can identify
	// reliably. Keep it authoritative in Go and require an explicit full
	// replacement before any command value crosses into the interface.
	profile.PreserveOnSave = true
	profile.ExecutableLabel = ExecutableLabel(toolcatalog.ProfileID(profile.PresetID))
	profile.ArgumentCount = len(spec.Command) - 1
	return profile
}

// ExecutableLabel is the name an editor shows for an agent's executable. It
// names only a catalogue tool, never a path or a custom executable, which can
// be a credential or name the user's home directory.
func ExecutableLabel(profile toolcatalog.ProfileID) string {
	switch profile {
	case toolcatalog.Aider:
		return "aider"
	case toolcatalog.ClaudeCode:
		return "claude"
	case toolcatalog.CodexCLI:
		return "codex"
	case toolcatalog.GooseCLI:
		return "goose"
	case toolcatalog.OpenInterpreter:
		return "open-interpreter"
	case toolcatalog.MimoCode:
		return "mimo"
	case toolcatalog.Ollama:
		return "ollama"
	default:
		return "custom command"
	}
}

// EffectiveAdapter is the adapter an agent runs with: its own, or the default
// of the catalogue tool its executable names, or the generic one.
func EffectiveAdapter(spec agent.Spec) string {
	if adapterID := strings.ToLower(strings.TrimSpace(spec.Adapter)); adapterID != "" {
		return adapterID
	}
	if len(spec.Command) > 0 {
		if descriptor, ok := toolcatalog.Lookup(ProfileForExecutable(spec.Command[0])); ok {
			if adapterID := strings.ToLower(strings.TrimSpace(descriptor.DefaultAdapter)); adapterID != "" {
				return adapterID
			}
		}
	}
	return agent.AdapterGeneric
}

// ProfileForExecutable is the catalogue tool an executable names, or Custom.
func ProfileForExecutable(executable string) toolcatalog.ProfileID {
	name := portableExecutableName(executable)
	for _, descriptor := range toolcatalog.Descriptors() {
		for _, candidate := range descriptor.Executables {
			if name != "" && name == portableExecutableName(candidate) {
				return descriptor.ID
			}
		}
	}
	return toolcatalog.Custom
}

func portableExecutableName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	// Configurations can be prepared on another OS. filepath.Base follows the
	// host separator, so normalize Windows paths before comparing catalog
	// candidates and ignore the executable suffix just like adapter hints do.
	value = strings.ReplaceAll(value, `\`, "/")
	name := strings.ToLower(strings.TrimSpace(filepath.Base(value)))
	return strings.TrimSuffix(name, ".exe")
}

// LockedReason says why the editor cannot represent spec without loss, or is
// empty when it can. A locked agent is shown read-only, and a save must send
// it back with Preserve and nothing else: it is then copied as it is.
func LockedReason(spec agent.Spec) string {
	switch {
	case spec.Shell != "":
		return ReasonShell
	case len(spec.Env) > 0:
		return ReasonEnvironment
	case len(spec.Command) == 0:
		return ReasonInvalidCommand
	case !editableAdapter(spec):
		return ReasonAdapter
	case !idPattern.MatchString(spec.ID) ||
		utf8.RuneCountInString(spec.Name) > maximumName ||
		utf8.RuneCountInString(spec.Cwd) > maximumValue:
		return ReasonLegacyFields
	default:
		return ""
	}
}

func editableAdapter(spec agent.Spec) bool {
	adapterID := strings.ToLower(strings.TrimSpace(spec.Adapter))
	if adapterID == "" || adapterID == agent.AdapterGeneric {
		return true
	}
	switch ProfileForExecutable(spec.Command[0]) {
	case toolcatalog.Aider:
		return adapterID == adapters.AiderID
	case toolcatalog.ClaudeCode:
		return adapterID == adapters.ClaudeID
	case toolcatalog.CodexCLI:
		return adapterID == adapters.CodexID
	case toolcatalog.GooseCLI:
		return adapterID == adapters.GooseID
	case toolcatalog.OpenInterpreter:
		return adapterID == adapters.OpenInterpreterID
	default:
		return false
	}
}

// Resolve turns the profiles an editor sent back into the agents to write,
// against current, the agents of the configuration the editor was shown. The
// caller must have checked that the file still has that content, and must write
// only if it still has it: what a preserved agent keeps is what current holds.
//
//   - A locked agent must come back with Preserve, and is copied as it is:
//     environment, shell script and every other field. One that is missing,
//     or sent without Preserve, is refused: it cannot be removed or replaced
//     from the form, only in the YAML.
//   - An agent sent with Preserve keeps its command and adapter; only its
//     name, working directory and backend are taken from the form. Preserve
//     for an ID the file does not have is refused.
//   - Any other profile is a new agent, built from the form alone. It inherits
//     nothing from an agent of the same ID, and never matches one by
//     position: an agent removed and added again is a new one.
func Resolve(inputs []Input, current []agent.Spec, baseDir, defaultBackend string) ([]agent.Spec, error) {
	if len(inputs) < MinProfiles || len(inputs) > MaxProfiles {
		return nil, fmt.Errorf("%w: configure between %d and %d agents", ErrInvalid, MinProfiles, MaxProfiles)
	}
	currentByID := make(map[string]agent.Spec, len(current))
	lockedCurrent := make(map[string]struct{})
	for _, spec := range current {
		normalizedID := strings.ToLower(strings.TrimSpace(spec.ID))
		currentByID[normalizedID] = spec
		if LockedReason(spec) != "" {
			lockedCurrent[normalizedID] = struct{}{}
		}
	}
	preservedLocked := make(map[string]struct{}, len(lockedCurrent))
	specs := make([]agent.Spec, 0, len(inputs))
	for index, input := range inputs {
		position := index + 1
		normalizedID := strings.ToLower(strings.TrimSpace(input.ID))
		existing, exists := currentByID[normalizedID]
		_, isLocked := lockedCurrent[normalizedID]
		if isLocked && !input.Preserve {
			return nil, fmt.Errorf("%w: agent %d reuses the identifier of a read-only agent, which can only be changed in the YAML", ErrInvalid, position)
		}
		if input.Preserve {
			if !exists {
				return nil, fmt.Errorf("%w: agent %d keeps an agent the configuration does not have", ErrInvalid, position)
			}
			if len(input.Argv) != 0 {
				return nil, fmt.Errorf("%w: agent %d keeps its command and cannot also send one", ErrInvalid, position)
			}
			if input.Adapter != "" && !strings.EqualFold(strings.TrimSpace(input.Adapter), EffectiveAdapter(existing)) {
				return nil, fmt.Errorf("%w: agent %d keeps its command, whose adapter cannot change", ErrInvalid, position)
			}
			if isLocked {
				specs = append(specs, existing)
				preservedLocked[normalizedID] = struct{}{}
				continue
			}
			if !validEditableFields(input.ID, input.Name, input.Cwd) ||
				!agent.IsSupportedBackend(input.Backend) {
				return nil, fmt.Errorf("%w: agent %d has an invalid name, working directory or backend", ErrInvalid, position)
			}
			preserved := existing
			preserved.Name = input.Name
			preserved.Cwd = input.Cwd
			preserved.Backend = input.Backend
			specs = append(specs, preserved)
			continue
		}
		if !validEditableFields(input.ID, input.Name, input.Cwd) ||
			!agent.IsSupportedBackend(input.Backend) {
			return nil, fmt.Errorf("%w: agent %d has an invalid identifier, name, working directory or backend", ErrInvalid, position)
		}
		if len(input.Argv) == 0 || len(input.Argv) > maximumArgs || argvContainsInvalidValue(input.Argv) {
			return nil, fmt.Errorf("%w: agent %d needs a command of 1 to %d valid arguments", ErrInvalid, position, maximumArgs)
		}
		if argvContainsSensitiveValue(input.Argv) {
			return nil, fmt.Errorf("%w: agent %d has what looks like a credential in its command", ErrInvalid, position)
		}
		adapterID, ok := validatedAdapter(toolcatalog.ProfileID(input.PresetID), input.Adapter)
		if !ok {
			return nil, fmt.Errorf("%w: agent %d names an unknown catalogue profile or an adapter it does not support", ErrInvalid, position)
		}
		resolved, err := toolcatalog.Resolve(toolcatalog.LaunchRequest{
			ProfileID:  toolcatalog.ProfileID(input.PresetID),
			AgentID:    input.ID,
			Name:       input.Name,
			Executable: input.Argv[0],
			Args:       append([]string(nil), input.Argv[1:]...),
			Cwd:        input.Cwd,
			Adapter:    adapterID,
			Backend:    input.Backend,
		})
		if err != nil {
			return nil, fmt.Errorf("%w: agent %d does not have the arguments its catalogue profile requires", ErrInvalid, position)
		}
		specs = append(specs, resolved)
	}
	if len(preservedLocked) != len(lockedCurrent) {
		return nil, fmt.Errorf("%w: a read-only agent is missing, and can only be removed in the YAML", ErrInvalid)
	}
	if _, err := agent.ValidateAll(specs, baseDir, defaultBackend); err != nil {
		// The validation error can quote a working directory; the position
		// and the rule are enough.
		return nil, fmt.Errorf("%w: an identifier is used twice, or a working directory does not exist", ErrInvalid)
	}
	return specs, nil
}

func validatedAdapter(profileID toolcatalog.ProfileID, value string) (string, bool) {
	descriptor, ok := toolcatalog.Lookup(profileID)
	if !ok {
		return "", false
	}
	adapterID := strings.ToLower(strings.TrimSpace(value))
	if adapterID == "" {
		adapterID = strings.ToLower(strings.TrimSpace(descriptor.DefaultAdapter))
	}
	if adapterID == agent.AdapterGeneric || adapterID == strings.ToLower(strings.TrimSpace(descriptor.DefaultAdapter)) {
		return adapterID, adapterID != ""
	}
	return "", false
}

func validEditableFields(id, name, cwd string) bool {
	return idPattern.MatchString(strings.TrimSpace(id)) &&
		strings.TrimSpace(name) != "" &&
		utf8.RuneCountInString(name) <= maximumName &&
		!strings.ContainsRune(name, '\x00') &&
		utf8.RuneCountInString(cwd) <= maximumValue &&
		!strings.ContainsRune(cwd, '\x00')
}

func argvContainsInvalidValue(argv []string) bool {
	for index, argument := range argv {
		if utf8.RuneCountInString(argument) > maximumValue || strings.ContainsRune(argument, '\x00') {
			return true
		}
		if index == 0 && strings.TrimSpace(argument) == "" {
			return true
		}
	}
	return false
}

func argvContainsSensitiveValue(argv []string) bool {
	markers := map[string]struct{}{
		"access-key": {}, "api-key": {}, "apikey": {}, "auth": {},
		"authentication": {}, "authorization": {}, "bearer": {},
		"client-secret": {}, "cookie": {}, "credential": {}, "key": {},
		"otp": {}, "passphrase": {}, "password": {}, "pin": {},
		"private-key": {}, "secret": {}, "session": {}, "token": {},
	}
	for index, argument := range argv {
		redacted := audit.Redact(argument)
		if strings.Contains(redacted, "[REDACTED]") ||
			strings.Contains(strings.ToUpper(redacted), "%5BREDACTED%5D") ||
			keyLikeArgPattern.MatchString(strings.TrimSpace(argument)) {
			return true
		}
		normalized := strings.ToLower(strings.TrimLeft(strings.TrimSpace(argument), "-"))
		name := normalized
		if separator := strings.IndexByte(name, '='); separator >= 0 {
			name = name[:separator]
		}
		name = strings.NewReplacer("_", "-", ".", "-").Replace(name)
		if _, sensitive := markers[name]; sensitive {
			if strings.Contains(normalized, "=") || index+1 < len(argv) {
				return true
			}
		}
	}
	return false
}
