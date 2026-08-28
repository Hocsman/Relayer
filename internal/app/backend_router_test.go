package app

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/terminal"
)

type routerStartCall struct {
	spec agent.Spec
	size terminal.Size
}

type routerSendCall struct {
	id   string
	data []byte
}

type routerResizeCall struct {
	id   string
	size terminal.Size
}

type routerEventSendCall struct {
	id      string
	eventID string
	data    []byte
}

type routerLineCall struct {
	id   string
	line string
}

type routerLineBackend struct {
	*routerFakeBackend
	lineCalls []routerLineCall
	lineErr   error
}

func (b *routerLineBackend) SendLine(ctx context.Context, id, line string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lineCalls = append(b.lineCalls, routerLineCall{id: id, line: line})
	return b.lineErr
}

type routerEventBackend struct {
	*routerFakeBackend
	eventSends []routerEventSendCall
}

type routerPendingBackend struct{ *routerFakeBackend }

func (b *routerPendingBackend) PendingEvent(_ context.Context, _ string) (*adapters.Event, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pending == nil {
		return nil, nil
	}
	clone := b.pending.Clone()
	return &clone, nil
}

func (b *routerEventBackend) PendingEvent(_ context.Context, id string) (*adapters.Event, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pending == nil {
		return nil, nil
	}
	pending := b.pending.Clone()
	pending.SessionID = id
	return &pending, nil
}

func (b *routerEventBackend) SendEvent(_ context.Context, id, eventID string, data []byte) error {
	b.mu.Lock()
	b.eventSends = append(b.eventSends, routerEventSendCall{
		id: id, eventID: eventID, data: append([]byte(nil), data...),
	})
	b.mu.Unlock()
	return nil
}

type routerFakeBackend struct {
	mu sync.Mutex

	name            string
	returnedBackend string
	startErr        error
	attachErr       error
	closeErr        error
	closeErrors     []error
	pending         *adapters.Event
	starts          []routerStartCall
	sends           []routerSendCall
	resizes         []routerResizeCall
	snapshots       []string
	attaches        []string
	stops           []string
	closeCalls      int
}

type routerBlockingCloseBackend struct {
	*routerFakeBackend
	started  chan<- string
	barrier  <-chan struct{}
	failure  error
	deadline time.Time
}

type routerSequencedCloseBackend struct {
	*routerFakeBackend
	started      chan<- int
	releaseFirst <-chan struct{}
	temporaryErr error
	active       int
	maxActive    int
}

func (b *routerSequencedCloseBackend) Close(ctx context.Context) error {
	b.mu.Lock()
	b.closeCalls++
	call := b.closeCalls
	b.active++
	if b.active > b.maxActive {
		b.maxActive = b.active
	}
	b.mu.Unlock()
	b.started <- call
	if call == 1 {
		select {
		case <-b.releaseFirst:
		case <-ctx.Done():
		}
	}
	b.mu.Lock()
	b.active--
	b.mu.Unlock()
	if call == 1 {
		return b.temporaryErr
	}
	return nil
}

func (b *routerBlockingCloseBackend) Close(ctx context.Context) error {
	b.mu.Lock()
	b.closeCalls++
	b.deadline, _ = ctx.Deadline()
	b.mu.Unlock()
	b.started <- b.name
	select {
	case <-b.barrier:
	case <-ctx.Done():
	}
	<-ctx.Done()
	return errors.Join(b.failure, ctx.Err())
}

func newRouterFakeBackend(name string) *routerFakeBackend {
	return &routerFakeBackend{name: name, returnedBackend: name}
}

func newRouterLineBackend(name string) *routerLineBackend {
	return &routerLineBackend{routerFakeBackend: newRouterFakeBackend(name)}
}

func (b *routerFakeBackend) Name() string { return b.name }

func (b *routerFakeBackend) Start(_ context.Context, spec agent.Spec, size terminal.Size) (terminal.Info, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.starts = append(b.starts, routerStartCall{spec: spec, size: size})
	if b.startErr != nil {
		return terminal.Info{}, b.startErr
	}
	return terminal.Info{
		ID:             spec.ID,
		Name:           spec.Name,
		DisplayCommand: "safe command",
		Backend:        b.returnedBackend,
	}, nil
}

