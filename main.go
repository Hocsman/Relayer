// Relayer is a small human-in-the-loop terminal orchestrator. It runs each
// command behind a PTY, watches its output for interactive prompts, and relays
// the supervisor's answer back to the process that is waiting for it.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/acarl005/stripansi"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/creack/pty"
	"gopkg.in/yaml.v3"
)

const (
	defaultRingCapacity = 256 * 1024
	detectionWindowSize = 16 * 1024
	maxANSICarrySize    = 4 * 1024
	maxSystemLogLines   = 200
	minTerminalWidth    = 30
	minTerminalHeight   = 10
	defaultConfigPath   = "config.yaml"
)

var errSessionClosed = errors.New("session PTY fermée")

// RingBuffer is a concurrency-safe, byte-bounded circular buffer. PTY output
// can be unbounded, so retaining only a recent window prevents long-running
// sessions from consuming all available memory.
type RingBuffer struct {
	mu     sync.RWMutex
	data   []byte
	start  int
	length int
}

func NewRingBuffer(capacity int) *RingBuffer {
	if capacity < 1 {
		capacity = 1
	}
	return &RingBuffer{data: make([]byte, capacity)}
}

func (r *RingBuffer) Capacity() int {
	return len(r.data)
}

func (r *RingBuffer) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.length
}

// Write implements io.Writer. If p is larger than the whole buffer, only its
// newest bytes are retained.
func (r *RingBuffer) Write(p []byte) (int, error) {
	written := len(p)
	if written == 0 {
		return 0, nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	capacity := len(r.data)
	if len(p) >= capacity {
		copy(r.data, p[len(p)-capacity:])
		r.start = 0
		r.length = capacity
		return written, nil
	}

	end := (r.start + r.length) % capacity
	first := minInt(len(p), capacity-end)
	copy(r.data[end:], p[:first])
	copy(r.data, p[first:])

	if r.length+len(p) <= capacity {
		r.length += len(p)
	} else {
		overflow := r.length + len(p) - capacity
		r.start = (r.start + overflow) % capacity
		r.length = capacity
	}

	return written, nil
}

// Bytes returns an ordered copy. Callers can safely retain or mutate it.
func (r *RingBuffer) Bytes() []byte {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]byte, r.length)
	if r.length == 0 {
		return result
	}

	first := minInt(r.length, len(r.data)-r.start)
	copy(result, r.data[r.start:r.start+first])
	copy(result[first:], r.data[:r.length-first])
	return result
}

func (r *RingBuffer) String() string {
	return string(r.Bytes())
}

// PromptPattern describes one interactive prompt that needs human input.
type PromptPattern struct {
	Name        string
	Description string
	Expression  string
	Sensitive   bool
}

type compiledPromptPattern struct {
	PromptPattern
	regex *regexp.Regexp
}

var defaultPromptPatterns = []PromptPattern{
	{
		Name:        "overwrite",
		Description: "confirmation d'écrasement",
		Expression:  `(?i)overwrite.*\[y/n\]`,
	},
	{
		Name:        "confirmation",
		Description: "confirmation oui/non",
		Expression:  `(?i)\[[yn]/[yn]\]`,
	},
	{
		Name:        "password",
		Description: "saisie d'un mot de passe",
		Expression:  `(?im)password:[[:space:]]*$`,
		Sensitive:   true,
	},
	{
		Name:        "continue",
		Description: "confirmation de poursuite",
		Expression:  `(?i)do you want to continue`,
	},
}

// ConfigPattern is the YAML representation requested from users. Internal
// names and password masking are derived so the file stays limited to the two
// documented keys: pattern and description.
type ConfigPattern struct {
	Pattern     string `yaml:"pattern"`
	Description string `yaml:"description"`
}

type configFile struct {
	InterceptPatterns []ConfigPattern `yaml:"intercept_patterns"`
}

// loadPromptPatterns reads the configuration before any PTY is started. It
// accepts both a direct list and the intercept_patterns wrapper documented in
// the README. A missing file is populated with the built-in defaults.
func loadPromptPatterns(path string) ([]PromptPattern, bool, error) {
	if strings.TrimSpace(path) == "" {
		return nil, false, errors.New("le chemin du fichier de configuration est vide")
	}

	data, err := os.ReadFile(path)
	created := false
	if errors.Is(err, os.ErrNotExist) {
		created, err = createDefaultConfig(path)
		if err != nil {
			return nil, false, err
		}
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, created, fmt.Errorf("lecture de %s: %w", path, err)
	}

	configured, err := decodeConfigPatterns(data)
	if err != nil {
		return nil, created, fmt.Errorf("configuration %s invalide: %w", path, err)
	}
	patterns, err := validateConfigPatterns(configured)
	if err != nil {
		return nil, created, fmt.Errorf("configuration %s invalide: %w", path, err)
	}
	return patterns, created, nil
}

func createDefaultConfig(path string) (bool, error) {
	configured := make([]ConfigPattern, 0, len(defaultPromptPatterns))
	for _, pattern := range defaultPromptPatterns {
		configured = append(configured, ConfigPattern{
			Pattern:     pattern.Expression,
			Description: pattern.Description,
		})
	}
	payload, err := yaml.Marshal(configFile{InterceptPatterns: configured})
	if err != nil {
		return false, fmt.Errorf("sérialisation de la configuration par défaut: %w", err)
	}
	payload = append([]byte("# Patterns d'interception de Relayer.\n"), payload...)

	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return false, fmt.Errorf("création du dossier de configuration %s: %w", directory, err)
	}

	// The temporary file is fully written and synced before an atomic hard-link
	// publishes it at path. Link never overwrites an existing user file.
	temporary, err := os.CreateTemp(directory, ".relayer-config-*.tmp")
	if err != nil {
		return false, fmt.Errorf("création temporaire de %s: %w", path, err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)

	if err := temporary.Chmod(0o644); err != nil {
		_ = temporary.Close()
		return false, fmt.Errorf("permissions de %s: %w", temporaryPath, err)
	}
	if _, err := temporary.Write(payload); err != nil {
		_ = temporary.Close()
		return false, fmt.Errorf("écriture de %s: %w", temporaryPath, err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return false, fmt.Errorf("synchronisation de %s: %w", temporaryPath, err)
	}
	if err := temporary.Close(); err != nil {
		return false, fmt.Errorf("fermeture de %s: %w", temporaryPath, err)
	}
	if err := os.Link(temporaryPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, nil
		}
		// Some network/removable filesystems do not support hard links. Keep
		// the no-overwrite guarantee with an exclusive direct creation there.
		return createConfigExclusively(path, payload, err)
	}
	return true, nil
}

