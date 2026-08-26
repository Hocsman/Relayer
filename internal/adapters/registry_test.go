package adapters

import (
	"errors"
	"reflect"
	"sync"
	"testing"
)

type registryStubAdapter struct{ id string }

func (a *registryStubAdapter) ID() string { return a.id }
func (*registryStubAdapter) Detect(*DetectionState, []byte) ([]Event, error) {
	return nil, nil
}
func (*registryStubAdapter) EncodeDecision(Event, Decision, string) ([]byte, error) {
	return nil, nil
}

func TestRegistryDescriptorsAreDeterministicDefensiveAndHonest(t *testing.T) {
	registry, err := NewRegistry(DefaultPatterns())
	if err != nil {
		t.Fatal(err)
	}
	want := []Descriptor{
		{ID: "claude", Status: StatusExperimental, Implemented: false, Executables: []string{"claude"}},
		{ID: "codex", Status: StatusExperimental, Implemented: false, Executables: []string{"codex"}},
		{ID: GenericID, Status: StatusStable, Implemented: true},
	}
	got := registry.Descriptors()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Descriptors = %#v, want %#v", got, want)
	}
	got[0].ID = "mutated"
	got[0].Executables[0] = "mutated"
	if fresh := registry.Descriptors(); !reflect.DeepEqual(fresh, want) {
		t.Fatalf("Descriptors returned shared storage: %#v", fresh)
	}
	if descriptors := (*Registry)(nil).Descriptors(); descriptors != nil {
		t.Fatalf("nil registry descriptors = %#v", descriptors)
	}
}

func TestRegistryResolveExplicitUnavailableUnknownAndGenericFallback(t *testing.T) {
	registry, err := NewRegistry(DefaultPatterns())
	if err != nil {
		t.Fatal(err)
	}
	first, descriptor, err := registry.Resolve(" GeNeRiC ", "ignored")
	if err != nil || first == nil || first.ID() != GenericID || descriptor.ID != GenericID || !descriptor.Implemented {
		t.Fatalf("explicit generic = adapter %#v descriptor %#v error %v", first, descriptor, err)
	}
	second, _, err := registry.Resolve(GenericID, "")
	if err != nil || second == nil || first == second {
		t.Fatalf("generic factories did not return independent instances: first %#v second %#v error %v", first, second, err)
	}

	for _, id := range []string{"claude", "codex"} {
		adapter, unavailable, err := registry.Resolve(id, "")
		if adapter != nil || !errors.Is(err, ErrAdapterUnavailable) || unavailable.ID != id ||
			unavailable.Implemented || unavailable.Status != StatusExperimental {
			t.Fatalf("Resolve(%q) = adapter %#v descriptor %#v error %v", id, adapter, unavailable, err)
		}
	}
	if adapter, descriptor, err := registry.Resolve("not-registered", ""); adapter != nil ||
		!errors.Is(err, ErrUnknownAdapter) || !reflect.DeepEqual(descriptor, Descriptor{}) {
		t.Fatalf("unknown adapter resolution = adapter %#v descriptor %#v error %v", adapter, descriptor, err)
	}

	// Executable hints for unimplemented placeholders must never imply vendor
	// support. They deliberately fall back to the stable generic adapter.
	for _, executable := range []string{"/opt/tools/claude", `C:\tools\codex.exe`} {
		adapter, descriptor, err := registry.Resolve("", executable)
		if err != nil || adapter == nil || adapter.ID() != GenericID || descriptor.ID != GenericID {
			t.Fatalf("fallback for %q = adapter %#v descriptor %#v error %v", executable, adapter, descriptor, err)
		}
	}
	if adapter, _, err := (*Registry)(nil).Resolve(GenericID, ""); adapter != nil || err == nil {
		t.Fatalf("nil registry Resolve = adapter %#v error %v", adapter, err)
	}
}

func TestRegistryExecutableHintUsesOnlyImplementedNormalizedEntry(t *testing.T) {
	registry := &Registry{entries: make(map[string]registryEntry)}
	if err := registry.Register(Descriptor{
		ID: "generic", Status: StatusStable, Implemented: true,
	}, func() (Adapter, error) { return &registryStubAdapter{id: GenericID}, nil }); err != nil {
		t.Fatal(err)
	}
	executables := []string{"Synthetic-Runner.EXE"}
	if err := registry.Register(Descriptor{
		ID: " Synthetic ", Status: StatusExperimental, Implemented: true, Executables: executables,
	}, func() (Adapter, error) { return &registryStubAdapter{id: "synthetic"}, nil }); err != nil {
		t.Fatal(err)
	}
	executables[0] = "mutated"
	adapter, descriptor, err := registry.Resolve("", "/work/bin/SYNTHETIC-RUNNER.exe")
	if err != nil || adapter == nil || adapter.ID() != "synthetic" || descriptor.ID != "synthetic" ||
		!reflect.DeepEqual(descriptor.Executables, []string{"synthetic-runner"}) {
		t.Fatalf("implemented executable hint = adapter %#v descriptor %#v error %v", adapter, descriptor, err)
	}
	descriptor.Executables[0] = "mutated"
	if fresh := registry.Descriptors(); !reflect.DeepEqual(fresh[1].Executables, []string{"synthetic-runner"}) {
		t.Fatalf("resolved descriptor aliases registry storage: %#v", fresh)
	}
}

