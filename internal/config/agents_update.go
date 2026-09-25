package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/platform"
	"gopkg.in/yaml.v3"
)

var (
	ErrRevisionMismatch = errors.New("configuration modified since it was loaded")
	// ErrCommitUncertain means the atomic rename completed but a subsequent
	// durability or verification step failed. Callers must reload before any
	// retry so they never overwrite a publication that may already be visible.
	ErrCommitUncertain = errors.New("configuration publication completed with uncertain durability state")
)

var configurationPathLocks sync.Map
var syncConfigurationDirectory = syncDirectory

// FileSnapshot is an opaque, in-memory copy of one regular configuration
// file. Its bytes may contain credentials and are therefore deliberately not
// exported. Desktop lifecycle code uses it only to restore an exact previous
// document after a failed restart.
type FileSnapshot struct {
	path     string
	data     []byte
	mode     os.FileMode
	revision string
}

// CaptureFileSnapshot reads a configuration under the same cooperative lock
// used by ReplaceAgents. The returned snapshot owns an independent byte copy.
func CaptureFileSnapshot(path string) (*FileSnapshot, error) {
	absolutePath, unlock, err := acquireConfigurationUpdateLock(path)
	if err != nil {
		return nil, err
	}
	defer unlock()
	data, info, err := readRegularConfiguration(absolutePath)
	if err != nil {
		return nil, err
	}
	return &FileSnapshot{
		path:     absolutePath,
		data:     append([]byte(nil), data...),
		mode:     info.Mode().Perm(),
		revision: contentRevision(data),
	}, nil
}

// Revision returns the content revision captured with the snapshot without
// exposing any file bytes.
func (snapshot *FileSnapshot) Revision() string {
	if snapshot == nil {
		return ""
	}
	return snapshot.revision
}

// Restore atomically republishes the exact captured bytes only while the
// current file still has expectedRevision. This prevents rollback from
// overwriting a newer edit made by another process.
func (snapshot *FileSnapshot) Restore(expectedRevision string) (Result, string, error) {
	if snapshot == nil || len(snapshot.data) == 0 || strings.TrimSpace(snapshot.path) == "" {
		return Result{}, "", errors.New("configuration snapshot unavailable")
	}
	absolutePath, unlock, err := acquireConfigurationUpdateLock(snapshot.path)
	if err != nil {
		return Result{}, "", err
	}
	defer unlock()
	current, _, err := readRegularConfiguration(absolutePath)
	if err != nil {
		return Result{}, "", err
	}
	if contentRevision(current) != expectedRevision {
		return Result{}, "", ErrRevisionMismatch
	}
	if err := publishConfigurationBytes(absolutePath, snapshot.data, snapshot.mode); err != nil {
		return Result{}, "", err
	}
	restored, err := LoadExisting(absolutePath)
	if err != nil {
		return Result{}, snapshot.revision, errors.Join(ErrCommitUncertain, err)
	}
	return restored, restored.Revision, nil
}

// Discard overwrites the retained bytes and makes the snapshot unusable.
func (snapshot *FileSnapshot) Discard() {
	if snapshot == nil {
		return
	}
	for index := range snapshot.data {
		snapshot.data[index] = 0
	}
	snapshot.data = nil
	snapshot.path = ""
	snapshot.revision = ""
}

// FileRevision returns a content-derived revision suitable for optimistic
// updates. It never includes file contents in errors or diagnostics.
func FileRevision(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("configuration file path is empty")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read configuration: %w", err)
	}
	return contentRevision(data), nil
}