func (b *routerFakeBackend) Send(_ context.Context, id string, data []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Retain the supplied slice deliberately. The router, not a friendly
	// backend test double, owns the defensive-copy guarantee.
	b.sends = append(b.sends, routerSendCall{id: id, data: data})
	return nil
}

func (b *routerFakeBackend) Resize(_ context.Context, id string, size terminal.Size) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.resizes = append(b.resizes, routerResizeCall{id: id, size: size})
	return nil
}

func (b *routerFakeBackend) Snapshot(_ context.Context, id string) (terminal.Snapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.snapshots = append(b.snapshots, id)
	var pending *adapters.Event
	if b.pending != nil {
		clone := b.pending.Clone()
		pending = &clone
	}
	return terminal.Snapshot{ID: id, Running: true, Output: b.name + " output", Pending: pending}, nil
}

func (b *routerFakeBackend) AttachCommand(_ context.Context, id string) (*exec.Cmd, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.attaches = append(b.attaches, id)
	if b.attachErr != nil {
		return nil, b.attachErr
	}
	return exec.Command("true"), nil
}

func (b *routerFakeBackend) Stop(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stops = append(b.stops, id)
	return nil
}

func (b *routerFakeBackend) Close(context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	callIndex := b.closeCalls
	b.closeCalls++
	if callIndex < len(b.closeErrors) {
		return b.closeErrors[callIndex]
	}
	return b.closeErr
}

func TestBackendRouterRoutesMixedBackendsByCaseInsensitiveSessionID(t *testing.T) {
	pty := newRouterFakeBackend(agent.BackendPTY)
	tmux := newRouterFakeBackend(agent.BackendTmux)
	router, err := newBackendRouter(context.Background(), tmux, pty)
	if err != nil {
		t.Fatalf("newBackendRouter: %v", err)
	}
	t.Cleanup(func() { _ = router.Close(context.Background()) })

	ptyInfo, err := router.Start(context.Background(), agent.Spec{
		ID: "PTY-Agent", Name: "PTY Agent", Command: []string{"runner"}, Backend: agent.BackendPTY,
	}, terminal.Size{Columns: 80, Rows: 24})
	if err != nil {
		t.Fatalf("start PTY: %v", err)
	}
	tmuxInfo, err := router.Start(context.Background(), agent.Spec{
		ID: "Tmux-Agent", Name: "Tmux Agent", Command: []string{"runner"}, Backend: agent.BackendTmux,
	}, terminal.Size{Columns: 100, Rows: 30})
	if err != nil {
		t.Fatalf("start tmux: %v", err)
	}
	if ptyInfo.Backend != agent.BackendPTY || tmuxInfo.Backend != agent.BackendTmux {
		t.Fatalf("concrete info backends = PTY %q tmux %q", ptyInfo.Backend, tmuxInfo.Backend)
	}
	if router.Name() != "pty+tmux" {
		t.Fatalf("router name = %q, want sorted concrete names", router.Name())
	}

	if err := router.Send(context.Background(), "pty-agent", []byte("left")); err != nil {
		t.Fatalf("case-insensitive PTY route: %v", err)
	}
	if err := router.Resize(context.Background(), "TMUX-AGENT", terminal.Size{Columns: 0, Rows: 100000}); err != nil {
		t.Fatalf("case-insensitive tmux route: %v", err)
	}
	if snapshot, snapshotErr := router.Snapshot(context.Background(), "tMuX-aGeNt"); snapshotErr != nil || snapshot.Output != "tmux output" {
		t.Fatalf("tmux snapshot = %#v, error %v", snapshot, snapshotErr)
	}

	pty.mu.Lock()
	ptySends := append([]routerSendCall(nil), pty.sends...)
	pty.mu.Unlock()
	tmux.mu.Lock()
	tmuxResizes := append([]routerResizeCall(nil), tmux.resizes...)
	tmuxSnapshots := append([]string(nil), tmux.snapshots...)
	tmux.mu.Unlock()
	if len(ptySends) != 1 || ptySends[0].id != "pty-agent" || string(ptySends[0].data) != "left" {
		t.Fatalf("PTY sends = %#v", ptySends)
	}
	if len(tmuxResizes) != 1 || tmuxResizes[0].size != (terminal.Size{Columns: 1, Rows: 65535}) {
		t.Fatalf("tmux resizes = %#v", tmuxResizes)
	}
	if !reflect.DeepEqual(tmuxSnapshots, []string{"tMuX-aGeNt"}) {
		t.Fatalf("tmux snapshots = %#v", tmuxSnapshots)
	}
}