func createConfigExclusively(path string, payload []byte, linkErr error) (bool, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("publication de %s (lien indisponible: %v): %w", path, linkErr, err)
	}
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(path)
	}
	if _, err := file.Write(payload); err != nil {
		cleanup()
		return false, fmt.Errorf("écriture de %s: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		cleanup()
		return false, fmt.Errorf("synchronisation de %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return false, fmt.Errorf("fermeture de %s: %w", path, err)
	}
	return true, nil
}

func decodeConfigPatterns(data []byte) ([]ConfigPattern, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	if len(document.Content) == 0 {
		return nil, errors.New("le document YAML est vide")
	}

	root := document.Content[0]
	if err := validateConfigScalarTypes(root); err != nil {
		return nil, err
	}
	switch root.Kind {
	case yaml.SequenceNode:
		var direct []ConfigPattern
		if err := decodeStrictYAML(data, &direct); err != nil {
			return nil, err
		}
		return direct, nil
	case yaml.MappingNode:
		var wrapped configFile
		if err := decodeStrictYAML(data, &wrapped); err != nil {
			return nil, err
		}
		return wrapped.InterceptPatterns, nil
	default:
		return nil, errors.New("la racine YAML doit être une liste ou contenir intercept_patterns")
	}
}

func validateConfigScalarTypes(root *yaml.Node) error {
	sequence := root
	if root.Kind == yaml.MappingNode {
		sequence = nil
		for index := 0; index+1 < len(root.Content); index += 2 {
			if root.Content[index].Value == "intercept_patterns" {
				sequence = root.Content[index+1]
				break
			}
		}
	}
	if sequence == nil || sequence.Kind != yaml.SequenceNode {
		return nil
	}

	for entryIndex, entry := range sequence.Content {
		if entry.Kind != yaml.MappingNode {
			continue
		}
		for fieldIndex := 0; fieldIndex+1 < len(entry.Content); fieldIndex += 2 {
			name := entry.Content[fieldIndex].Value
			if name != "pattern" && name != "description" {
				continue
			}
			value := entry.Content[fieldIndex+1]
			if value.Kind == yaml.AliasNode && value.Alias != nil {
				value = value.Alias
			}
			if value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
				return fmt.Errorf("entrée %d: %s doit être une chaîne YAML", entryIndex+1, name)
			}
		}
	}
	return nil
}

func decodeStrictYAML(data []byte, target any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("plusieurs documents YAML ne sont pas autorisés")
		}
		return err
	}
	return nil
}

func validateConfigPatterns(configured []ConfigPattern) ([]PromptPattern, error) {
	if len(configured) == 0 {
		return nil, errors.New("aucun pattern d'interception n'est défini")
	}

	patterns := make([]PromptPattern, 0, len(configured))
	for index, pattern := range configured {
		if strings.TrimSpace(pattern.Pattern) == "" {
			return nil, fmt.Errorf("entrée %d: pattern manquant", index+1)
		}
		if strings.TrimSpace(pattern.Description) == "" {
			return nil, fmt.Errorf("entrée %d: description manquante", index+1)
		}
		if _, err := regexp.Compile(pattern.Pattern); err != nil {
			return nil, fmt.Errorf("entrée %d: regex invalide: %w", index+1, err)
		}
		patterns = append(patterns, PromptPattern{
			Name:        inferPatternName(pattern, index),
			Description: strings.TrimSpace(pattern.Description),
			Expression:  pattern.Pattern,
			Sensitive:   isSensitiveConfigPattern(pattern),
		})
	}
	return patterns, nil
}

func inferPatternName(pattern ConfigPattern, index int) string {
	text := strings.ToLower(pattern.Pattern + " " + pattern.Description)
	switch {
	case isSensitiveConfigPattern(pattern):
		return "password"
	case strings.Contains(text, "overwrite"), strings.Contains(text, "écras"):
		return "overwrite"
	case strings.Contains(text, "continue"), strings.Contains(text, "poursuite"):
		return "continue"
	case strings.Contains(text, "confirm"), strings.Contains(text, "oui/non"):
		return "confirmation"
	default:
		return fmt.Sprintf("config-%d", index+1)
	}
}

func isSensitiveConfigPattern(pattern ConfigPattern) bool {
	return isSensitiveText(pattern.Pattern + " " + pattern.Description)
}

func isSensitiveText(value string) bool {
	text := strings.ToLower(value)
	return strings.Contains(text, "password") ||
		strings.Contains(text, "passphrase") ||
		strings.Contains(text, "mot de passe") ||
		strings.Contains(text, "credential") ||
		strings.Contains(text, "secret") ||
		strings.Contains(text, "sensitive") ||
		strings.Contains(text, "api key") ||
		strings.Contains(text, "api_key") ||
		strings.Contains(text, "apikey") ||
		strings.Contains(text, "access key") ||
		strings.Contains(text, "token") ||
		strings.Contains(text, "pin:") ||
		strings.Contains(text, "pin code") ||
		strings.Contains(text, "code pin") ||
		strings.Contains(text, "otp") ||
		strings.Contains(text, "clé api") ||
		strings.Contains(text, "cle api")
}

// Bubble Tea messages emitted by the asynchronous PTY layer.
type SessionOutputMsg struct {
	SessionID int
	Content   string
}

type PromptDetectedMsg struct {
	SessionID   int
	Pattern     string
	Description string
	Match       string
	Sensitive   bool
}

type SessionExitedMsg struct {
	SessionID int
	Err       error
}

type SessionErrorMsg struct {
	SessionID int
	Err       error
}

type inputDeliveredMsg struct {
	SessionID int
	Prompt    PromptDetectedMsg
	Err       error
}

type managerStoppedMsg struct{}

// managerEventMsg distinguishes a completed channel subscription from other
// Bubble Tea commands. This keeps exactly one channel waiter active at a time.
type managerEventMsg struct {
	Message tea.Msg
}

type eventEmitter func(message tea.Msg, essential bool) bool

// Interceptor owns the output ring and the rolling, ANSI-free detection
// window for one session. Consume is safe to call concurrently with
// Acknowledge, although a session normally has only one reader goroutine.
type Interceptor struct {
	sessionID int
	patterns  []compiledPromptPattern
	output    *RingBuffer
	emit      eventEmitter

	mu         sync.Mutex
	detectTail string
	ansiCarry  string
	blocked    bool
}