// ReplaceAgents atomically replaces only the version-one agents sequence. It
// preserves every other YAML node, refuses legacy documents, and verifies the
// caller's revision immediately before publication.
func ReplaceAgents(path, expectedRevision string, specs []agent.Spec) (Result, string, error) {
	if strings.TrimSpace(path) == "" {
		return Result{}, "", errors.New("configuration file path is empty")
	}
	if strings.TrimSpace(expectedRevision) == "" {
		return Result{}, "", errors.New("expected revision is empty")
	}
	if len(specs) > maxAgents {
		return Result{}, "", fmt.Errorf("too many agents: maximum %d", maxAgents)
	}

	absolutePath, unlock, err := acquireConfigurationUpdateLock(path)
	if err != nil {
		return Result{}, "", err
	}
	defer unlock()

	current, err := LoadExisting(absolutePath)
	if err != nil {
		return Result{}, "", err
	}
	if current.Legacy {
		return Result{}, "", errors.New("legacy configuration must be migrated to version: 1 before modifying agents")
	}
	data, info, err := readRegularConfiguration(absolutePath)
	if err != nil {
		return Result{}, "", err
	}
	if contentRevision(data) != expectedRevision {
		return Result{}, "", ErrRevisionMismatch
	}

	baseDir, err := filepath.Abs(filepath.Dir(absolutePath))
	if err != nil {
		return Result{}, "", errors.New("could not resolve configuration directory")
	}
	validated, err := agent.ValidateAll(specs, baseDir, current.Backend)
	if err != nil {
		return Result{}, "", fmt.Errorf("invalid agent profiles: %w", err)
	}
	if err := validateUpdatedPolicyAgents(current, validated); err != nil {
		return Result{}, "", err
	}
	if sameAgents(validated, current.Agents) {
		// Nothing to write. Publishing the same agents again only reformatted
		// the file and gave it a new revision, which every open editor then
		// had to reload.
		return current, current.Revision, nil
	}
	rendered, err := replaceAgentsYAML(data, validated, specs, current.Agents, baseDir)
	if err != nil {
		return Result{}, "", err
	}
	publishedRevision := contentRevision(rendered)

	directory := filepath.Dir(absolutePath)
	temporary, err := os.CreateTemp(directory, ".relayer-agents-*.tmp")
	if err != nil {
		return Result{}, "", errors.New("could not create temporary configuration file")
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	mode := info.Mode().Perm()
	if mode == 0 {
		mode = 0o600
	}
	if _, err := temporary.Write(rendered); err != nil {
		_ = temporary.Close()
		return Result{}, "", errors.New("could not write configuration")
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return Result{}, "", errors.New("could not sync configuration")
	}
	// CreateTemp starts at 0600. Keep that restrictive mode while bytes are
	// written, then reproduce the existing file mode only after content is
	// complete so a permissive config never exposes a partial temporary file.
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return Result{}, "", errors.New("could not apply configuration permissions")
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return Result{}, "", errors.New("could not sync configuration permissions")
	}
	if err := temporary.Close(); err != nil {
		return Result{}, "", errors.New("could not close temporary configuration")
	}
	candidate, err := LoadExisting(temporaryPath)
	if err != nil {
		return Result{}, "", fmt.Errorf("invalid updated configuration: %w", err)
	}
	if !sameAgents(candidate.Agents, validated) {
		return Result{}, "", errors.New("the written agents would differ from the requested ones")
	}

	latest, _, err := readRegularConfiguration(absolutePath)
	if err != nil {
		return Result{}, "", err
	}
	if contentRevision(latest) != expectedRevision {
		return Result{}, "", ErrRevisionMismatch
	}
	if renameErr := platform.PublishByRename(temporaryPath, absolutePath); renameErr != nil {
		return Result{}, "", errors.New("could not atomically publish configuration")
	}
	if err := syncConfigurationDirectory(directory); err != nil {
		updated, loadErr := LoadExisting(absolutePath)
		if loadErr != nil {
			return Result{}, contentRevision(rendered), errors.Join(
				ErrCommitUncertain,
				errors.New("could not sync configuration directory"),
			)
		}
		return updated, updated.Revision, errors.Join(
			ErrCommitUncertain,
			errors.New("could not sync configuration directory"),
		)
	}

	updated, err := LoadExisting(absolutePath)
	if err != nil {
		return Result{}, publishedRevision, errors.Join(ErrCommitUncertain, err)
	}
	if updated.Revision != publishedRevision {
		// A non-cooperating editor published after our rename. Its bytes are
		// authoritative, but our requested transaction did not win and therefore
		// must never be used as rollback authority by a lifecycle controller.
		return updated, updated.Revision, ErrRevisionMismatch
	}
	return updated, updated.Revision, nil
}