func TestBackendRouterSendCopiesExactBytes(t *testing.T) {
	backend := newRouterFakeBackend(agent.BackendPTY)
	router, err := newBackendRouter(context.Background(), backend)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = router.Close(context.Background()) })
	if _, err := router.Start(context.Background(), agent.Spec{
		ID: "copy", Name: "Copy", Command: []string{"runner"}, Backend: agent.BackendPTY,
	}, terminal.Size{}); err != nil {
		t.Fatal(err)
	}

	input := []byte{'Y', 0, '\n'}
	if err := router.Send(context.Background(), "copy", input); err != nil {
		t.Fatal(err)
	}
	input[0] = 'N'
	backend.mu.Lock()
	got := append([]byte(nil), backend.sends[0].data...)
	backend.mu.Unlock()
	if !reflect.DeepEqual(got, []byte{'Y', 0, '\n'}) {
		t.Fatalf("backend received %v after caller mutation", got)
	}
}

func TestBackendRouterEncodesAndAtomicallyRoutesExactEventDecision(t *testing.T) {
	backend := &routerEventBackend{routerFakeBackend: newRouterFakeBackend(agent.BackendPTY)}
	router, err := newBackendRouter(context.Background(), backend)
	if err != nil {
		t.Fatal(err)
	}
	router.adapters, err = adapters.NewRegistry(adapters.DefaultPatterns())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = router.Close(context.Background()) })
	if _, err := router.Start(context.Background(), agent.Spec{
		ID: "decision", Name: "Decision", Command: []string{"runner"}, Backend: agent.BackendPTY,
	}, terminal.Size{}); err != nil {
		t.Fatal(err)
	}

	event := adapters.Event{
		ID: "evt-decision-1", SessionID: "decision", Adapter: adapters.GenericID,
		Type: adapters.EventConfirmation,
	}
	stored := event.Clone()
	backend.pending = &stored
	emptyID := event.Clone()
	emptyID.ID = ""
	if err := router.SendDecision(context.Background(), "DECISION", emptyID, "secret"); !errors.Is(err, adapters.ErrEventMismatch) {
		t.Fatalf("empty event ID error = %v", err)
	}
	presented := event.Clone()
	presented.Adapter = "not-authoritative"
	presented.Type = adapters.EventProcessExit
	if err := router.SendDecision(context.Background(), "DECISION", presented, "Y"); err != nil {
		t.Fatalf("SendDecision: %v", err)
	}
	backend.mu.Lock()
	calls := append([]routerEventSendCall(nil), backend.eventSends...)
	backend.mu.Unlock()
	if len(calls) != 1 || calls[0].id != "DECISION" || calls[0].eventID != event.ID ||
		!reflect.DeepEqual(calls[0].data, []byte("Y\r")) {
		t.Fatalf("event sends = %#v", calls)
	}

	wrongSession := event.Clone()
	wrongSession.SessionID = "another-agent"
	if err := router.SendDecision(context.Background(), "decision", wrongSession, "secret"); !errors.Is(err, adapters.ErrEventMismatch) {
		t.Fatalf("cross-session decision error = %v", err)
	}
	backend.mu.Lock()
	callCount := len(backend.eventSends)
	backend.mu.Unlock()
	if callCount != 1 {
		t.Fatalf("cross-session decision reached backend: %#v", backend.eventSends)
	}
}