func NewInterceptor(
	sessionID int,
	patterns []PromptPattern,
	ringCapacity int,
	emit eventEmitter,
) (*Interceptor, error) {
	compiled := make([]compiledPromptPattern, 0, len(patterns))
	for _, pattern := range patterns {
		re, err := regexp.Compile(pattern.Expression)
		if err != nil {
			return nil, fmt.Errorf("regex %q invalide: %w", pattern.Name, err)
		}
		compiled = append(compiled, compiledPromptPattern{
			PromptPattern: pattern,
			regex:         re,
		})
	}

	if emit == nil {
		emit = func(tea.Msg, bool) bool { return true }
	}

	return &Interceptor{
		sessionID: sessionID,
		patterns:  compiled,
		output:    NewRingBuffer(ringCapacity),
		emit:      emit,
	}, nil
}

// Run continuously reads the PTY. The read itself is intentionally blocking:
// running it in its own goroutine avoids polling and never blocks the TUI.
func (i *Interceptor) Run(ctx context.Context, reader io.Reader) error {
	buffer := make([]byte, 4096)
	for {
		count, err := reader.Read(buffer)
		if count > 0 {
			i.Consume(buffer[:count])
		}

		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			select {
			case <-ctx.Done():
				return nil
			default:
				return err
			}
		}
	}
}

// Consume strips ANSI sequences before both display and regex matching. A
// partial escape sequence at the end of a read is carried into the next read.
func (i *Interceptor) Consume(chunk []byte) {
	if len(chunk) == 0 {
		return
	}

	i.mu.Lock()
	complete, carry := splitIncompleteANSI(i.ansiCarry + string(chunk))
	if len(carry) > maxANSICarrySize {
		// A malformed OSC/CSI must not hide all subsequent process output or
		// grow forever. Control bytes are neutralized below before rendering.
		complete += carry
		carry = ""
	}
	i.ansiCarry = carry
	clean := sanitizeTerminalText(stripansi.Strip(complete))

	if clean != "" {
		_, _ = i.output.Write([]byte(clean))
		i.detectTail += clean
		if len(i.detectTail) > detectionWindowSize {
			i.detectTail = i.detectTail[len(i.detectTail)-detectionWindowSize:]
		}
	}

	var detected *PromptDetectedMsg
	if !i.blocked && clean != "" {
		for _, pattern := range i.patterns {
			match := pattern.regex.FindString(i.detectTail)
			if match == "" {
				continue
			}
			i.blocked = true
			detected = &PromptDetectedMsg{
				SessionID:   i.sessionID,
				Pattern:     pattern.Name,
				Description: pattern.Description,
				Match:       match,
				Sensitive:   pattern.Sensitive || isSensitiveText(match),
			}
			break
		}
	}
	i.mu.Unlock()

	// Output notifications are lightweight: the model reads the newest snapshot
	// directly from the bounded ring. Prompts themselves are essential.
	if clean != "" {
		i.emit(SessionOutputMsg{SessionID: i.sessionID}, false)
	}
	if detected != nil {
		i.emit(*detected, true)
	}
}

// Acknowledge rearms prompt detection after the supervisor's answer has been
// written. Clearing only the detection tail prevents the old prompt from
// immediately firing again while preserving the visible output history.
func (i *Interceptor) Acknowledge() {
	i.mu.Lock()
	i.blocked = false
	i.detectTail = ""
	i.mu.Unlock()
}

func (i *Interceptor) IsBlocked() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.blocked
}

func (i *Interceptor) Output() string {
	return i.output.String()
}

// Reblock restores the old waiting state when writing to the PTY fails.
func (i *Interceptor) Reblock() {
	i.mu.Lock()
	i.blocked = true
	i.mu.Unlock()
}

// splitIncompleteANSI retains only a trailing, incomplete CSI/OSC sequence.
// Complete escape sequences remain in complete and are removed by stripansi.
func splitIncompleteANSI(input string) (complete string, carry string) {
	for offset := 0; offset < len(input); {
		if input[offset] != 0x1b {
			offset++
			continue
		}

		end, ok := ansiSequenceEnd(input, offset)
		if !ok {
			return input[:offset], input[offset:]
		}
		offset = end
	}
	return input, ""
}

func ansiSequenceEnd(input string, start int) (int, bool) {
	if start+1 >= len(input) {
		return 0, false
	}

	switch input[start+1] {
	case '[': // Control Sequence Introducer (CSI)
		for index := start + 2; index < len(input); index++ {
			if input[index] >= 0x40 && input[index] <= 0x7e {
				return index + 1, true
			}
		}
		return 0, false
	case ']': // Operating System Command (OSC), terminated by BEL or ST.
		for index := start + 2; index < len(input); index++ {
			if input[index] == 0x07 {
				return index + 1, true
			}
			if input[index] == 0x1b {
				if index+1 >= len(input) {
					return 0, false
				}
				if input[index+1] == '\\' {
					return index + 2, true
				}
			}
		}
		return 0, false
	default:
		// Most non-CSI escapes are two-byte sequences.
		return start + 2, true
	}
}

// sanitizeTerminalText neutralizes controls that could move the application's
// own cursor. Carriage returns used by progress bars become line breaks.
func sanitizeTerminalText(input string) string {
	input = strings.ReplaceAll(input, "\r\n", "\n")
	input = strings.ReplaceAll(input, "\r", "\n")
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || (r >= 0x20 && (r < 0x7f || r >= 0xa0)) {
			return r
		}
		return -1
	}, input)
}

// Session owns a command and the master side of its PTY.
type Session struct {
	ID          int
	Name        string
	Command     string
	cmd         *exec.Cmd
	ctx         context.Context
	cancel      context.CancelFunc
	interceptor *Interceptor
	done        chan struct{}

	fileMu       sync.RWMutex
	master       *os.File
	closePTYOnce sync.Once
	stopOnce     sync.Once
}

func (s *Session) Write(input string) error {
	s.fileMu.RLock()
	master := s.master
	s.fileMu.RUnlock()
	if master == nil {
		return errSessionClosed
	}
	// os.File permits Close concurrently with Write. Releasing fileMu first
	// lets Close unblock a saturated PTY write instead of deadlocking on it.
	_, err := io.WriteString(master, input)
	return err
}

func (s *Session) Resize(columns, rows int) error {
	columns = clampInt(columns, 1, 65535)
	rows = clampInt(rows, 1, 65535)

	s.fileMu.RLock()
	defer s.fileMu.RUnlock()
	if s.master == nil {
		return errSessionClosed
	}

	// Keep the descriptor protected for the duration of the ioctl. Unlike a
	// potentially blocking PTY write, Setsize completes immediately, so Close
	// can safely wait for this short critical section.
	return pty.Setsize(s.master, &pty.Winsize{
		Rows: uint16(rows),
		Cols: uint16(columns),
	})
}