func acquireConfigurationUpdateLock(path string) (string, func(), error) {
	if strings.TrimSpace(path) == "" {
		return "", nil, errors.New("configuration file path is empty")
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return "", nil, errors.New("could not resolve configuration path")
	}
	pathLock, _ := configurationPathLocks.LoadOrStore(filepath.Clean(absolutePath), &sync.Mutex{})
	pathMutex := pathLock.(*sync.Mutex)
	pathMutex.Lock()
	unlockFile, err := lockConfigurationFile(absolutePath)
	if err != nil {
		pathMutex.Unlock()
		return "", nil, err
	}
	return absolutePath, func() {
		unlockFile()
		pathMutex.Unlock()
	}, nil
}

func publishConfigurationBytes(path string, data []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".relayer-rollback-*.tmp")
	if err != nil {
		return errors.New("could not create temporary restore file")
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if mode.Perm() == 0 {
		mode = 0o600
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return errors.New("could not write restore")
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return errors.New("could not sync restore")
	}
	if err := temporary.Chmod(mode.Perm()); err != nil {
		_ = temporary.Close()
		return errors.New("could not apply restore permissions")
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return errors.New("could not sync restore permissions")
	}
	if err := temporary.Close(); err != nil {
		return errors.New("could not close temporary restore")
	}
	if _, err := LoadExisting(temporaryPath); err != nil {
		return errors.New("invalid restore snapshot")
	}
	if err := platform.PublishByRename(temporaryPath, path); err != nil {
		return errors.New("could not atomically publish restore")
	}
	if err := syncConfigurationDirectory(directory); err != nil {
		return errors.Join(ErrCommitUncertain, errors.New("could not sync restored directory"))
	}
	return nil
}

func validateUpdatedPolicyAgents(current Result, specs []agent.Spec) error {
	available := make(map[string]struct{}, len(specs))
	for _, spec := range specs {
		available[strings.ToLower(strings.TrimSpace(spec.ID))] = struct{}{}
	}
	for _, rule := range current.Policies.Rules {
		for _, configuredID := range rule.Match.AgentIDs {
			if _, exists := available[strings.ToLower(strings.TrimSpace(configuredID))]; !exists {
				return fmt.Errorf("policy rule %q references a missing agent", rule.Name)
			}
		}
	}
	return nil
}

func readRegularConfiguration(path string) ([]byte, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, errors.New("could not inspect configuration")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, nil, errors.New("configuration must be a regular non-symlink file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, errors.New("could not read configuration")
	}
	return data, info, nil
}

func contentRevision(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func replaceAgentsYAML(data []byte, specs, requested, loaded []agent.Spec, baseDir string) ([]byte, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return nil, errors.New("could not decode configuration")
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("configuration contains multiple YAML documents")
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("versioned configuration must be a YAML mapping")
	}
	agentsNode := mappingValue(document.Content[0], "agents")
	if agentsNode == nil {
		return nil, errors.New("the agents field is absent")
	}
	replacement := agentSequenceNode(specs, requested, loaded, agentsNode, baseDir)
	replacement.HeadComment = agentsNode.HeadComment
	replacement.LineComment = agentsNode.LineComment
	replacement.FootComment = agentsNode.FootComment
	*agentsNode = *replacement

	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	if err := encoder.Encode(&document); err != nil {
		return nil, errors.New("could not encode configuration")
	}
	if err := encoder.Close(); err != nil {
		return nil, errors.New("could not finalize configuration")
	}
	return output.Bytes(), nil
}

