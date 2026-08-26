package app

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/terminal"
)

// backendRouter is the only application component that knows a run may mix
// concrete terminal implementations. Every operation is routed by stable
// agent ID; auto never reaches this layer.
type backendRouter struct {
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.RWMutex
	backends map[string]terminal.Backend
	routes   map[string]terminal.Backend
	closed   bool

	closeMu        sync.Mutex
	closedBackends map[string]bool
	adapters       *adapters.Registry
}

var _ terminal.Backend = (*backendRouter)(nil)

func newBackendRouter(parent context.Context, backends ...terminal.Backend) (*backendRouter, error) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	result := &backendRouter{
		ctx:            ctx,
		cancel:         cancel,
		backends:       make(map[string]terminal.Backend, len(backends)),
		routes:         make(map[string]terminal.Backend),
		closedBackends: make(map[string]bool, len(backends)),
	}
	for _, backend := range backends {
		if backend == nil {
			cancel()
			return nil, errors.New("backend terminal nil")
		}
		name := strings.ToLower(strings.TrimSpace(backend.Name()))
		if name == "" || name == agent.BackendAuto {
			cancel()
			return nil, fmt.Errorf("nom de backend concret invalide %q", backend.Name())
		}
		if _, exists := result.backends[name]; exists {
			cancel()
			return nil, fmt.Errorf("backend concret dupliqué %q", name)
		}
		result.backends[name] = backend
	}
	if len(result.backends) == 0 {
		cancel()
		return nil, errors.New("aucun backend terminal concret")
	}
	return result, nil
}

func (r *backendRouter) Name() string {
	r.mu.RLock()
	names := make([]string, 0, len(r.backends))
	for name := range r.backends {
		names = append(names, name)
	}
	r.mu.RUnlock()
	sort.Strings(names)
	return strings.Join(names, "+")
}

func (r *backendRouter) Start(ctx context.Context, spec agent.Spec, size terminal.Size) (terminal.Info, error) {
	backendName := strings.ToLower(strings.TrimSpace(spec.Backend))
	if backendName == agent.BackendAuto || backendName == "" {
		return terminal.Info{}, fmt.Errorf("backend de l'agent %q non résolu: %q", spec.ID, spec.Backend)
	}
	r.mu.RLock()
	backend := r.backends[backendName]
	closed := r.closed
	r.mu.RUnlock()
	if closed || r.ctx.Err() != nil {
		return terminal.Info{}, terminal.ErrClosed
	}
	if backend == nil {
		return terminal.Info{}, fmt.Errorf("%w: backend %q", terminal.ErrUnavailable, backendName)
	}
	info, err := backend.Start(effectiveContext(ctx, r.ctx), spec, size)
	if err != nil {
		return terminal.Info{}, err
	}
	if strings.TrimSpace(info.Backend) == "" {
		info.Backend = backendName
	}
	if !strings.EqualFold(info.Backend, backendName) {
		_ = backend.Stop(context.Background(), info.ID)
		return terminal.Info{}, fmt.Errorf("backend %q a retourné le backend incohérent %q", backendName, info.Backend)
	}
	key := strings.ToLower(info.ID)
	r.mu.Lock()
	if r.closed || r.ctx.Err() != nil {
		r.mu.Unlock()
		_ = backend.Stop(context.Background(), info.ID)
		return terminal.Info{}, terminal.ErrClosed
	}
	if _, exists := r.routes[key]; exists {
		r.mu.Unlock()
		_ = backend.Stop(context.Background(), info.ID)
		return terminal.Info{}, fmt.Errorf("ID de session dupliqué %q", info.ID)
	}
	r.routes[key] = backend
	r.mu.Unlock()
	return info, nil
}

func (r *backendRouter) Send(ctx context.Context, id string, data []byte) error {
	backend, err := r.backendFor(id)
	if err != nil {
		return err
	}
	return backend.Send(effectiveContext(ctx, r.ctx), id, append([]byte(nil), data...))
}

func (r *backendRouter) Resize(ctx context.Context, id string, size terminal.Size) error {
	backend, err := r.backendFor(id)
	if err != nil {
		return err
	}
	return backend.Resize(effectiveContext(ctx, r.ctx), id, size.Normalize())
}