func (s *Session) closePTY() {
	s.closePTYOnce.Do(func() {
		s.fileMu.Lock()
		master := s.master
		s.master = nil
		s.fileMu.Unlock()
		if master != nil {
			_ = master.Close()
		}
	})
}

func (s *Session) requestStop() {
	s.stopOnce.Do(func() {
		select {
		case <-s.done:
			s.cancel()
			s.closePTY()
			return
		default:
		}

		// StartWithSize creates a new session/process group on Unix. Signalling
		// the group also stops descendants spawned by a shell command.
		terminateProcessGroup(s.cmd)
		s.cancel()
		s.closePTY()
	})
}

func (s *Session) waitForStop() {
	select {
	case <-s.done:
		return
	case <-time.After(1500 * time.Millisecond):
	}

	killProcessGroup(s.cmd)
	select {
	case <-s.done:
	case <-time.After(500 * time.Millisecond):
	}
}

// SessionManager is the sole owner of process lifecycle and PTY descriptors.
type SessionManager struct {
	ctx    context.Context
	cancel context.CancelFunc
	events chan<- tea.Msg

	mu           sync.RWMutex
	sessions     map[int]*Session
	nextID       int
	closed       bool
	patterns     []PromptPattern
	ringCapacity int
	wg           sync.WaitGroup
	closeOnce    sync.Once
}

func NewSessionManager(
	parent context.Context,
	events chan<- tea.Msg,
	patterns []PromptPattern,
	ringCapacity int,
) (*SessionManager, error) {
	// Validate all configured expressions before starting any process.
	if _, err := NewInterceptor(-1, patterns, ringCapacity, nil); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(parent)
	return &SessionManager{
		ctx:          ctx,
		cancel:       cancel,
		events:       events,
		sessions:     make(map[int]*Session),
		patterns:     append([]PromptPattern(nil), patterns...),
		ringCapacity: ringCapacity,
	}, nil
}

func (m *SessionManager) Context() context.Context {
	return m.ctx
}

func (m *SessionManager) emit(message tea.Msg, essential bool) bool {
	if essential {
		select {
		case m.events <- message:
			return true
		case <-m.ctx.Done():
			return false
		}
	}

	select {
	case m.events <- message:
		return true
	case <-m.ctx.Done():
		return false
	default:
		return false
	}
}

func (m *SessionManager) Start(name, command string, columns, rows int) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errors.New("gestionnaire de sessions fermé")
	}

	sessionCtx, sessionCancel := context.WithCancel(m.ctx)
	cmd := exec.CommandContext(sessionCtx, "/bin/sh", "-c", command)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	master, err := pty.StartWithSize(cmd, &pty.Winsize{
		Rows: uint16(clampInt(rows, 1, 65535)),
		Cols: uint16(clampInt(columns, 1, 65535)),
	})
	if err != nil {
		sessionCancel()
		return nil, fmt.Errorf("démarrage de %s: %w", name, err)
	}

	sessionID := m.nextID
	m.nextID++
	interceptor, err := NewInterceptor(
		sessionID,
		m.patterns,
		m.ringCapacity,
		m.emit,
	)
	if err != nil {
		_ = master.Close()
		sessionCancel()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		return nil, err
	}

	session := &Session{
		ID:          sessionID,
		Name:        name,
		Command:     command,
		cmd:         cmd,
		ctx:         sessionCtx,
		cancel:      sessionCancel,
		interceptor: interceptor,
		done:        make(chan struct{}),
		master:      master,
	}
	m.sessions[sessionID] = session

	m.wg.Add(2)
	// Capture the descriptor before publishing the goroutine. Close may set
	// session.master to nil concurrently, while this local pointer remains safe
	// to read from (Close on *os.File unblocks that read).
	go m.readSession(session, master)
	go m.waitSession(session)
	return session, nil
}

func (m *SessionManager) readSession(session *Session, master *os.File) {
	defer m.wg.Done()
	defer session.closePTY()

	err := session.interceptor.Run(session.ctx, master)
	// A reliable final invalidation makes the last chunk visible even if
	// intermediate refresh notifications were coalesced while the UI was busy.
	m.emit(SessionOutputMsg{SessionID: session.ID}, true)
	if err != nil && !isExpectedPTYError(err) && session.ctx.Err() == nil {
		m.emit(SessionErrorMsg{SessionID: session.ID, Err: err}, true)
	}
}

func (m *SessionManager) waitSession(session *Session) {
	defer m.wg.Done()
	err := session.cmd.Wait()
	// The shell may have exited while descendants still own the slave PTY.
	terminateProcessGroup(session.cmd)
	if processGroupExists(session.cmd) {
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-timer.C:
		case <-m.ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		killProcessGroup(session.cmd)
	}
	close(session.done)
	if m.ctx.Err() == nil {
		m.emit(SessionExitedMsg{SessionID: session.ID, Err: err}, true)
	}
}

func (m *SessionManager) session(sessionID int) (*Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	session, ok := m.sessions[sessionID]
	if !ok {
		return nil, fmt.Errorf("session %d inconnue", sessionID)
	}
	return session, nil
}

func (m *SessionManager) SendInput(sessionID int, value string) error {
	session, err := m.session(sessionID)
	if err != nil {
		return err
	}
	// Rearm first. Together with the model's optimistic state transition, this
	// ensures an immediately-following second prompt cannot be lost.
	session.interceptor.Acknowledge()
	if err := session.Write(value + "\r"); err != nil {
		session.interceptor.Reblock()
		return err
	}
	return nil
}

func (m *SessionManager) Output(sessionID int) (string, error) {
	session, err := m.session(sessionID)
	if err != nil {
		return "", err
	}
	return session.interceptor.Output(), nil
}

func (m *SessionManager) Resize(sessionID, columns, rows int) error {
	session, err := m.session(sessionID)
	if err != nil {
		return err
	}
	return session.Resize(columns, rows)
}

// BeginShutdown unblocks channel senders immediately. Close performs the
// descriptor/process cleanup and waits for all owned goroutines.
func (m *SessionManager) BeginShutdown() {
	m.cancel()
}

func (m *SessionManager) Close() {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		sessions := make([]*Session, 0, len(m.sessions))
		for _, session := range m.sessions {
			sessions = append(sessions, session)
		}
		m.mu.Unlock()

		m.cancel()
		for _, session := range sessions {
			session.requestStop()
		}
		for _, session := range sessions {
			session.waitForStop()
		}
		m.wg.Wait()
	})
}

func isExpectedPTYError(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, os.ErrClosed) ||
		isPlatformPTYCloseError(err)
}