func TestBackendRouterRefusesDecisionWithoutAtomicEventSender(t *testing.T) {
	backend := &routerPendingBackend{routerFakeBackend: newRouterFakeBackend(agent.BackendPTY)}
	backend.pending = &adapters.Event{
		ID: "evt-legacy", SessionID: "legacy", Adapter: adapters.GenericID, Type: adapters.EventConfirmation,
	}
	router, err := newBackendRouter(context.Background(), backend)
	if err != nil {
		t.Fatal(err)
	}
	router.adapters, err = adapters.NewRegistry(adapters.DefaultPatterns())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = router.Close(context.Background()) })
	if _, err := router.Start(context.Background(), agent.Spec{
		ID: "legacy", Name: "Legacy", Command: []string{"runner"}, Backend: agent.BackendPTY,
	}, terminal.Size{}); err != nil {
		t.Fatal(err)
	}
	err = router.SendDecision(context.Background(), "legacy", adapters.Event{
		ID: "evt-legacy", SessionID: "legacy", Adapter: adapters.GenericID, Type: adapters.EventConfirmation,
	}, "Y")
	if !errors.Is(err, terminal.ErrUnsupported) {
		t.Fatalf("non-atomic decision error = %v", err)
	}
	backend.mu.Lock()
	sends := len(backend.sends)
	backend.mu.Unlock()
	if sends != 0 {
		t.Fatalf("non-atomic decision used raw Send %d time(s)", sends)
	}
}

func TestBackendRouterFillsBlankConcreteInfoBackend(t *testing.T) {
	backend := newRouterFakeBackend(agent.BackendPTY)
	backend.returnedBackend = ""
	router, err := newBackendRouter(context.Background(), backend)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = router.Close(context.Background()) })

	info, err := router.Start(context.Background(), agent.Spec{
		ID: "filled", Name: "Filled", Command: []string{"runner"}, Backend: agent.BackendPTY,
	}, terminal.Size{})
	if err != nil {
		t.Fatal(err)
	}
	if info.Backend != agent.BackendPTY {
		t.Fatalf("info backend = %q, want concrete PTY", info.Backend)
	}
}

func TestBackendRouterRejectsIncoherentInfoAndStopsStartedSession(t *testing.T) {
	backend := newRouterFakeBackend(agent.BackendTmux)
	backend.returnedBackend = agent.BackendPTY
	router, err := newBackendRouter(context.Background(), backend)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = router.Close(context.Background()) })

	_, err = router.Start(context.Background(), agent.Spec{
		ID: "mismatch", Name: "Mismatch", Command: []string{"runner"}, Backend: agent.BackendTmux,
	}, terminal.Size{})
	if err == nil || !strings.Contains(err.Error(), "inconsistent backend") {
		t.Fatalf("Start error = %v", err)
	}
	backend.mu.Lock()
	stops := append([]string(nil), backend.stops...)
	backend.mu.Unlock()
	if !reflect.DeepEqual(stops, []string{"mismatch"}) {
		t.Fatalf("rollback Stop calls = %#v", stops)
	}
	if _, lookupErr := router.Snapshot(context.Background(), "mismatch"); !errors.Is(lookupErr, terminal.ErrSessionNotFound) {
		t.Fatalf("incoherent session escaped into routes: %v", lookupErr)
	}
}

func TestBackendRouterUnknownIDsAreRecognizable(t *testing.T) {
	router, err := newBackendRouter(context.Background(), newRouterFakeBackend(agent.BackendPTY))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = router.Close(context.Background()) })

	if err := router.Send(context.Background(), "unknown", []byte("input")); !errors.Is(err, terminal.ErrSessionNotFound) {
		t.Fatalf("unknown Send error = %v", err)
	}
	if _, err := router.AttachCommand(context.Background(), "unknown"); !errors.Is(err, terminal.ErrSessionNotFound) {
		t.Fatalf("unknown AttachCommand error = %v", err)
	}
}

func TestBackendRouterCloseRetriesEveryBackendThatStillFails(t *testing.T) {
	ptyErr := errors.New("PTY close failure")
	tmuxErr := errors.New("tmux close failure")
	pty := newRouterFakeBackend(agent.BackendPTY)
	pty.closeErr = ptyErr
	tmux := newRouterFakeBackend(agent.BackendTmux)
	tmux.closeErr = tmuxErr
	router, err := newBackendRouter(context.Background(), pty, tmux)
	if err != nil {
		t.Fatal(err)
	}

	first := router.Close(context.Background())
	second := router.Close(context.Background())
	if !errors.Is(first, ptyErr) || !errors.Is(first, tmuxErr) || !errors.Is(second, ptyErr) || !errors.Is(second, tmuxErr) {
		t.Fatalf("joined close errors = first %v second %v", first, second)
	}
	pty.mu.Lock()
	ptyCloses := pty.closeCalls
	pty.mu.Unlock()
	tmux.mu.Lock()
	tmuxCloses := tmux.closeCalls
	tmux.mu.Unlock()
	if ptyCloses != 2 || tmuxCloses != 2 {
		t.Fatalf("Close calls = PTY %d tmux %d, want two attempts each", ptyCloses, tmuxCloses)
	}
}

