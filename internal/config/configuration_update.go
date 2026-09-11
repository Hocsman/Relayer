package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/notify"
	"github.com/Hocsman/Relayer/internal/policy"
	"gopkg.in/yaml.v3"
)

// FullConfigurationUpdate defines atomic updates to the configuration file.
// If a pointer/slice is nil, that section remains untouched.
type FullConfigurationUpdate struct {
	Agents        []agent.Spec
	UpdateAgents  bool // Explicit flag indicating Agents slice should replace current agents
	Policies      *policy.Config
	Notifications *notify.Config
}

// UpdateFullConfiguration atomically applies the requested updates to path,
// verifying expectedRevision first. It preserves unreferenced YAML blocks and comments.
func UpdateFullConfiguration(path, expectedRevision string, update FullConfigurationUpdate) (Result, string, error) {
	if strings.TrimSpace(path) == "" {
		return Result{}, "", errors.New("configuration file path is empty")
	}
	if strings.TrimSpace(expectedRevision) == "" {
		return Result{}, "", errors.New("expected revision is empty")
	}
	if update.UpdateAgents && len(update.Agents) > maxAgents {
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
		return Result{}, "", errors.New("legacy configuration must be migrated to version: 1 before updating")
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

	// Validate agents if updated
	effectiveAgents := current.Agents
	var validatedAgents []agent.Spec
	if update.UpdateAgents {
		validated, err := agent.ValidateAll(update.Agents, baseDir, current.Backend)
		if err != nil {
			return Result{}, "", fmt.Errorf("invalid agent profiles: %w", err)
		}
		validatedAgents = validated
		effectiveAgents = validated
	}

	// Validate policies if updated
	effectivePolicies := current.Policies
	if update.Policies != nil {
		if _, err := policy.New(*update.Policies); err != nil {
			return Result{}, "", fmt.Errorf("invalid policy configuration: %w", err)
		}
		effectivePolicies = *update.Policies
	}
	if err := validateUpdatedPolicyAgentsWith(effectivePolicies, effectiveAgents); err != nil {
		return Result{}, "", err
	}

	// Validate notifications if updated
	if update.Notifications != nil {
		if err := notify.Validate(*update.Notifications); err != nil {
			return Result{}, "", fmt.Errorf("invalid notifications configuration: %w", err)
		}
	}

	rendered, err := replaceFullConfigurationYAML(data, update, validatedAgents, baseDir)
	if err != nil {
		return Result{}, "", err
	}
	publishedRevision := contentRevision(rendered)

	directory := filepath.Dir(absolutePath)
	temporary, err := os.CreateTemp(directory, ".relayer-config-*.tmp")
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

	var renameErr error
	for attempt := 0; attempt < 5; attempt++ {
		renameErr = os.Rename(temporaryPath, absolutePath)
		if renameErr == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if renameErr != nil {
		return Result{}, "", errors.New("could not atomically publish configuration")
	}
	if err := syncConfigurationDirectory(directory); err != nil {
		updated, loadErr := LoadExisting(absolutePath)
		if loadErr != nil {
			return Result{}, publishedRevision, errors.Join(
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
		return updated, updated.Revision, ErrRevisionMismatch
	}
	return updated, updated.Revision, nil
}

func validateUpdatedPolicyAgentsWith(policies policy.Config, specs []agent.Spec) error {
	available := make(map[string]struct{}, len(specs))
	for _, spec := range specs {
		available[strings.ToLower(strings.TrimSpace(spec.ID))] = struct{}{}
	}
	for _, rule := range policies.Rules {
		for _, configuredID := range rule.Match.AgentIDs {
			if _, exists := available[strings.ToLower(strings.TrimSpace(configuredID))]; !exists {
				return fmt.Errorf("policy rule %q references a missing agent", rule.Name)
			}
		}
	}
	return nil
}

func replaceFullConfigurationYAML(
	data []byte,
	update FullConfigurationUpdate,
	validatedAgents []agent.Spec,
	baseDir string,
) ([]byte, error) {
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
	root := document.Content[0]

	// 1. Update agents if requested
	if update.UpdateAgents {
		agentsNode := mappingValue(root, "agents")
		replacement := agentSequenceNode(validatedAgents, update.Agents, agentsNode, baseDir)
		if agentsNode != nil {
			replacement.HeadComment = agentsNode.HeadComment
			replacement.LineComment = agentsNode.LineComment
			replacement.FootComment = agentsNode.FootComment
			*agentsNode = *replacement
		} else {
			setMappingField(root, "agents", replacement)
		}
	}

	// 2. Update policies if requested
	if update.Policies != nil {
		configured := configuredPoliciesFrom(*update.Policies)
		node, err := yamlNodeFrom(configured)
		if err != nil {
			return nil, fmt.Errorf("could not encode policies: %w", err)
		}
		setMappingField(root, "policies", node)
	}

	// 3. Update notifications if requested
	if update.Notifications != nil {
		configured := configuredNotificationsPointer(*update.Notifications)
		node, err := yamlNodeFrom(configured)
		if err != nil {
			return nil, fmt.Errorf("could not encode notifications: %w", err)
		}
		setMappingField(root, "notifications", node)
	}

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

func setMappingField(mapping *yaml.Node, key string, value *yaml.Node) {
	if mapping.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			value.HeadComment = mapping.Content[i+1].HeadComment
			value.LineComment = mapping.Content[i+1].LineComment
			value.FootComment = mapping.Content[i+1].FootComment
			mapping.Content[i+1] = value
			return
		}
	}
	mapping.Content = append(mapping.Content, stringNode(key), value)
}

func yamlNodeFrom(val any) (*yaml.Node, error) {
	raw, err := yaml.Marshal(val)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 1 {
		return doc.Content[0], nil
	}
	return &doc, nil
}