// creack/pty's functional implementations are Unix-only. StartWithSize puts
// the command in a new session, whose process-group ID is the leader's PID.
func terminateProcessGroup(command *exec.Cmd) {
	if command != nil && command.Process != nil {
		_ = signalProcessGroup(command.Process.Pid, syscall.SIGTERM)
	}
}

func killProcessGroup(command *exec.Cmd) {
	if command == nil || command.Process == nil {
		return
	}
	_ = signalProcessGroup(command.Process.Pid, syscall.SIGKILL)
	_ = command.Process.Kill()
}

func processGroupExists(command *exec.Cmd) bool {
	if command == nil || command.Process == nil {
		return false
	}
	err := signalProcessGroup(command.Process.Pid, syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

func signalProcessGroup(pid int, signal os.Signal) error {
	// On Unix, a negative PID addresses the whole process group. Using the
	// portable os.Process API keeps the single-file build command valid on
	// every GOOS; creack/pty itself reports ErrUnsupported where PTYs are absent.
	group, err := os.FindProcess(-pid)
	if err != nil {
		return err
	}
	return group.Signal(signal)
}

func isPlatformPTYCloseError(err error) bool {
	return errors.Is(err, syscall.EIO)
}

type agentPane struct {
	sessionID int
	name      string
	command   string
	viewport  viewport.Model
	blocked   bool
	prompt    PromptDetectedMsg
	exited    bool
	exitErr   error
}

type model struct {
	manager *SessionManager
	events  <-chan tea.Msg

	panes      [2]agentPane
	supervisor viewport.Model
	input      textinput.Model
	logs       []string

	width            int
	height           int
	leftWidth        int
	rightWidth       int
	topHeight        int
	supervisorHeight int
	activePanel      int
	pending          []int
	inputTarget      int
	writePending     bool
}

func newModel(manager *SessionManager, events <-chan tea.Msg, sessions [2]*Session) model {
	input := textinput.New()
	input.Prompt = "› "
	input.Placeholder = "En attente d'une validation interactive…"
	input.CharLimit = 4096
	input.Blur()
	setInputInterceptionStyle(&input, false)

	result := model{
		manager:     manager,
		events:      events,
		supervisor:  viewport.New(1, 1),
		input:       input,
		activePanel: 0,
		inputTarget: -1,
	}
	for index, session := range sessions {
		result.panes[index] = agentPane{
			sessionID: session.ID,
			name:      session.Name,
			command:   session.Command,
			viewport:  viewport.New(1, 1),
		}
	}
	result.appendLog("Relayer démarré avec deux sessions PTY")
	return result
}

func (m model) Init() tea.Cmd {
	return waitForManagerEvent(m.manager.Context(), m.events)
}

func waitForManagerEvent(ctx context.Context, events <-chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		select {
		case message := <-events:
			return managerEventMsg{Message: message}
		case <-ctx.Done():
			return managerEventMsg{Message: managerStoppedMsg{}}
		}
	}
}

func (m model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	commands := make([]tea.Cmd, 0, 3)
	if event, ok := message.(managerEventMsg); ok {
		message = event.Message
		commands = append(commands, waitForManagerEvent(m.manager.Context(), m.events))
	}

	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.resize(msg.Width, msg.Height)
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			m.manager.BeginShutdown()
			return m, tea.Quit
		}
		if msg.String() == "ctrl+left" {
			m.activePanel = (m.activePanel + 2) % 3
			commands = append(commands, m.syncFocus())
			break
		}
		if msg.String() == "ctrl+right" {
			m.activePanel = (m.activePanel + 1) % 3
			commands = append(commands, m.syncFocus())
			break
		}
		if m.activePanel == 2 && isViewportNavigationKey(msg) {
			// Vertical navigation is unambiguous for the supervisor: textinput
			// only needs horizontal cursor movement, while these keys browse logs.
			var command tea.Cmd
			m.supervisor, command = m.supervisor.Update(msg)
			commands = append(commands, command)
			break
		}

		if m.activePanel == 2 && m.inputTarget >= 0 {
			if msg.Type == tea.KeyEnter && !m.writePending {
				paneIndex := m.inputTarget
				prompt := m.panes[paneIndex].prompt
				value := m.input.Value()

				// Clear the old waiting state before the asynchronous PTY write.
				// This allows an immediate second prompt from this session to be
				// accepted instead of mistaken for a duplicate of the first.
				m.panes[paneIndex].blocked = false
				m.panes[paneIndex].prompt = PromptDetectedMsg{}
				m.removePending(paneIndex)
				m.inputTarget = -1
				m.input.Reset()
				m.input.Blur()
				setInputInterceptionStyle(&m.input, false)
				m.writePending = true
				m.appendLog(fmt.Sprintf("Réponse transmise à %s", m.panes[paneIndex].name))
				commands = append(commands, deliverInput(
					m.manager,
					m.panes[paneIndex].sessionID,
					value,
					prompt,
				))
				break
			}
			var command tea.Cmd
			m.input, command = m.input.Update(msg)
			commands = append(commands, command)
		} else if m.activePanel >= 0 && m.activePanel < len(m.panes) {
			var command tea.Cmd
			m.panes[m.activePanel].viewport, command = m.panes[m.activePanel].viewport.Update(msg)
			commands = append(commands, command)
		}
	case tea.MouseMsg:
		commands = append(commands, m.handleViewportMouse(msg))
	case SessionOutputMsg:
		if paneIndex := m.paneIndex(msg.SessionID); paneIndex >= 0 {
			m.refreshPaneOutput(paneIndex, msg.Content)
		}
	case PromptDetectedMsg:
		if paneIndex := m.paneIndex(msg.SessionID); paneIndex >= 0 {
			m.refreshPaneOutput(paneIndex, "")
			if m.panes[paneIndex].exited {
				break
			}
			if !m.panes[paneIndex].blocked {
				m.panes[paneIndex].blocked = true
				m.panes[paneIndex].prompt = msg
				m.pending = append(m.pending, paneIndex)
				m.appendLog(fmt.Sprintf(
					"%s attend une intervention humaine (%s)",
					m.panes[paneIndex].name,
					msg.Description,
				))
			}
			if m.inputTarget < 0 && !m.writePending {
				commands = append(commands, m.activateNextPrompt())
			}
		}
	case SessionExitedMsg:
		if paneIndex := m.paneIndex(msg.SessionID); paneIndex >= 0 {
			m.refreshPaneOutput(paneIndex, "")
			m.panes[paneIndex].exited = true
			m.panes[paneIndex].exitErr = msg.Err
			if msg.Err == nil {
				m.appendLog(fmt.Sprintf("%s terminé", m.panes[paneIndex].name))
			} else {
				m.appendLog(fmt.Sprintf("%s terminé avec erreur: %v", m.panes[paneIndex].name, msg.Err))
			}
			wasInputTarget := m.inputTarget == paneIndex
			m.removePending(paneIndex)
			m.panes[paneIndex].blocked = false
			m.panes[paneIndex].prompt = PromptDetectedMsg{}
			if wasInputTarget {
				m.inputTarget = -1
				// In particular, never carry a password into the next agent.
				m.input.Reset()
				commands = append(commands, m.activateNextPrompt())
			}
		}
	case SessionErrorMsg:
		if paneIndex := m.paneIndex(msg.SessionID); paneIndex >= 0 {
			m.appendLog(fmt.Sprintf("Erreur PTY de %s: %v", m.panes[paneIndex].name, msg.Err))
		}
	case inputDeliveredMsg:
		m.writePending = false
		paneIndex := m.paneIndex(msg.SessionID)
		if paneIndex < 0 {
			break
		}
		if msg.Err != nil {
			m.appendLog(fmt.Sprintf("Échec de l'envoi à %s: %v", m.panes[paneIndex].name, msg.Err))
			if !m.panes[paneIndex].exited && !m.panes[paneIndex].blocked {
				m.panes[paneIndex].blocked = true
				m.panes[paneIndex].prompt = msg.Prompt
				m.prependPending(paneIndex)
			}
		}
		commands = append(commands, m.activateNextPrompt())
	case managerStoppedMsg:
		return m, tea.Quit
	}

	return m, batchCommands(commands...)
}

