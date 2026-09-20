package record

import (
	"io"
	"strings"
	"sync"
	"time"

	"github.com/Hocsman/Relayer/internal/terminal"
)

// defaultQueueSize bounds one session's pending frames. A saturated queue
// drops rather than blocks, so this is the only memory a stalled disk costs.
const defaultQueueSize = 1024

// maximumQueueSize keeps a configured queue bounded.
const maximumQueueSize = 65536

// MultiplexerOptions configures the recorder bridge between the terminal
// backends and the store.
type MultiplexerOptions struct {
	Store       *Store
	Config      Config
	RunID       string
	Diagnostics io.Writer
	QueueSize   int
}

// Multiplexer implements terminal.Recorder by owning one goroutine and one
// bounded queue per session. Every method is safe on a nil receiver, so a
// caller never needs a nil check around recording.
type Multiplexer struct {
	store         *Store
	config        Config
	runID         string
	queueSize     int
	diagnostics   io.Writer
	diagnosticsMu sync.Mutex
	mu            sync.Mutex
	sessions      map[terminal.SessionID]*recordingSession
	wg            sync.WaitGroup
	closed        bool
}

var _ terminal.Recorder = (*Multiplexer)(nil)

// recordEvent is one queued transcript entry. Data is already a private copy:
// the PTY read buffer is reused as soon as the caller returns.
type recordEvent struct {
	at    time.Time
	kind  Kind
	data  []byte
	size  terminal.Size
	label string
}

// recordingSession owns one queue. Its mutex is held only for a non-blocking
// channel send and small field updates, never across a write to disk.
type recordingSession struct {
	mu       sync.Mutex
	closed   bool
	dropped  int
	endedAt  time.Time
	exitCode *int
	events   chan recordEvent
	options  CreateOptions
}

// NewMultiplexer returns nil when recording is disabled or has no store; the
// nil-safe methods make that an ordinary, silent no-op for every caller.
func NewMultiplexer(options MultiplexerOptions) *Multiplexer {
	if options.Store == nil || !options.Config.Enabled {
		return nil
	}
	queueSize := options.QueueSize
	if queueSize <= 0 {
		queueSize = defaultQueueSize
	}
	if queueSize > maximumQueueSize {
		queueSize = maximumQueueSize
	}
	return &Multiplexer{
		store:       options.Store,
		config:      options.Config,
		runID:       options.RunID,
		queueSize:   queueSize,
		diagnostics: options.Diagnostics,
		sessions:    make(map[terminal.SessionID]*recordingSession),
	}
}

// StartSession opens a transcript for a session. The store is only touched by
// the session goroutine, so no filesystem work happens under a mutex a PTY
// read loop can wait on.
func (m *Multiplexer) StartSession(info terminal.Info, size terminal.Size, at time.Time) {
	if m == nil {
		return
	}
	normalized := size.Normalize()
	if at.IsZero() {
		at = time.Now()
	}
	session := &recordingSession{
		events: make(chan recordEvent, m.queueSize),
		options: CreateOptions{
			RunID:     m.runID,
			SessionID: info.ID,
			AgentID:   info.Name,
			Name:      info.Name,
			Backend:   info.Backend,
			Adapter:   info.Adapter,
			Title:     info.DisplayCommand,
			Width:     normalized.Columns,
			Height:    normalized.Rows,
			StartedAt: at,
		},
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	if _, exists := m.sessions[info.ID]; exists {
		m.mu.Unlock()
		return
	}
	m.sessions[info.ID] = session
	m.wg.Add(1)
	m.mu.Unlock()

	go m.run(session)
}

// RecordOutput queues terminal output. It never blocks the PTY read loop.
func (m *Multiplexer) RecordOutput(id terminal.SessionID, at time.Time, data []byte) {
	if m == nil || len(data) == 0 {
		return
	}
	m.offer(id, recordEvent{at: at, kind: KindOutput, data: copyBytes(data)})
}

// RecordInput queues operator input when the configuration allows it. With
// redaction on, printable bytes are masked so a transcript keeps the shape of
// what was typed without keeping the secret itself.
func (m *Multiplexer) RecordInput(id terminal.SessionID, at time.Time, data []byte) {
	if m == nil || len(data) == 0 || !m.config.RecordInput {
		return
	}
	payload := copyBytes(data)
	if m.config.Redact {
		payload = redactInput(payload)
	}
	m.offer(id, recordEvent{at: at, kind: KindInput, data: payload})
}

// RecordResize queues a geometry change.
func (m *Multiplexer) RecordResize(id terminal.SessionID, at time.Time, size terminal.Size) {
	if m == nil {
		return
	}
	m.offer(id, recordEvent{at: at, kind: KindResize, size: size.Normalize()})
}

// FinishSession closes the queue and returns immediately. The session
// goroutine drains what is still queued before it closes the transcript and
// rewrites the sidecar; Close joins that work.
func (m *Multiplexer) FinishSession(id terminal.SessionID, at time.Time, exitCode *int) {
	if m == nil {
		return
	}
	m.mu.Lock()
	session, known := m.sessions[id]
	if known {
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	if !known {
		return
	}
	session.finish(at, exitCode)
}

// Close finishes every open session and joins their goroutines.
func (m *Multiplexer) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		m.wg.Wait()
		return nil
	}
	m.closed = true
	pending := make([]*recordingSession, 0, len(m.sessions))
	for id, session := range m.sessions {
		pending = append(pending, session)
		delete(m.sessions, id)
	}
	m.mu.Unlock()

	for _, session := range pending {
		session.finish(time.Time{}, nil)
	}
	m.wg.Wait()
	return nil
}