func (r *backendRouter) Snapshot(ctx context.Context, id string) (terminal.Snapshot, error) {
	backend, err := r.backendFor(id)
	if err != nil {
		return terminal.Snapshot{}, err
	}
	return backend.Snapshot(effectiveContext(ctx, r.ctx), id)
}

func (r *backendRouter) Output(id string) (string, error) {
	backend, err := r.backendFor(id)
	if err != nil {
		return "", err
	}
	if provider, ok := backend.(interface {
		Output(string) (string, error)
	}); ok {
		return provider.Output(id)
	}
	snapshot, err := backend.Snapshot(r.ctx, id)
	return snapshot.Output, err
}

func (r *backendRouter) PendingEvent(ctx context.Context, id string) (*adapters.Event, error) {
	backend, err := r.backendFor(id)
	if err != nil {
		return nil, err
	}
	ctx = effectiveContext(ctx, r.ctx)
	if provider, ok := backend.(terminal.PendingEventProvider); ok {
		return provider.PendingEvent(ctx, id)
	}
	return nil, fmt.Errorf("%w: backend %s sans snapshot d'événement en cache", terminal.ErrUnsupported, backend.Name())
}

func (r *backendRouter) SendDecision(ctx context.Context, id string, event adapters.Event, manualInput string) error {
	if r.adapters == nil {
		return errors.New("registry d'adaptateurs indisponible")
	}
	if strings.TrimSpace(event.ID) == "" {
		return fmt.Errorf("%w: identifiant d'événement vide", adapters.ErrEventMismatch)
	}
	if !strings.EqualFold(strings.TrimSpace(event.SessionID), strings.TrimSpace(id)) {
		return fmt.Errorf("%w: événement %q destiné à la session %q, pas %q",
			adapters.ErrEventMismatch, event.ID, event.SessionID, id)
	}
	backend, err := r.backendFor(id)
	if err != nil {
		return err
	}
	ctx = effectiveContext(ctx, r.ctx)
	provider, ok := backend.(terminal.PendingEventProvider)
	if !ok {
		return fmt.Errorf("%w: backend %s sans snapshot d'événement en cache", terminal.ErrUnsupported, backend.Name())
	}
	pending, err := provider.PendingEvent(ctx, id)
	if err != nil {
		return err
	}
	if pending == nil || pending.ID != event.ID {
		currentID := ""
		if pending != nil {
			currentID = pending.ID
		}
		return fmt.Errorf("%w: reçu %q, attendu %q", adapters.ErrEventMismatch, event.ID, currentID)
	}
	canonical := pending.Clone()
	adapter, _, err := r.adapters.Resolve(canonical.Adapter, "")
	if err != nil {
		return err
	}
	data, err := adapter.EncodeDecision(canonical, adapters.DecisionManual, manualInput)
	if err != nil {
		return err
	}
	if sender, ok := backend.(terminal.EventSender); ok {
		return sender.SendEvent(ctx, id, canonical.ID, data)
	}
	return fmt.Errorf("%w: backend %s sans livraison d'événement atomique", terminal.ErrUnsupported, backend.Name())
}

func (r *backendRouter) AttachCommand(ctx context.Context, id string) (*exec.Cmd, error) {
	backend, err := r.backendFor(id)
	if err != nil {
		return nil, err
	}
	return backend.AttachCommand(effectiveContext(ctx, r.ctx), id)
}

func (r *backendRouter) Stop(ctx context.Context, id string) error {
	backend, err := r.backendFor(id)
	if err != nil {
		return err
	}
	return backend.Stop(effectiveContext(ctx, r.ctx), id)
}

// Resync lets a backend reconcile attachment-specific state before the TUI
// fetches its latest snapshot. PTY needs only a regular resize.
func (r *backendRouter) Resync(ctx context.Context, id string, columns, rows int) error {
	backend, err := r.backendFor(id)
	if err != nil {
		return err
	}
	ctx = effectiveContext(ctx, r.ctx)
	if resyncer, ok := backend.(interface {
		Resync(context.Context, string, int, int) error
	}); ok {
		return resyncer.Resync(ctx, id, columns, rows)
	}
	return backend.Resize(ctx, id, terminal.Size{Columns: columns, Rows: rows}.Normalize())
}