func isViewportNavigationKey(message tea.KeyMsg) bool {
	switch message.Type {
	case tea.KeyUp, tea.KeyDown, tea.KeyPgUp, tea.KeyPgDown:
		return true
	default:
		return false
	}
}

// handleViewportMouse sends vertical wheel events to the viewport below the
// cursor, independently of keyboard focus. Coordinates in tea.MouseMsg are
// zero-based and refer to the whole terminal, whose top/bottom split is kept
// in the model by calculateLayout.
func (m *model) handleViewportMouse(message tea.MouseMsg) tea.Cmd {
	event := tea.MouseEvent(message)
	if event.Action != tea.MouseActionPress ||
		(event.Button != tea.MouseButtonWheelUp && event.Button != tea.MouseButtonWheelDown) {
		return nil
	}
	if m.width < minTerminalWidth || m.height < minTerminalHeight {
		// View() displays only the size warning, so no viewport is actually under
		// the pointer in this state.
		return nil
	}
	if event.X < 0 || event.X >= m.width || event.Y < 0 || event.Y >= m.height {
		return nil
	}

	if event.Y < m.topHeight {
		paneIndex := 0
		if event.X >= m.leftWidth {
			paneIndex = 1
		}
		var command tea.Cmd
		m.panes[paneIndex].viewport, command = m.panes[paneIndex].viewport.Update(message)
		return command
	}

	var command tea.Cmd
	m.supervisor, command = m.supervisor.Update(message)
	return command
}

func deliverInput(
	manager *SessionManager,
	sessionID int,
	value string,
	prompt PromptDetectedMsg,
) tea.Cmd {
	return func() tea.Msg {
		return inputDeliveredMsg{
			SessionID: sessionID,
			Prompt:    prompt,
			Err:       manager.SendInput(sessionID, value),
		}
	}
}

func (m *model) paneIndex(sessionID int) int {
	for index := range m.panes {
		if m.panes[index].sessionID == sessionID {
			return index
		}
	}
	return -1
}

func (m *model) activateNextPrompt() tea.Cmd {
	if len(m.pending) == 0 {
		m.inputTarget = -1
		m.input.Blur()
		m.input.EchoMode = textinput.EchoNormal
		m.input.Placeholder = "En attente d'une validation interactive…"
		setInputInterceptionStyle(&m.input, false)
		if m.activePanel == 2 {
			m.activePanel = 0
		}
		return nil
	}

	m.inputTarget = m.pending[0]
	m.activePanel = 2
	target := &m.panes[m.inputTarget]
	if target.prompt.Sensitive {
		m.input.EchoMode = textinput.EchoPassword
		m.input.EchoCharacter = '•'
	} else {
		m.input.EchoMode = textinput.EchoNormal
	}
	m.input.Placeholder = fmt.Sprintf("Réponse pour %s (Entrée pour envoyer)", target.name)
	setInputInterceptionStyle(&m.input, true)
	return m.input.Focus()
}

func setInputInterceptionStyle(input *textinput.Model, active bool) {
	if active {
		input.PromptStyle = inputActivePromptStyle
		input.TextStyle = inputActiveTextStyle
		input.PlaceholderStyle = inputActivePlaceholderStyle
		input.Cursor.Style = inputActiveCursorStyle
		input.Cursor.TextStyle = inputActiveTextStyle
		return
	}
	input.PromptStyle = inputInactivePromptStyle
	input.TextStyle = inputInactiveTextStyle
	input.PlaceholderStyle = inputInactivePlaceholderStyle
	input.Cursor.Style = inputInactivePromptStyle
	input.Cursor.TextStyle = inputInactiveTextStyle
}

func (m *model) syncFocus() tea.Cmd {
	if m.activePanel == 2 && m.inputTarget >= 0 {
		return m.input.Focus()
	}
	m.input.Blur()
	return nil
}

func (m *model) removePending(paneIndex int) {
	filtered := m.pending[:0]
	for _, pendingIndex := range m.pending {
		if pendingIndex != paneIndex {
			filtered = append(filtered, pendingIndex)
		}
	}
	m.pending = filtered
}

func (m *model) prependPending(paneIndex int) {
	for _, pendingIndex := range m.pending {
		if pendingIndex == paneIndex {
			return
		}
	}
	m.pending = append([]int{paneIndex}, m.pending...)
}

func (m *model) refreshPaneOutput(paneIndex int, content string) {
	if content == "" {
		var err error
		content, err = m.manager.Output(m.panes[paneIndex].sessionID)
		if err != nil {
			return
		}
	}
	setViewportContent(&m.panes[paneIndex].viewport, content)
}

// setViewportContent follows new output only while the user is already at the
// bottom. Once they scroll up, Bubble Tea keeps the same Y offset as new PTY
// chunks arrive. If the bounded RingBuffer eventually evicts that position,
// SetContent safely clamps it back into the retained history.
func setViewportContent(target *viewport.Model, content string) {
	wasAtBottom := target.AtBottom()
	previousOffset := target.YOffset
	target.SetContent(content)
	if wasAtBottom {
		target.GotoBottom()
		return
	}
	target.SetYOffset(previousOffset)
}