// agentSequenceNode returns the agents sequence to write for specs, the
// validated agents of a save. previous is the file's agents sequence and loaded
// the agents the loader read from it, entry for entry; requested is the save's
// agents as given, whose working directories may be relative.
//
// Every entry used to be rebuilt from its spec, so a save that changed one
// agent rewrote them all: their comments went, flow sequences became block
// lists, a shell script's quoting changed, and an agent that inherited the
// file's backend was pinned to it with "backend: pty". Now an agent whose
// spec is the one the file already gives it keeps its entry exactly as
// written, and an agent that changed keeps every field that did not change,
// written where it was. Only a new agent is built from its spec.
func agentSequenceNode(specs, requested, loaded []agent.Spec, previous *yaml.Node, baseDir string) *yaml.Node {
	type writtenAgent struct {
		entry *yaml.Node
		spec  agent.Spec
	}
	previousByID := make(map[string]writtenAgent)
	var writtenDirectories []string
	if previous != nil && previous.Kind == yaml.SequenceNode {
		for index, entry := range previous.Content {
			if entry.Kind != yaml.MappingNode {
				continue
			}
			id := mappingValue(entry, "id")
			if id == nil || id.Kind != yaml.ScalarNode {
				continue
			}
			// The loader reads the sequence in order, so loaded[index] is
			// this entry's spec. An entry it cannot be paired with is
			// rebuilt, as every entry was before.
			if index < len(loaded) && strings.TrimSpace(loaded[index].ID) == strings.TrimSpace(id.Value) {
				previousByID[strings.ToLower(strings.TrimSpace(id.Value))] = writtenAgent{entry: entry, spec: loaded[index]}
			}
			if cwd := mappingValue(entry, "cwd"); cwd != nil && cwd.Kind == yaml.ScalarNode {
				writtenDirectories = append(writtenDirectories, cwd.Value)
			}
		}
	}
	sequence := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for index, spec := range specs {
		requestedCwd := ""
		if index < len(requested) {
			requestedCwd = requested[index].Cwd
		}
		written, found := previousByID[strings.ToLower(strings.TrimSpace(spec.ID))]
		switch {
		case found && sameAgent(written.spec, spec):
			sequence.Content = append(sequence.Content, written.entry)
		case found:
			sequence.Content = append(sequence.Content,
				changedAgentEntry(written.entry, written.spec, spec, requestedCwd, writtenDirectories, baseDir))
		default:
			sequence.Content = append(sequence.Content,
				newAgentEntry(spec, workingDirectoryText(spec.Cwd, requestedCwd, writtenDirectories, baseDir)))
		}
	}
	return sequence
}

// newAgentEntry builds the entry of an agent the file does not have.
func newAgentEntry(spec agent.Spec, cwd string) *yaml.Node {
	entry := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	appendStringField(entry, "id", spec.ID)
	appendStringField(entry, "name", spec.Name)
	if len(spec.Command) > 0 {
		appendNodeField(entry, "command", stringSequenceNode(spec.Command, 0))
	} else {
		appendStringField(entry, "shell", spec.Shell)
	}
	if cwd != "" {
		appendStringField(entry, "cwd", cwd)
	}
	if len(spec.Env) > 0 {
		appendNodeField(entry, "env", environmentNode(spec.Env))
	}
	if spec.Adapter != "" {
		appendStringField(entry, "adapter", spec.Adapter)
	}
	if spec.Backend != "" {
		appendStringField(entry, "backend", spec.Backend)
	}
	return entry
}