func TestBackendRouterCloseStatusRemainsPerConcreteBackend(t *testing.T) {
	ptyBackend := newRouterFakeBackend(agent.BackendPTY)
	tmuxBackend := newRouterFakeBackend(agent.BackendTmux)
	tmuxBackend.closeErr = errors.New("tmux cleanup failed")
	router, err := newBackendRouter(context.Background(), ptyBackend, tmuxBackend)
	if err != nil {
		t.Fatal(err)
	}
	if err := router.Close(context.Background()); err == nil {
		t.Fatal("mixed close error was lost")
	}
	if succeeded, known := router.backendCloseStatus(agent.BackendPTY); !known || !succeeded {
		t.Fatalf("PTY close status = succeeded %t known %t", succeeded, known)
	}
	if succeeded, known := router.backendCloseStatus(agent.BackendTmux); !known || succeeded {
		t.Fatalf("tmux close status = succeeded %t known %t", succeeded, known)
	}
	if succeeded, known := router.backendCloseStatus("missing"); known || succeeded {
		t.Fatalf("missing close status = succeeded %t known %t", succeeded, known)
	}
}

func TestBackendRouterCloseRetriesOnlyFlakyBackendUntilItSucceeds(t *testing.T) {
	flakyErr := errors.New("temporary tmux close failure")
	pty := newRouterFakeBackend(agent.BackendPTY)
	tmux := newRouterFakeBackend(agent.BackendTmux)
	tmux.closeErrors = []error{flakyErr, nil}
	router, err := newBackendRouter(context.Background(), pty, tmux)
	if err != nil {
		t.Fatal(err)
	}

	if first := router.Close(context.Background()); !errors.Is(first, flakyErr) {
		t.Fatalf("first Close error = %v, want temporary failure", first)
	}
	if second := router.Close(context.Background()); second != nil {
		t.Fatalf("retry Close error = %v, want success", second)
	}
	if third := router.Close(context.Background()); third != nil {
		t.Fatalf("Close after complete cleanup = %v", third)
	}
	pty.mu.Lock()
	ptyCalls := pty.closeCalls
	pty.mu.Unlock()
	tmux.mu.Lock()
	tmuxCalls := tmux.closeCalls
	tmux.mu.Unlock()
	if ptyCalls != 1 || tmuxCalls != 2 {
		t.Fatalf("retry Close calls = PTY %d tmux %d, want successful PTY once and flaky tmux twice", ptyCalls, tmuxCalls)
	}
}

func TestBackendRouterSerializesConcurrentCloseAttempts(t *testing.T) {
	temporaryErr := errors.New("first Close attempt failed")
	started := make(chan int, 2)
	releaseFirst := make(chan struct{})
	backend := &routerSequencedCloseBackend{
		routerFakeBackend: newRouterFakeBackend(agent.BackendPTY),
		started:           started,
		releaseFirst:      releaseFirst,
		temporaryErr:      temporaryErr,
	}
	router, err := newBackendRouter(context.Background(), backend)
	if err != nil {
		t.Fatal(err)
	}

	type closeResult struct {
		attempt int
		err     error
	}
	results := make(chan closeResult, 2)
	go func() { results <- closeResult{attempt: 1, err: router.Close(context.Background())} }()
	select {
	case call := <-started:
		if call != 1 {
			t.Fatalf("first backend Close call = %d", call)
		}
	case <-time.After(time.Second):
		t.Fatal("first backend Close did not start")
	}

	secondInvoked := make(chan struct{})
	go func() {
		close(secondInvoked)
		results <- closeResult{attempt: 2, err: router.Close(context.Background())}
	}()
	<-secondInvoked
	select {
	case call := <-started:
		close(releaseFirst)
		t.Fatalf("concurrent router Close entered backend call %d before attempt 1 completed", call)
	case <-time.After(150 * time.Millisecond):
	}
	close(releaseFirst)
	select {
	case call := <-started:
		if call != 2 {
			t.Fatalf("retry backend Close call = %d", call)
		}
	case <-time.After(time.Second):
		t.Fatal("serialized retry did not start after attempt 1 completed")
	}

	got := make(map[int]error, 2)
	for len(got) < 2 {
		select {
		case result := <-results:
			got[result.attempt] = result.err
		case <-time.After(time.Second):
			t.Fatal("concurrent router Close attempts did not complete")
		}
	}
	if !errors.Is(got[1], temporaryErr) || got[2] != nil {
		t.Fatalf("serialized Close results = first %v second %v", got[1], got[2])
	}
	if err := router.Close(context.Background()); err != nil {
		t.Fatalf("Close after serialized retry = %v", err)
	}
	backend.mu.Lock()
	calls := backend.closeCalls
	maximum := backend.maxActive
	backend.mu.Unlock()
	if calls != 2 || maximum != 1 {
		t.Fatalf("backend Close calls = %d, maximum concurrent = %d; want 2 and 1", calls, maximum)
	}
}