func (r *backendRouter) Close(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	r.closeMu.Lock()
	defer r.closeMu.Unlock()

	r.mu.Lock()
	firstClose := !r.closed
	r.closed = true
	type namedBackend struct {
		name    string
		backend terminal.Backend
	}
	pending := make([]namedBackend, 0, len(r.backends))
	for name, backend := range r.backends {
		if !r.closedBackends[name] {
			pending = append(pending, namedBackend{name: name, backend: backend})
		}
	}
	r.mu.Unlock()
	if firstClose {
		r.cancel()
	}

	// Backends share the caller's global shutdown budget. Closing them in map
	// order would let a slow PTY cleanup consume the entire deadline before
	// tmux can remove its owned sessions. A failed backend stays pending so a
	// later Close can safely retry it; successful backends are never reopened.
	errorsByIndex := make([]error, len(pending))
	var group sync.WaitGroup
	for index, entry := range pending {
		index, entry := index, entry
		group.Add(1)
		go func() {
			defer group.Done()
			errorsByIndex[index] = entry.backend.Close(ctx)
		}()
	}
	group.Wait()

	var closeErr error
	r.mu.Lock()
	for index, err := range errorsByIndex {
		if err == nil {
			r.closedBackends[pending[index].name] = true
			continue
		}
		closeErr = errors.Join(closeErr, err)
	}
	r.mu.Unlock()
	return closeErr
}

func (r *backendRouter) BeginShutdown() {
	r.cancel()
	r.mu.RLock()
	backends := make([]terminal.Backend, 0, len(r.backends))
	for _, backend := range r.backends {
		backends = append(backends, backend)
	}
	r.mu.RUnlock()
	for _, backend := range backends {
		if shutdown, ok := backend.(interface{ BeginShutdown() }); ok {
			shutdown.BeginShutdown()
		}
	}
}
func (r *backendRouter) Context() context.Context { return r.ctx }

func (r *backendRouter) backendFor(id string) (terminal.Backend, error) {
	if r.ctx.Err() != nil {
		return nil, terminal.ErrClosed
	}
	r.mu.RLock()
	backend := r.routes[strings.ToLower(id)]
	r.mu.RUnlock()
	if backend == nil {
		return nil, fmt.Errorf("%w: %q", terminal.ErrSessionNotFound, id)
	}
	return backend, nil
}

func effectiveContext(request, owner context.Context) context.Context {
	if request == nil {
		return owner
	}
	return request
}

// tuiBackendAdapter translates Bubble Tea's small historical boundary to the
// exact-byte, context-aware backend contract.
type tuiBackendAdapter struct{ router *backendRouter }

func (a *tuiBackendAdapter) Name() string             { return a.router.Name() }
func (a *tuiBackendAdapter) Context() context.Context { return a.router.Context() }
func (a *tuiBackendAdapter) BeginShutdown()           { a.router.BeginShutdown() }

func (a *tuiBackendAdapter) Output(id string) (string, error) {
	return a.router.Output(id)
}

func (a *tuiBackendAdapter) SendInput(id, value string) error {
	return a.router.Send(a.router.Context(), id, []byte(value+"\r"))
}

func (a *tuiBackendAdapter) SendDecision(id string, event adapters.Event, value string) error {
	return a.router.SendDecision(a.router.Context(), id, event, value)
}

func (a *tuiBackendAdapter) Resize(id string, columns, rows int) error {
	return a.router.Resize(a.router.Context(), id, terminal.Size{Columns: columns, Rows: rows})
}

func (a *tuiBackendAdapter) ResizeContext(ctx context.Context, id string, columns, rows int) error {
	return a.router.Resize(ctx, id, terminal.Size{Columns: columns, Rows: rows})
}

func (a *tuiBackendAdapter) AttachCommand(ctx context.Context, id string) (*exec.Cmd, error) {
	return a.router.AttachCommand(ctx, id)
}

func (a *tuiBackendAdapter) Resync(ctx context.Context, id string, columns, rows int) error {
	return a.router.Resync(ctx, id, columns, rows)
}

func (a *tuiBackendAdapter) PendingEvent(ctx context.Context, id string) (*adapters.Event, error) {
	return a.router.PendingEvent(ctx, id)
}