// changedAgentEntry is a copy of entry, whose agent the file gives as was,
// with each field that differs in spec written in place. A field that did not
// change keeps its node, its text and its comments. A backend the entry leaves
// to the file stays left to it unless the save changes the agent's backend.
func changedAgentEntry(entry *yaml.Node, was, spec agent.Spec, requestedCwd string, writtenDirectories []string, baseDir string) *yaml.Node {
	updated := *entry
	updated.Content = append([]*yaml.Node(nil), entry.Content...)
	if was.ID != spec.ID {
		setScalarField(&updated, "id", spec.ID)
	}
	if was.Name != spec.Name {
		setScalarField(&updated, "name", spec.Name)
	}
	if !sameStrings(was.Command, spec.Command) || was.Shell != spec.Shell {
		if len(spec.Command) > 0 {
			style := yaml.Style(0)
			if previous := mappingValue(&updated, "command"); previous != nil {
				style = previous.Style & yaml.FlowStyle
			}
			replaceField(&updated, "command", "shell", stringSequenceNode(spec.Command, style))
		} else {
			replaceField(&updated, "shell", "command", scalarLike(mappingValue(&updated, "shell"), spec.Shell))
		}
	}
	if was.Cwd != spec.Cwd {
		if text := workingDirectoryText(spec.Cwd, requestedCwd, writtenDirectories, baseDir); text == "" {
			removeField(&updated, "cwd")
		} else {
			setScalarField(&updated, "cwd", text)
		}
	}
	if !sameEnvironment(was.Env, spec.Env) {
		if len(spec.Env) == 0 {
			removeField(&updated, "env")
		} else {
			setNodeField(&updated, "env", changedEnvironmentNode(mappingValue(&updated, "env"), spec.Env))
		}
	}
	if was.Adapter != spec.Adapter {
		if spec.Adapter == "" {
			removeField(&updated, "adapter")
		} else {
			setScalarField(&updated, "adapter", spec.Adapter)
		}
	}
	if was.Backend != spec.Backend {
		setScalarField(&updated, "backend", spec.Backend)
	}
	return &updated
}

// workingDirectoryText is the text written for an agent's resolved working
// directory: the text the save gave when it names the same directory and is
// relative, else a relative text another entry of the file already uses for
// it, else the save's own text. The editors show a working directory
// resolved, so it comes back absolute, and a relative one was written as an
// absolute path naming the user's home directory.
func workingDirectoryText(resolved, requested string, writtenDirectories []string, baseDir string) string {
	if strings.TrimSpace(resolved) == "" {
		return ""
	}
	if strings.TrimSpace(requested) != "" && !filepath.IsAbs(requested) && equivalentWorkingDirectory(requested, resolved, baseDir) {
		return requested
	}
	for _, written := range writtenDirectories {
		if strings.TrimSpace(written) != "" && !filepath.IsAbs(written) && equivalentWorkingDirectory(written, resolved, baseDir) {
			return written
		}
	}
	if equivalentWorkingDirectory(requested, resolved, baseDir) {
		return requested
	}
	return resolved
}

// stringSequenceNode is a sequence of strings: a command, or a policy's
// blocked patterns.
func stringSequenceNode(values []string, style yaml.Style) *yaml.Node {
	node := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: style}
	for _, value := range values {
		node.Content = append(node.Content, stringNode(value))
	}
	return node
}

func environmentNode(environment map[string]string) *yaml.Node {
	node := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	names := make([]string, 0, len(environment))
	for name := range environment {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		appendStringField(node, name, environment[name])
	}
	return node
}

// changedEnvironmentNode edits the entry's env mapping in place, when it has
// one, so each variable that did not change keeps its line and comment.
func changedEnvironmentNode(previous *yaml.Node, environment map[string]string) *yaml.Node {
	if previous == nil || previous.Kind != yaml.MappingNode {
		return environmentNode(environment)
	}
	updated := *previous
	updated.Content = nil
	written := make(map[string]struct{}, len(environment))
	for index := 0; index+1 < len(previous.Content); index += 2 {
		name, value := previous.Content[index], previous.Content[index+1]
		wanted, kept := environment[name.Value]
		if !kept {
			continue
		}
		written[name.Value] = struct{}{}
		if value.Kind != yaml.ScalarNode || value.Value != wanted {
			value = scalarLike(value, wanted)
		}
		updated.Content = append(updated.Content, name, value)
	}
	added := make([]string, 0, len(environment))
	for name := range environment {
		if _, done := written[name]; !done {
			added = append(added, name)
		}
	}
	sort.Strings(added)
	for _, name := range added {
		appendStringField(&updated, name, environment[name])
	}
	return &updated
}