func TestBackendRouterCloseRunsBackendsConcurrentlyWithOneSharedDeadline(t *testing.T) {
	started := make(chan string, 2)
	barrier := make(chan struct{})
	ptyFailure := errors.New("blocked PTY close")
	tmuxFailure := errors.New("blocked tmux close")
	pty := &routerBlockingCloseBackend{
		routerFakeBackend: newRouterFakeBackend(agent.BackendPTY),
		started:           started,
		barrier:           barrier,
		failure:           ptyFailure,
	}
	tmux := &routerBlockingCloseBackend{
		routerFakeBackend: newRouterFakeBackend(agent.BackendTmux),
		started:           started,
		barrier:           barrier,
		failure:           tmuxFailure,
	}
	router, err := newBackendRouter(context.Background(), pty, tmux)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	wantDeadline, _ := ctx.Deadline()
	finished := make(chan error, 1)
	go func() { finished <- router.Close(ctx) }()

	concurrent := true
	seen := make(map[string]bool, 2)
	for len(seen) < 2 {
		select {
		case name := <-started:
			seen[name] = true
		case <-time.After(time.Second):
			concurrent = false
			close(barrier)
			cancel()
			goto closed
		}
	}
	close(barrier)
	cancel()

closed:
	var closeErr error
	select {
	case closeErr = <-finished:
	case <-time.After(time.Second):
		t.Fatal("router Close did not join blocking backend goroutines")
	}
	if !concurrent {
		t.Fatal("backend Close calls were serialized; the second did not reach the barrier")
	}
	if !seen[agent.BackendPTY] || !seen[agent.BackendTmux] {
		t.Fatalf("backends reaching Close barrier = %#v", seen)
	}
	if !errors.Is(closeErr, ptyFailure) || !errors.Is(closeErr, tmuxFailure) || !errors.Is(closeErr, context.Canceled) {
		t.Fatalf("joined concurrent Close error = %v", closeErr)
	}
	for name, backend := range map[string]*routerBlockingCloseBackend{
		agent.BackendPTY:  pty,
		agent.BackendTmux: tmux,
	} {
		backend.mu.Lock()
		calls := backend.closeCalls
		deadline := backend.deadline
		backend.mu.Unlock()
		if calls != 1 {
			t.Fatalf("%s Close calls = %d, want 1", name, calls)
		}
		if !deadline.Equal(wantDeadline) {
			t.Fatalf("%s Close deadline = %v, want caller's shared %v", name, deadline, wantDeadline)
		}
	}
}

func TestBackendRouterPropagatesPTYAttachSentinel(t *testing.T) {
	pty := newRouterFakeBackend(agent.BackendPTY)
	pty.attachErr = terminal.ErrNotAttachable
	router, err := newBackendRouter(context.Background(), pty)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = router.Close(context.Background()) })
	if _, err := router.Start(context.Background(), agent.Spec{
		ID: "pty", Name: "PTY", Command: []string{"runner"}, Backend: agent.BackendPTY,
	}, terminal.Size{}); err != nil {
		t.Fatal(err)
	}

	if _, err := router.AttachCommand(context.Background(), "PTY"); !errors.Is(err, terminal.ErrNotAttachable) {
		t.Fatalf("AttachCommand error = %v", err)
	}
}

