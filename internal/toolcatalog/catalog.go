// Package toolcatalog describes local CLI launch profiles without coupling
// them to terminal backends, provider APIs, credentials, or model selection.
package toolcatalog

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Hocsman/Relayer/internal/agent"
)

// ProfileID is the stable identifier of a declarative CLI launch profile.
type ProfileID string

const (
	ClaudeCode ProfileID = "claude-code"
	CodexCLI   ProfileID = "codex-cli"
	MimoCode   ProfileID = "mimo-code"
	Custom     ProfileID = "custom"
)

// Descriptor contains only non-sensitive launch metadata. A descriptor is not
// a claim that Relayer has a vendor-specific adapter for the named tool.
type Descriptor struct {
	ID                 ProfileID
	Name               string
	Executables        []string
	DefaultAdapter     string
	RequiresExecutable bool
}

// LaunchRequest resolves one profile to Relayer's existing process contract.
// Args are literal argv entries. No provider, model, shell, environment, or
// credential fields are accepted or inferred by this package.
type LaunchRequest struct {
	ProfileID  ProfileID
	AgentID    string
	Name       string
	Executable string
	Args       []string
	Cwd        string
	Adapter    string
	Backend    string
}

var descriptors = []Descriptor{
	{
		ID:             ClaudeCode,
		Name:           "Claude Code",
		Executables:    []string{"claude"},
		DefaultAdapter: agent.AdapterGeneric,
	},
	{
		ID:             CodexCLI,
		Name:           "Codex CLI",
		Executables:    []string{"codex"},
		DefaultAdapter: agent.AdapterGeneric,
	},
	{
		ID:             MimoCode,
		Name:           "MiMo Code",
		Executables:    []string{"mimo"},
		DefaultAdapter: agent.AdapterGeneric,
	},
	{
		ID:                 Custom,
		Name:               "Custom CLI",
		DefaultAdapter:     agent.AdapterGeneric,
		RequiresExecutable: true,
	},
}

// Descriptors returns the built-in profiles in stable display order. The
// result and every mutable field are independent from the package catalogue.
func Descriptors() []Descriptor {
	result := make([]Descriptor, len(descriptors))
	for index, descriptor := range descriptors {
		result[index] = cloneDescriptor(descriptor)
	}
	return result
}

// Lookup returns an independent copy of the requested descriptor. IDs are
// matched case-insensitively after trimming surrounding whitespace.
func Lookup(id ProfileID) (Descriptor, bool) {
	normalized := normalizeID(id)
	for _, descriptor := range descriptors {
		if descriptor.ID == normalized {
			return cloneDescriptor(descriptor), true
		}
	}
	return Descriptor{}, false
}

// Resolve creates an agent.Spec using exact argv supplied by the caller. A
// known profile may provide only its executable name; it never adds flags. The
// caller remains responsible for canonical agent.ValidateSpec validation in
// the context of its configuration directory and default backend.
func Resolve(request LaunchRequest) (agent.Spec, error) {
	descriptor, ok := Lookup(request.ProfileID)
	if !ok {
		return agent.Spec{}, fmt.Errorf("unknown tool profile %q", strings.TrimSpace(string(request.ProfileID)))
	}

	agentID := strings.TrimSpace(request.AgentID)
	if agentID == "" {
		return agent.Spec{}, errors.New("agent id must not be blank")
	}
	name := strings.TrimSpace(request.Name)
	if name == "" {
		return agent.Spec{}, errors.New("agent name must not be blank")
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "agent id", value: agentID},
		{name: "agent name", value: name},
		{name: "working directory", value: request.Cwd},
		{name: "adapter", value: request.Adapter},
		{name: "backend", value: request.Backend},
	} {
		if err := rejectNUL(field.name, field.value); err != nil {
			return agent.Spec{}, err
		}
	}

	executable := request.Executable
	if strings.TrimSpace(executable) == "" {
		if descriptor.RequiresExecutable || len(descriptor.Executables) == 0 {
			return agent.Spec{}, fmt.Errorf("tool profile %q requires an explicit executable", descriptor.ID)
		}
		executable = descriptor.Executables[0]
	}
	if err := rejectNUL("executable", executable); err != nil {
		return agent.Spec{}, err
	}

	command := make([]string, 1, len(request.Args)+1)
	command[0] = executable
	for index, argument := range request.Args {
		if err := rejectNUL(fmt.Sprintf("argument %d", index), argument); err != nil {
			return agent.Spec{}, err
		}
		command = append(command, argument)
	}

	adapterID := strings.TrimSpace(request.Adapter)
	if adapterID == "" {
		adapterID = descriptor.DefaultAdapter
	}
	return agent.Spec{
		ID:      agentID,
		Name:    name,
		Command: command,
		Cwd:     request.Cwd,
		Adapter: adapterID,
		Backend: strings.TrimSpace(request.Backend),
	}, nil
}

func normalizeID(id ProfileID) ProfileID {
	return ProfileID(strings.ToLower(strings.TrimSpace(string(id))))
}

func cloneDescriptor(descriptor Descriptor) Descriptor {
	descriptor.Executables = append([]string(nil), descriptor.Executables...)
	return descriptor
}

func rejectNUL(field, value string) error {
	if strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("%s contains a NUL byte", field)
	}
	return nil
}