func (m *model) appendLog(message string) {
	line := fmt.Sprintf("%s  %s", time.Now().Format("15:04:05"), message)
	m.logs = append(m.logs, line)
	if len(m.logs) > maxSystemLogLines {
		m.logs = append([]string(nil), m.logs[len(m.logs)-maxSystemLogLines:]...)
	}
	setViewportContent(&m.supervisor, strings.Join(m.logs, "\n"))
}

type layoutGeometry struct {
	width                    int
	height                   int
	leftWidth                int
	rightWidth               int
	topHeight                int
	supervisorHeight         int
	agentViewportWidths      [2]int
	agentViewportHeight      int
	supervisorViewportWidth  int
	supervisorViewportHeight int
	inputWidth               int
}

// calculateLayout is the single source of truth for both the Bubble Tea
// viewports and the PTY window sizes. Lip Gloss Width/Height describe the
// content box, so borders and the agent title line are removed explicitly.
func calculateLayout(width, height int) layoutGeometry {
	result := layoutGeometry{
		width:  maxInt(1, width),
		height: maxInt(1, height),
	}
	result.leftWidth = maxInt(1, result.width/2)
	result.rightWidth = maxInt(1, result.width-result.leftWidth)

	if result.height >= minTerminalHeight {
		// Reserve one quarter for supervision and leave roughly 75% of the
		// terminal to the two live agent streams.
		result.supervisorHeight = maxInt(6, result.height/4)
		result.topHeight = result.height - result.supervisorHeight
		if result.topHeight < 4 {
			result.topHeight = 4
			result.supervisorHeight = result.height - result.topHeight
		}
	} else {
		result.topHeight = maxInt(1, result.height/2)
		result.supervisorHeight = maxInt(1, result.height-result.topHeight)
	}

	agentFrame := agentABorderStyle
	result.agentViewportWidths[0] = maxInt(
		1,
		result.leftWidth-agentFrame.GetHorizontalFrameSize(),
	)
	result.agentViewportWidths[1] = maxInt(
		1,
		result.rightWidth-agentFrame.GetHorizontalFrameSize(),
	)
	agentInnerHeight := maxInt(1, result.topHeight-agentFrame.GetVerticalFrameSize())
	result.agentViewportHeight = maxInt(1, agentInnerHeight-1)

	supervisorFrame := supervisorBorderStyle
	result.supervisorViewportWidth = maxInt(
		1,
		result.width-supervisorFrame.GetHorizontalFrameSize(),
	)
	supervisorInnerHeight := maxInt(
		1,
		result.supervisorHeight-supervisorFrame.GetVerticalFrameSize(),
	)
	result.supervisorViewportHeight = maxInt(1, supervisorInnerHeight-3)
	result.inputWidth = maxInt(1, result.supervisorViewportWidth-2)
	return result
}

func resizeViewport(target *viewport.Model, width, height int) {
	wasAtBottom := target.AtBottom()
	previousOffset := target.YOffset
	target.Width = maxInt(1, width)
	target.Height = maxInt(1, height)
	if wasAtBottom {
		target.GotoBottom()
		return
	}
	// Keep a manually selected history position through terminal resizes while
	// still clamping it if the viewport grew beyond the remaining content.
	target.SetYOffset(previousOffset)
}

func (m *model) resize(width, height int) {
	layout := calculateLayout(width, height)
	m.width = layout.width
	m.height = layout.height
	m.leftWidth = layout.leftWidth
	m.rightWidth = layout.rightWidth
	m.topHeight = layout.topHeight
	m.supervisorHeight = layout.supervisorHeight

	for index := range m.panes {
		resizeViewport(
			&m.panes[index].viewport,
			layout.agentViewportWidths[index],
			layout.agentViewportHeight,
		)
	}
	resizeViewport(
		&m.supervisor,
		layout.supervisorViewportWidth,
		layout.supervisorViewportHeight,
	)
	m.input.Width = layout.inputWidth

	for index := range m.panes {
		if err := m.manager.Resize(
			m.panes[index].sessionID,
			m.panes[index].viewport.Width,
			m.panes[index].viewport.Height,
		); err != nil && !errors.Is(err, errSessionClosed) {
			m.appendLog(fmt.Sprintf("Redimensionnement de %s impossible: %v", m.panes[index].name, err))
		}
	}
}

var (
	colorAgentA    = lipgloss.Color("#00D7FF")
	colorAgentB    = lipgloss.Color("#FF5AF7")
	colorMuted     = lipgloss.Color("#4B5563")
	colorText      = lipgloss.Color("#E5E7EB")
	colorBlocked   = lipgloss.Color("#FF0000")
	colorSuccess   = lipgloss.Color("#00D75F")
	colorInputHint = lipgloss.Color("#9CA3AF")

	// Normal state: each agent keeps its own visual identity while the
	// supervisor stays deliberately understated.
	agentABorderStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(colorAgentA)
	agentBBorderStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(colorAgentB)
	supervisorBorderStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(colorMuted)

	// Interception state: a double, bright-red border is more prominent than
	// the normal rounded frame without changing its one-cell frame size.
	interceptionBorderStyle = lipgloss.NewStyle().
				Border(lipgloss.DoubleBorder()).
				BorderForeground(colorBlocked)

	inputInactivePromptStyle      = lipgloss.NewStyle().Foreground(colorMuted)
	inputInactiveTextStyle        = lipgloss.NewStyle().Foreground(colorInputHint)
	inputInactivePlaceholderStyle = lipgloss.NewStyle().Foreground(colorMuted).Italic(true)
	inputActivePromptStyle        = lipgloss.NewStyle().Foreground(colorBlocked).Bold(true)
	inputActiveTextStyle          = lipgloss.NewStyle().Foreground(colorText)
	inputActivePlaceholderStyle   = lipgloss.NewStyle().Foreground(colorInputHint)
	inputActiveCursorStyle        = lipgloss.NewStyle().Foreground(colorBlocked).Reverse(true)
)

func (m model) View() string {
	if m.width == 0 || m.height == 0 {
		return "Initialisation de Relayer…"
	}
	if m.width < minTerminalWidth || m.height < minTerminalHeight {
		return lipgloss.NewStyle().
			Foreground(colorBlocked).
			Bold(true).
			Width(m.width).
			Height(m.height).
			MaxWidth(m.width).
			MaxHeight(m.height).
			Align(lipgloss.Center, lipgloss.Center).
			Render(fmt.Sprintf(
				"Terminal trop petit (%dx%d). Minimum conseillé: %dx%d.",
				m.width,
				m.height,
				minTerminalWidth,
				minTerminalHeight,
			))
	}

	left := m.renderAgentPane(0, m.leftWidth, m.topHeight)
	right := m.renderAgentPane(1, m.rightWidth, m.topHeight)
	top := lipgloss.JoinHorizontal(lipgloss.Top, left, right)
	supervisor := m.renderSupervisorPane(m.width, m.supervisorHeight)
	return lipgloss.JoinVertical(lipgloss.Left, top, supervisor)
}