func TestTUIBackendAdapterAppendsExactlyOneCarriageReturn(t *testing.T) {
	backend := newRouterFakeBackend(agent.BackendPTY)
	router, err := newBackendRouter(context.Background(), backend)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = router.Close(context.Background()) })
	if _, err := router.Start(context.Background(), agent.Spec{
		ID: "input", Name: "Input", Command: []string{"runner"}, Backend: agent.BackendPTY,
	}, terminal.Size{}); err != nil {
		t.Fatal(err)
	}
	adapter := &tuiBackendAdapter{router: router}

	for _, value := range []string{"Y", "", "line\ninside"} {
		if err := adapter.SendInput("input", value); err != nil {
			t.Fatalf("SendInput(%q): %v", value, err)
		}
	}
	backend.mu.Lock()
	got := make([]string, len(backend.sends))
	for index, call := range backend.sends {
		got[index] = string(call.data)
	}
	backend.mu.Unlock()
	want := []string{"Y\r", "\r", "line\ninside\r"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("adapter inputs = %#v, want %#v", got, want)
	}
}

func TestBackendRouterAndDesktopRuntimeRouteSafeLineWithoutRawFallback(t *testing.T) {
	backend := newRouterLineBackend(agent.BackendPTY)
	router, err := newBackendRouter(context.Background(), backend)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = router.Close(context.Background()) })
	if _, err := router.Start(context.Background(), agent.Spec{
		ID: "line", Name: "Line", Command: []string{"runner"}, Backend: agent.BackendPTY,
	}, terminal.Size{}); err != nil {
		t.Fatal(err)
	}

	adapter := &tuiBackendAdapter{router: router}
	if err := adapter.SendLine("line", "ordinary text"); err != nil {
		t.Fatalf("tui SendLine: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runtime := &DesktopRuntime{ctx: ctx, cancel: cancel, router: router}
	if err := runtime.SendLine(context.Background(), "LINE", "desktop text"); err != nil {
		t.Fatalf("desktop SendLine: %v", err)
	}

	backend.mu.Lock()
	lineCalls := append([]routerLineCall(nil), backend.lineCalls...)
	rawCalls := len(backend.sends)
	backend.mu.Unlock()
	want := []routerLineCall{{id: "line", line: "ordinary text"}, {id: "LINE", line: "desktop text"}}
	if !reflect.DeepEqual(lineCalls, want) {
		t.Fatalf("line calls = %#v, want %#v", lineCalls, want)
	}
	if rawCalls != 0 {
		t.Fatalf("safe line path made %d raw Send call(s)", rawCalls)
	}

	cancelled, cancelRequest := context.WithCancel(context.Background())
	cancelRequest()
	if err := router.SendLine(cancelled, "line", "cancelled text"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled SendLine error = %v", err)
	}
	backend.mu.Lock()
	afterCancelled := len(backend.lineCalls)
	backend.mu.Unlock()
	if afterCancelled != len(want) {
		t.Fatalf("cancelled SendLine reached backend: calls=%d", afterCancelled)
	}
}

func TestBackendRouterSendLineRejectsBackendWithoutAtomicCapability(t *testing.T) {
	backend := newRouterFakeBackend(agent.BackendPTY)
	router, err := newBackendRouter(context.Background(), backend)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = router.Close(context.Background()) })
	if _, err := router.Start(context.Background(), agent.Spec{
		ID: "legacy", Name: "Legacy", Command: []string{"runner"}, Backend: agent.BackendPTY,
	}, terminal.Size{}); err != nil {
		t.Fatal(err)
	}
	const secret = "sk-fixturevalue123456"
	err = router.SendLine(context.Background(), "legacy", secret)
	if !errors.Is(err, terminal.ErrLineUnsupported) {
		t.Fatalf("unsupported SendLine error = %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("unsupported error exposed input: %q", err)
	}
	backend.mu.Lock()
	rawCalls := len(backend.sends)
	backend.mu.Unlock()
	if rawCalls != 0 {
		t.Fatalf("unsupported line fell back to %d raw Send call(s)", rawCalls)
	}
}