func TestRegistryRegistrationValidationAndFactoryErrors(t *testing.T) {
	factory := func() (Adapter, error) { return &registryStubAdapter{id: "one"}, nil }
	if err := (*Registry)(nil).Register(Descriptor{ID: "one", Status: StatusStable, Implemented: true}, factory); err == nil {
		t.Fatal("nil registry accepted registration")
	}
	for _, test := range []struct {
		name       string
		descriptor Descriptor
		factory    Factory
	}{
		{name: "empty ID", descriptor: Descriptor{Status: StatusStable}},
		{name: "invalid status", descriptor: Descriptor{ID: "one", Status: Status("future")}},
		{name: "implemented without factory", descriptor: Descriptor{ID: "one", Status: StatusStable, Implemented: true}},
		{name: "factory while unavailable", descriptor: Descriptor{ID: "one", Status: StatusStable}, factory: factory},
		{name: "empty executable hint", descriptor: Descriptor{ID: "one", Status: StatusStable, Implemented: true, Executables: []string{" "}}, factory: factory},
		{name: "duplicate executable hint", descriptor: Descriptor{ID: "one", Status: StatusStable, Implemented: true, Executables: []string{"runner", "RUNNER.exe"}}, factory: factory},
	} {
		t.Run(test.name, func(t *testing.T) {
			registry := &Registry{entries: make(map[string]registryEntry)}
			if err := registry.Register(test.descriptor, test.factory); err == nil {
				t.Fatalf("invalid registration accepted: %#v", test.descriptor)
			}
		})
	}

	registry := &Registry{entries: make(map[string]registryEntry)}
	if err := registry.Register(Descriptor{ID: "One", Status: StatusStable, Implemented: true}, factory); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(Descriptor{ID: " one ", Status: StatusStable, Implemented: true}, factory); err == nil {
		t.Fatal("case-insensitive duplicate adapter was accepted")
	}
	if err := registry.Register(Descriptor{
		ID: "two", Status: StatusStable, Implemented: true, Executables: []string{"one.exe"},
	}, func() (Adapter, error) { return &registryStubAdapter{id: "two"}, nil }); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(Descriptor{
		ID: "three", Status: StatusStable, Implemented: true, Executables: []string{"ONE"},
	}, func() (Adapter, error) { return &registryStubAdapter{id: "three"}, nil }); err == nil {
		t.Fatal("conflicting executable hint was accepted")
	}
	factoryErr := errors.New("synthetic factory failure")
	if err := registry.Register(Descriptor{
		ID: "broken", Status: StatusExperimental, Implemented: true,
	}, func() (Adapter, error) { return nil, factoryErr }); err != nil {
		t.Fatal(err)
	}
	if adapter, descriptor, err := registry.Resolve("broken", ""); adapter != nil ||
		descriptor.ID != "broken" || !errors.Is(err, factoryErr) {
		t.Fatalf("factory failure = adapter %#v descriptor %#v error %v", adapter, descriptor, err)
	}
	if err := registry.Register(Descriptor{
		ID: "nil-result", Status: StatusExperimental, Implemented: true,
	}, func() (Adapter, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if adapter, _, err := registry.Resolve("nil-result", ""); adapter != nil || err == nil {
		t.Fatalf("nil factory result = adapter %#v error %v", adapter, err)
	}
	if err := registry.Register(Descriptor{
		ID: "mismatch", Status: StatusExperimental, Implemented: true,
	}, func() (Adapter, error) { return &registryStubAdapter{id: "different"}, nil }); err != nil {
		t.Fatal(err)
	}
	if adapter, _, err := registry.Resolve("mismatch", ""); adapter != nil || err == nil {
		t.Fatalf("mismatched factory result = adapter %#v error %v", adapter, err)
	}
	if _, err := NewRegistry([]Pattern{{Name: "broken", Expression: "("}}); err == nil {
		t.Fatal("NewRegistry accepted invalid generic regex")
	}
}

func TestRegistryConcurrentResolveAndInventory(t *testing.T) {
	registry, err := NewRegistry(DefaultPatterns())
	if err != nil {
		t.Fatal(err)
	}
	const workers = 32
	var group sync.WaitGroup
	errorsFound := make(chan error, workers)
	for index := 0; index < workers; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			adapter, descriptor, err := registry.Resolve("", "/work/bin/tool")
			if err != nil {
				errorsFound <- err
				return
			}
			if adapter.ID() != GenericID || descriptor.ID != GenericID || len(registry.Descriptors()) != 3 {
				errorsFound <- errors.New("incoherent concurrent registry result")
			}
		}()
	}
	group.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Error(err)
	}
}

func TestRegistryFactoryRunsOutsideRegistryLock(t *testing.T) {
	registry := &Registry{entries: make(map[string]registryEntry)}
	if err := registry.Register(Descriptor{
		ID: "introspective", Status: StatusExperimental, Implemented: true,
	}, func() (Adapter, error) {
		if len(registry.Descriptors()) != 1 {
			return nil, errors.New("factory could not inspect registry")
		}
		return &registryStubAdapter{id: "introspective"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	adapter, _, err := registry.Resolve("introspective", "")
	if err != nil || adapter == nil || adapter.ID() != "introspective" {
		t.Fatalf("introspective factory = adapter %#v error %v", adapter, err)
	}
}