// offer hands one event to a session queue without ever blocking.
func (m *Multiplexer) offer(id terminal.SessionID, event recordEvent) {
	m.mu.Lock()
	session, known := m.sessions[id]
	m.mu.Unlock()
	if !known {
		return
	}
	session.offer(event)
}

// run owns one transcript for its whole lifetime.
func (m *Multiplexer) run(session *recordingSession) {
	defer m.wg.Done()

	metadata, writer, err := m.store.Create(session.options)
	if err != nil {
		m.report("cannot open the transcript for session " + slug(session.options.SessionID))
	}
	for event := range session.events {
		m.apply(writer, event)
		// The queue has caught up: make the loss visible in the artifact.
		if len(session.events) == 0 {
			m.flushDropped(session, writer, event.at)
		}
	}
	endedAt, exitCode := session.outcome()
	m.flushDropped(session, writer, endedAt)
	if writer == nil {
		return
	}
	if _, err := m.store.Finish(metadata.ID, endedAt, exitCode); err != nil {
		_ = writer.Close()
		m.report("cannot finalize the transcript for session " + slug(session.options.SessionID))
	}
}

func (m *Multiplexer) apply(writer *Writer, event recordEvent) {
	if writer == nil {
		return
	}
	var err error
	switch event.kind {
	case KindOutput:
		err = writer.WriteOutput(event.at, event.data)
	case KindInput:
		err = writer.WriteInput(event.at, event.data)
	case KindResize:
		err = writer.WriteResize(event.at, event.size.Columns, event.size.Rows)
	case KindMarker:
		err = writer.WriteMarker(event.at, event.label)
	}
	if err != nil {
		m.report("cannot append a transcript frame")
	}
}

// flushDropped writes one marker naming how many frames a saturated queue lost
// and accounts for them in the transcript statistics.
func (m *Multiplexer) flushDropped(session *recordingSession, writer *Writer, at time.Time) {
	lost := session.takeDropped()
	if lost == 0 {
		return
	}
	writer.RecordDropped(lost)
	m.apply(writer, recordEvent{at: at, kind: KindMarker, label: droppedMarker(lost)})
}

// report writes one bounded diagnostic line. The dedicated mutex exists only
// because an io.Writer is not required to be concurrency safe.
func (m *Multiplexer) report(message string) {
	if m.diagnostics == nil {
		return
	}
	m.diagnosticsMu.Lock()
	defer m.diagnosticsMu.Unlock()
	_, _ = io.WriteString(m.diagnostics, "record: "+message+"\n")
}

func (s *recordingSession) offer(event recordEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.events <- event:
	default:
		s.dropped++
	}
}

func (s *recordingSession) finish(at time.Time, exitCode *int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	s.endedAt = at
	s.exitCode = exitCode
	close(s.events)
}

func (s *recordingSession) outcome() (time.Time, *int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.endedAt, s.exitCode
}

func (s *recordingSession) takeDropped() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := s.dropped
	s.dropped = 0
	return count
}

func copyBytes(data []byte) []byte {
	result := make([]byte, len(data))
	copy(result, data)
	return result
}

// redactInput masks every printable byte and keeps control bytes, so replay
// still shows when a line was submitted or an interrupt was sent without
// retaining what was typed.
func redactInput(data []byte) []byte {
	var builder strings.Builder
	builder.Grow(len(data))
	for _, symbol := range data {
		if symbol < 0x20 || symbol == 0x7f {
			builder.WriteByte(symbol)
			continue
		}
		builder.WriteByte('*')
	}
	return []byte(builder.String())
}
