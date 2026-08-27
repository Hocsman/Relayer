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
	ErrRevisionMismatch = errors.New("configuration modifiée depuis son chargement")
	// ErrCommitUncertain means the atomic rename completed but a subsequent
	// durability or verification step failed. Callers must reload before any
	// retry so they never overwrite a publication that may already be visible.
	ErrCommitUncertain = errors.New("publication de configuration effectuée avec état de durabilité incertain")
)

var configurationPathLocks sync.Map
var syncConfigurationDirectory = syncDirectory

// FileRevision returns a content-derived revision suitable for optimistic
// updates. It never includes file contents in errors or diagnostics.
func FileRevision(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("le chemin du fichier de configuration est vide")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("lecture de la configuration: %w", err)
	}
	return contentRevision(data), nil
}

// ReplaceAgents atomically replaces only the version-one agents sequence. It
// preserves every other YAML node, refuses legacy documents, and verifies the
// caller's revision immediately before publication.
func ReplaceAgents(path, expectedRevision string, specs []agent.Spec) (Result, string, error) {
	if strings.TrimSpace(path) == "" {
		return Result{}, "", errors.New("le chemin du fichier de configuration est vide")
	}
	if strings.TrimSpace(expectedRevision) == "" {
		return Result{}, "", errors.New("la révision attendue est vide")
	}
	if len(specs) > maxAgents {
		return Result{}, "", fmt.Errorf("trop d'agents: maximum %d", maxAgents)
	}

	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return Result{}, "", errors.New("résolution du chemin de configuration impossible")
	}
	pathLock, _ := configurationPathLocks.LoadOrStore(filepath.Clean(absolutePath), &sync.Mutex{})
	pathMutex := pathLock.(*sync.Mutex)
	pathMutex.Lock()
	defer pathMutex.Unlock()
	unlockFile, err := lockConfigurationFile(absolutePath)
	if err != nil {
		return Result{}, "", err
	}
	defer unlockFile()

	current, err := Load(path)
	if err != nil {
		return Result{}, "", err
	}
	if current.Legacy {
		return Result{}, "", errors.New("la configuration historique doit être migrée vers version: 1 avant de modifier les agents")
	}
	data, info, err := readRegularConfiguration(path)
	if err != nil {
		return Result{}, "", err
	}
	if contentRevision(data) != expectedRevision {
		return Result{}, "", ErrRevisionMismatch
	}

	baseDir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return Result{}, "", errors.New("résolution du dossier de configuration impossible")
	}
	validated, err := agent.ValidateAll(specs, baseDir, current.Backend)
	if err != nil {
		return Result{}, "", fmt.Errorf("profils d'agents invalides: %w", err)
	}
	if err := validateUpdatedPolicyAgents(current, validated); err != nil {
		return Result{}, "", err
	}
	rendered, err := replaceAgentsYAML(data, validated, specs, baseDir)
	if err != nil {
		return Result{}, "", err
	}

	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".relayer-agents-*.tmp")
	if err != nil {
		return Result{}, "", errors.New("création du fichier temporaire de configuration impossible")
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	mode := info.Mode().Perm()
	if mode == 0 {
		mode = 0o600
	}
	if _, err := temporary.Write(rendered); err != nil {
		_ = temporary.Close()
		return Result{}, "", errors.New("écriture de la configuration impossible")
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return Result{}, "", errors.New("synchronisation de la configuration impossible")
	}
	// CreateTemp starts at 0600. Keep that restrictive mode while bytes are
	// written, then reproduce the existing file mode only after content is
	// complete so a permissive config never exposes a partial temporary file.
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return Result{}, "", errors.New("application des permissions de configuration impossible")
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return Result{}, "", errors.New("synchronisation des permissions de configuration impossible")
	}
	if err := temporary.Close(); err != nil {
		return Result{}, "", errors.New("fermeture de la configuration temporaire impossible")
	}
	if _, err := Load(temporaryPath); err != nil {
		return Result{}, "", fmt.Errorf("configuration mise à jour invalide: %w", err)
	}

	latest, _, err := readRegularConfiguration(path)
	if err != nil {
		return Result{}, "", err
	}
	if contentRevision(latest) != expectedRevision {
		return Result{}, "", ErrRevisionMismatch
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return Result{}, "", errors.New("publication atomique de la configuration impossible")
	}
	if err := syncConfigurationDirectory(directory); err != nil {
		updated, loadErr := Load(path)
		if loadErr != nil {
			return Result{}, contentRevision(rendered), errors.Join(
				ErrCommitUncertain,
				errors.New("synchronisation du dossier de configuration impossible"),
			)
		}
		return updated, updated.Revision, errors.Join(
			ErrCommitUncertain,
			errors.New("synchronisation du dossier de configuration impossible"),
		)
	}

	updated, err := Load(path)
	if err != nil {
		return Result{}, contentRevision(rendered), errors.Join(ErrCommitUncertain, err)
	}
	// The loaded result is authoritative. A non-cooperating editor may have
	// replaced the file after our atomic rename; returning the rendered hash in
	// that case would pair a fresh view with an already-stale revision.
	return updated, updated.Revision, nil
}

func validateUpdatedPolicyAgents(current Result, specs []agent.Spec) error {
	available := make(map[string]struct{}, len(specs))
	for _, spec := range specs {
		available[strings.ToLower(strings.TrimSpace(spec.ID))] = struct{}{}
	}
	for _, rule := range current.Policies.Rules {
		for _, configuredID := range rule.Match.AgentIDs {
			if _, exists := available[strings.ToLower(strings.TrimSpace(configuredID))]; !exists {
				return fmt.Errorf("la règle de politique %q référence un agent absent", rule.Name)
			}
		}
	}
	return nil
}

func readRegularConfiguration(path string) ([]byte, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, errors.New("inspection de la configuration impossible")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, nil, errors.New("la configuration doit être un fichier régulier non symbolique")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, errors.New("lecture de la configuration impossible")
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
		return nil, errors.New("décodage de la configuration impossible")
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("la configuration contient plusieurs documents YAML")
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("la configuration versionnée doit être un mapping YAML")
	}
	agentsNode := mappingValue(document.Content[0], "agents")
	if agentsNode == nil {
		return nil, errors.New("le champ agents est absent")
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
		return nil, errors.New("encodage de la configuration impossible")
	}
	if err := encoder.Close(); err != nil {
		return nil, errors.New("finalisation de la configuration impossible")
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

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
