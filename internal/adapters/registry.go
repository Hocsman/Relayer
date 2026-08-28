package adapters

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Status exposes the maturity of a registered adapter without claiming that
// an experimental placeholder is implemented.
type Status string

const (
	StatusStable       Status = "stable"
	StatusExperimental Status = "experimental"
)

// Descriptor is safe to display in diagnostics and documentation.
type Descriptor struct {
	ID          string
	Status      Status
	Implemented bool
	Executables []string
}

// Factory creates independent adapter state for one terminal session.
type Factory func() (Adapter, error)

type registryEntry struct {
	descriptor Descriptor
	factory    Factory
}

// Registry resolves explicit adapters and executable-name hints. Generic is
// always installed as the final fallback. Claude and Codex remain
// experimental and recognize only interactions backed by anonymized fixtures.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]registryEntry
}

// NewRegistry builds the production registry around legacy regex patterns.
func NewRegistry(patterns []Pattern) (*Registry, error) {
	// Compile once during configuration validation, before any process starts.
	if _, err := NewGenericRegexAdapter(patterns); err != nil {
		return nil, err
	}
	patternsCopy := append([]Pattern(nil), patterns...)
	registry := &Registry{entries: make(map[string]registryEntry)}
	if err := registry.register(Descriptor{
		ID:          GenericID,
		Status:      StatusStable,
		Implemented: true,
	}, func() (Adapter, error) {
		return NewGenericRegexAdapter(patternsCopy)
	}); err != nil {
		return nil, err
	}
	for _, implementation := range []struct {
		descriptor Descriptor
		factory    Factory
	}{
		{
			descriptor: Descriptor{ID: ClaudeID, Status: StatusExperimental, Implemented: true, Executables: []string{"claude"}},
			factory:    func() (Adapter, error) { return NewClaudeAdapter(patternsCopy) },
		},
		{
			descriptor: Descriptor{ID: CodexID, Status: StatusExperimental, Implemented: true, Executables: []string{"codex"}},
			factory:    func() (Adapter, error) { return NewCodexAdapter(patternsCopy) },
		},
	} {
		if err := registry.register(implementation.descriptor, implementation.factory); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

// Register adds an adapter implementation or an unavailable maturity
// descriptor. Executable hints are used only when factory is non-nil and the
// descriptor explicitly marks the adapter implemented.
func (r *Registry) Register(descriptor Descriptor, factory Factory) error {
	return r.register(descriptor, factory)
}

func (r *Registry) register(descriptor Descriptor, factory Factory) error {
	if r == nil {
		return errors.New("registry d'adaptateurs nil")
	}
	id := strings.ToLower(strings.TrimSpace(descriptor.ID))
	if id == "" {
		return errors.New("identifiant d'adaptateur vide")
	}
	if descriptor.Status != StatusStable && descriptor.Status != StatusExperimental {
		return fmt.Errorf("statut d'adaptateur %q invalide", descriptor.Status)
	}
	if descriptor.Implemented != (factory != nil) {
		return fmt.Errorf("disponibilité incohérente pour l'adaptateur %q", id)
	}
	descriptor.ID = id
	descriptor.Executables = append([]string(nil), descriptor.Executables...)
	seenExecutables := make(map[string]struct{}, len(descriptor.Executables))
	for index := range descriptor.Executables {
		descriptor.Executables[index] = executableName(descriptor.Executables[index])
		if descriptor.Executables[index] == "" {
			return fmt.Errorf("exécutable vide pour l'adaptateur %q", id)
		}
		if _, duplicate := seenExecutables[descriptor.Executables[index]]; duplicate {
			return fmt.Errorf("exécutable dupliqué %q pour l'adaptateur %q", descriptor.Executables[index], id)
		}
		seenExecutables[descriptor.Executables[index]] = struct{}{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.entries[id]; exists {
		return fmt.Errorf("adaptateur dupliqué %q", id)
	}
	for existingID, existing := range r.entries {
		for executable := range seenExecutables {
			if containsExecutable(existing.descriptor.Executables, executable) {
				return fmt.Errorf("exécutable %q déjà associé à l'adaptateur %q", executable, existingID)
			}
		}
	}
	r.entries[id] = registryEntry{descriptor: descriptor, factory: factory}
	return nil
}

// Resolve honors an explicit ID. With no explicit ID it considers executable
// hints only for implemented adapters, then falls back to generic.
func (r *Registry) Resolve(requestedID, executable string) (Adapter, Descriptor, error) {
	if r == nil {
		return nil, Descriptor{}, errors.New("registry d'adaptateurs nil")
	}
	requestedID = strings.ToLower(strings.TrimSpace(requestedID))
	r.mu.RLock()
	var (
		entry  registryEntry
		exists bool
	)
	if requestedID != "" {
		entry, exists = r.entries[requestedID]
	} else {
		executable = executableName(executable)
		if executable != "" {
			for _, candidate := range r.entries {
				if !candidate.descriptor.Implemented || candidate.factory == nil ||
					!containsExecutable(candidate.descriptor.Executables, executable) {
					continue
				}
				entry, exists = candidate, true
				break
			}
		}
		if !exists {
			entry, exists = r.entries[GenericID]
		}
	}
	r.mu.RUnlock()

	if !exists {
		if requestedID != "" {
			return nil, Descriptor{}, fmt.Errorf("%w: %q", ErrUnknownAdapter, requestedID)
		}
		return nil, Descriptor{}, errors.New("fallback generic indisponible")
	}
	if !entry.descriptor.Implemented || entry.factory == nil {
		return nil, cloneDescriptor(entry.descriptor), fmt.Errorf(
			"%w: %s (%s)",
			ErrAdapterUnavailable,
			entry.descriptor.ID,
			entry.descriptor.Status,
		)
	}
	adapter, err := entry.factory()
	if err != nil {
		return nil, cloneDescriptor(entry.descriptor), err
	}
	if adapter == nil {
		return nil, cloneDescriptor(entry.descriptor), fmt.Errorf("factory de l'adaptateur %q a retourné nil", entry.descriptor.ID)
	}
	if adapter.ID() != entry.descriptor.ID {
		return nil, cloneDescriptor(entry.descriptor), fmt.Errorf(
			"factory de l'adaptateur %q a retourné l'identifiant incohérent %q",
			entry.descriptor.ID,
			adapter.ID(),
		)
	}
	return adapter, cloneDescriptor(entry.descriptor), nil
}

// Descriptors returns a deterministic, defensive maturity inventory.
func (r *Registry) Descriptors() []Descriptor {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	result := make([]Descriptor, 0, len(r.entries))
	for _, entry := range r.entries {
		result = append(result, cloneDescriptor(entry.descriptor))
	}
	r.mu.RUnlock()
	sort.Slice(result, func(left, right int) bool { return result[left].ID < result[right].ID })
	return result
}

func cloneDescriptor(descriptor Descriptor) Descriptor {
	descriptor.Executables = append([]string(nil), descriptor.Executables...)
	return descriptor
}

func executableName(value string) string {
	// Configuration may be prepared on a different platform than the current
	// runtime. Normalize Windows separators before filepath.Base so executable
	// hints remain deterministic without relying on the host OS.
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	value = strings.ReplaceAll(value, `\`, "/")
	name := strings.ToLower(strings.TrimSpace(filepath.Base(value)))
	return strings.TrimSuffix(name, ".exe")
}

func containsExecutable(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(value, target) {
			return true
		}
	}
	return false
}
