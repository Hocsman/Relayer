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
	rendered, err := replaceAgentsYAML(data, validated, specs, baseDir)
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
	if _, err := LoadExisting(temporaryPath); err != nil {
		return Result{}, "", fmt.Errorf("invalid updated configuration: %w", err)
	}

	latest, _, err := readRegularConfiguration(absolutePath)
	if err != nil {
		return Result{}, "", err
	}
	if contentRevision(latest) != expectedRevision {
		return Result{}, "", ErrRevisionMismatch
	}
	if err := os.Rename(temporaryPath, absolutePath); err != nil {
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
	if err := os.Rename(temporaryPath, path); err != nil {
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

func replaceAgentsYAML(data []byte, specs, requested []agent.Spec, baseDir string) ([]byte, error) {
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
	replacement := agentSequenceNode(specs, requested, agentsNode, baseDir)
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

func agentSequenceNode(specs, requested []agent.Spec, previous *yaml.Node, baseDir string) *yaml.Node {
	previousByID := make(map[string]*yaml.Node)
	if previous != nil && previous.Kind == yaml.SequenceNode {
		for _, entry := range previous.Content {
			if entry.Kind != yaml.MappingNode {
				continue
			}
			id := mappingValue(entry, "id")
			if id != nil && id.Kind == yaml.ScalarNode {
				previousByID[strings.ToLower(strings.TrimSpace(id.Value))] = entry
			}
		}
	}
	sequence := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for index, spec := range specs {
		entry := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		appendStringField(entry, "id", spec.ID)
		appendStringField(entry, "name", spec.Name)
		if len(spec.Command) > 0 {
			command := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
			for _, argument := range spec.Command {
				command.Content = append(command.Content, stringNode(argument))
			}
			appendNodeField(entry, "command", command)
		} else {
			appendStringField(entry, "shell", spec.Shell)
		}
		cwd := spec.Cwd
		preservedPrevious := false
		if previousEntry := previousByID[strings.ToLower(strings.TrimSpace(spec.ID))]; previousEntry != nil {
			if previousCwd := mappingValue(previousEntry, "cwd"); previousCwd != nil &&
				previousCwd.Kind == yaml.ScalarNode && equivalentWorkingDirectory(previousCwd.Value, spec.Cwd, baseDir) {
				cwd = previousCwd.Value
				preservedPrevious = true
			}
		}
		if !preservedPrevious && index < len(requested) && equivalentWorkingDirectory(requested[index].Cwd, spec.Cwd, baseDir) {
			cwd = requested[index].Cwd
		}
		if cwd != "" {
			appendStringField(entry, "cwd", cwd)
		}
		if len(spec.Env) > 0 {
			environment := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			names := make([]string, 0, len(spec.Env))
			for name := range spec.Env {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				appendStringField(environment, name, spec.Env[name])
			}
			appendNodeField(entry, "env", environment)
		}
		if spec.Adapter != "" {
			appendStringField(entry, "adapter", spec.Adapter)
		}
		if spec.Backend != "" {
			appendStringField(entry, "backend", spec.Backend)
		}
		sequence.Content = append(sequence.Content, entry)
	}
	return sequence
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