func (m model) renderAgentPane(index, outerWidth, outerHeight int) string {
	pane := m.panes[index]
	style := agentPanelStyle(index, pane.blocked)
	innerWidth := maxInt(1, outerWidth-style.GetHorizontalFrameSize())
	innerHeight := maxInt(1, outerHeight-style.GetVerticalFrameSize())

	status := "EN COURS"
	statusColor := agentColor(index)
	if pane.blocked {
		status = "INTERVENTION REQUISE"
		statusColor = colorBlocked
	} else if pane.exited && pane.exitErr == nil {
		status = "TERMINÉ"
		statusColor = colorSuccess
	} else if pane.exited {
		status = "ERREUR"
		statusColor = colorBlocked
	}

	focusMarker := "  "
	if m.activePanel == index {
		focusMarker = "▶ "
	}
	title := lipgloss.NewStyle().Foreground(agentColor(index)).Bold(true).Render(focusMarker+pane.name) + "  " +
		lipgloss.NewStyle().Foreground(statusColor).Render("● "+status)
	title = lipgloss.NewStyle().MaxWidth(innerWidth).Render(title)
	content := title + "\n" + pane.viewport.View()
	return style.Width(innerWidth).Height(innerHeight).Render(content)
}

func (m model) renderSupervisorPane(outerWidth, outerHeight int) string {
	intercepting := m.hasBlockedPane()
	style := supervisorPanelStyle(intercepting)
	innerWidth := maxInt(1, outerWidth-style.GetHorizontalFrameSize())
	innerHeight := maxInt(1, outerHeight-style.GetVerticalFrameSize())

	title := "SUPERVISEUR  •  AUTOMATIQUE"
	if intercepting {
		title = "SUPERVISEUR  •  ACTION HUMAINE REQUISE"
	}
	if m.inputTarget >= 0 {
		title += "  →  " + m.panes[m.inputTarget].name
	}
	help := lipgloss.NewStyle().Foreground(colorMuted).MaxWidth(innerWidth).Render(
		"Ctrl+←/→: panneau • ↑/↓, PgUp/PgDn, molette: historique • Entrée: envoyer • Ctrl+C: quitter",
	)
	titleColor := colorMuted
	if intercepting {
		titleColor = colorBlocked
	}
	title = lipgloss.NewStyle().Foreground(titleColor).Bold(true).MaxWidth(innerWidth).Render(title)
	content := title + "\n" +
		m.supervisor.View() + "\n" +
		m.input.View() + "\n" + help
	return style.Width(innerWidth).Height(innerHeight).Render(content)
}

func agentPanelStyle(index int, blocked bool) lipgloss.Style {
	if blocked {
		return interceptionBorderStyle
	}
	if index == 0 {
		return agentABorderStyle
	}
	return agentBBorderStyle
}

func supervisorPanelStyle(blocked bool) lipgloss.Style {
	if blocked {
		return interceptionBorderStyle
	}
	return supervisorBorderStyle
}

func agentColor(index int) lipgloss.Color {
	if index == 0 {
		return colorAgentA
	}
	return colorAgentB
}

func (m model) hasBlockedPane() bool {
	for _, pane := range m.panes {
		if pane.blocked {
			return true
		}
	}
	return false
}

func batchCommands(commands ...tea.Cmd) tea.Cmd {
	filtered := commands[:0]
	for _, command := range commands {
		if command != nil {
			filtered = append(filtered, command)
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	if len(filtered) == 1 {
		return filtered[0]
	}
	return tea.Batch(filtered...)
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func clampInt(value, minimum, maximum int) int {
	return minInt(maxInt(value, minimum), maximum)
}

const defaultPane1Command = `printf 'Agent A: Running...\n'; sleep 2; printf 'Overwrite file? [Y/n] '; read ans; printf 'Agent A done: %s\n' "$ans"`

const defaultPane2Command = `printf 'Agent B: Running...\n'; sleep 4; printf 'Password: '; stty -echo; read ans; stty echo; printf '\nAgent B received %d characters\n' "${#ans}"`

// initialTerminalLayout avoids starting fast-producing CLIs with the old
// arbitrary 80x16 PTY size. Bubble Tea still remains authoritative: every
// subsequent WindowSizeMsg recalculates this layout and calls pty.Setsize.
func initialTerminalLayout() layoutGeometry {
	const fallbackWidth = 80
	const fallbackHeight = 24

	for _, terminal := range []*os.File{os.Stdout, os.Stdin, os.Stderr} {
		rows, columns, err := pty.Getsize(terminal)
		if err == nil && rows > 0 && columns > 0 {
			return calculateLayout(columns, rows)
		}
	}
	return calculateLayout(fallbackWidth, fallbackHeight)
}

func run() error {
	pane1Command := flag.String("pane1", defaultPane1Command, "commande shell du premier agent")
	pane2Command := flag.String("pane2", defaultPane2Command, "commande shell du second agent")
	configPath := flag.String("config", defaultConfigPath, "fichier YAML des patterns d'interception")
	flag.Parse()
	patterns, configCreated, err := loadPromptPatterns(*configPath)
	if err != nil {
		return err
	}
	initialLayout := initialTerminalLayout()

	events := make(chan tea.Msg, 256)
	manager, err := NewSessionManager(
		context.Background(),
		events,
		patterns,
		defaultRingCapacity,
	)
	if err != nil {
		return err
	}
	defer manager.Close()

	first, err := manager.Start(
		"Agent A (Claude)",
		*pane1Command,
		initialLayout.agentViewportWidths[0],
		initialLayout.agentViewportHeight,
	)
	if err != nil {
		return err
	}
	second, err := manager.Start(
		"Agent B (Local)",
		*pane2Command,
		initialLayout.agentViewportWidths[1],
		initialLayout.agentViewportHeight,
	)
	if err != nil {
		return err
	}

	application := newModel(manager, events, [2]*Session{first, second})
	application.resize(initialLayout.width, initialLayout.height)
	if configCreated {
		application.appendLog(fmt.Sprintf("Configuration par défaut créée: %s", *configPath))
	}
	application.appendLog(fmt.Sprintf("%d patterns chargés depuis %s", len(patterns), *configPath))
	program := tea.NewProgram(
		application,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)
	_, err = program.Run()
	return err
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "relayer: %v\n", err)
		os.Exit(1)
	}
}