// scalarLike is a string scalar holding value, quoted as previous was and
// keeping its comments.
func scalarLike(previous *yaml.Node, value string) *yaml.Node {
	node := stringNode(value)
	if previous != nil {
		if previous.Kind == yaml.ScalarNode {
			node.Style = previous.Style & (yaml.SingleQuotedStyle | yaml.DoubleQuotedStyle | yaml.LiteralStyle | yaml.FoldedStyle)
		}
		node.HeadComment = previous.HeadComment
		node.LineComment = previous.LineComment
		node.FootComment = previous.FootComment
	}
	return node
}

func fieldIndex(mapping *yaml.Node, key string) int {
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			return index
		}
	}
	return -1
}

func setScalarField(mapping *yaml.Node, key, value string) {
	setNodeField(mapping, key, scalarLike(mappingValue(mapping, key), value))
}

func setNodeField(mapping *yaml.Node, key string, value *yaml.Node) {
	if index := fieldIndex(mapping, key); index >= 0 {
		mapping.Content[index+1] = value
		return
	}
	appendNodeField(mapping, key, value)
}

// replaceField writes value under key, in the place of key or else of
// alternative, which it removes: a shell agent given a command, or the
// reverse, keeps its place in the entry.
func replaceField(mapping *yaml.Node, key, alternative string, value *yaml.Node) {
	if index := fieldIndex(mapping, key); index >= 0 {
		mapping.Content[index+1] = value
		removeField(mapping, alternative)
		return
	}
	if index := fieldIndex(mapping, alternative); index >= 0 {
		keyNode := *mapping.Content[index]
		keyNode.Value = key
		mapping.Content[index] = &keyNode
		mapping.Content[index+1] = value
		return
	}
	appendNodeField(mapping, key, value)
}

func removeField(mapping *yaml.Node, key string) {
	if index := fieldIndex(mapping, key); index >= 0 {
		mapping.Content = append(mapping.Content[:index:index], mapping.Content[index+2:]...)
	}
}

// sameAgents reports whether two agent lists configure the same agents in
// the same order.
func sameAgents(left, right []agent.Spec) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !sameAgent(left[index], right[index]) {
			return false
		}
	}
	return true
}

// sameAgent compares two validated specs. An absent and an empty env or
// command are the same configuration.
func sameAgent(left, right agent.Spec) bool {
	return left.ID == right.ID && left.Name == right.Name && left.Shell == right.Shell &&
		left.Cwd == right.Cwd && left.Adapter == right.Adapter && left.Backend == right.Backend &&
		sameStrings(left.Command, right.Command) && sameEnvironment(left.Env, right.Env)
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func sameEnvironment(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for name, value := range left {
		if other, found := right[name]; !found || other != value {
			return false
		}
	}
	return true
}

func equivalentWorkingDirectory(candidate, normalized, baseDir string) bool {
	if strings.TrimSpace(candidate) == "" || strings.TrimSpace(normalized) == "" {
		return strings.TrimSpace(candidate) == "" && strings.TrimSpace(normalized) == ""
	}
	resolved := candidate
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(baseDir, resolved)
	}
	return filepath.Clean(resolved) == filepath.Clean(normalized)
}

func appendStringField(mapping *yaml.Node, key, value string) {
	appendNodeField(mapping, key, stringNode(value))
}

func appendNodeField(mapping *yaml.Node, key string, value *yaml.Node) {
	mapping.Content = append(mapping.Content, stringNode(key), value)
}

func stringNode(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}
