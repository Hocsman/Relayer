// Package config loads and creates Relayer interception configuration files.
// It deliberately owns YAML concerns so the interception engine remains
// independent from persistence and command-line conventions.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/intercept"
	"github.com/Hocsman/Relayer/internal/policy"
	"gopkg.in/yaml.v3"
)

const (
	DefaultPath    = "config.yaml"
	CurrentVersion = 1
	maxAgents      = 8
)

// ErrExistingConfigRead identifies failures while reading the selected file
// in LoadExisting. Validation errors that happen after the bytes were read,
// including inaccessible agent working directories, never match this value.
var ErrExistingConfigRead = errors.New("could not read existing configuration")

// ExistingConfigReadError retains only a finite read-failure classification
// for errors.Is/errors.As while discarding the selected path and raw
// operating-system details.
type ExistingConfigReadError struct {
	classification error
}

func (err *ExistingConfigReadError) Error() string {
	return ErrExistingConfigRead.Error()
}

func (err *ExistingConfigReadError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.classification
}

func (err *ExistingConfigReadError) Is(target error) bool {
	return target == ErrExistingConfigRead
}

func newExistingConfigReadError(cause error) *ExistingConfigReadError {
	var classification error
	switch {
	case errors.Is(cause, os.ErrNotExist):
		classification = os.ErrNotExist
	case errors.Is(cause, os.ErrPermission):
		classification = os.ErrPermission
	}
	return &ExistingConfigReadError{classification: classification}
}

// Result describes the effective interception configuration and whether the
// loader had to create the file during this call.
type Result struct {
	Version int
	Legacy  bool
	// Revision is a content hash used internally for optimistic file updates.
	// It must not be exposed to an untrusted UI when the file may contain
	// environment values; desktop bridges exchange an opaque random token.
	Revision string
	Backend  string
	Sessions SessionPolicy
	Agents   []agent.Spec
	Patterns []intercept.Pattern
	Policies policy.Config
	Audit    audit.Config
	Created  bool
}

// SessionPolicy controls ownership of detached backend sessions. PTY sessions
// are always process-owned and therefore ignore persistence settings.
type SessionPolicy struct {
	PersistOnExit    bool `yaml:"persist_on_exit"`
	CleanupOnSuccess bool `yaml:"cleanup_on_success"`
}

type configuredSessionPolicy struct {
	PersistOnExit    *bool `yaml:"persist_on_exit,omitempty"`
	CleanupOnSuccess *bool `yaml:"cleanup_on_success,omitempty"`
}

func defaultSessionPolicy() SessionPolicy {
	return SessionPolicy{CleanupOnSuccess: true}
}

// ConfigPattern is the strict YAML representation exposed to users.
type ConfigPattern struct {
	Pattern     string `yaml:"pattern"`
	Description string `yaml:"description"`

	// Sensitive marks a pattern as reading a secret, which masks the operator
	// field and forces a human decision. Relayer also infers this from the
	// pattern text, but that inference is a word list and cannot recognize
	// every prompt; without this field a missed word means an unmasked
	// credential entry with no way to correct it.
	//
	// It can only escalate. `sensitive: false` does not downgrade a pattern the
	// inference already considers sensitive.
	Sensitive *bool `yaml:"sensitive,omitempty"`
}

type legacyFile struct {
	InterceptPatterns []ConfigPattern `yaml:"intercept_patterns"`
}

// versionOneFile is intentionally pointer-based for required collection and
// scalar fields. This lets the strict decoder distinguish a missing key from
// an explicitly empty agents list, which is valid and asks the application to
// provide its historical fallback agents.
type versionOneFile struct {
	Version           *int                     `yaml:"version"`
	Backend           *string                  `yaml:"backend"`
	Sessions          *configuredSessionPolicy `yaml:"sessions,omitempty"`
	Policies          *configuredPolicies      `yaml:"policies,omitempty"`
	Audit             *configuredAudit         `yaml:"audit,omitempty"`
	Agents            *[]configuredAgent       `yaml:"agents"`
	InterceptPatterns *[]ConfigPattern         `yaml:"intercept_patterns"`
}

type configuredPolicies struct {
	DefaultAction *string                 `yaml:"default_action,omitempty"`
	DryRun        *bool                   `yaml:"dry_run,omitempty"`
	Rules         *[]configuredPolicyRule `yaml:"rules,omitempty"`
}

type configuredAudit struct {
	Enabled       *bool   `yaml:"enabled,omitempty"`
	Mode          *string `yaml:"mode,omitempty"`
	Path          *string `yaml:"path,omitempty"`
	MaxFileSizeMB *int    `yaml:"max_file_size_mb,omitempty"`
	MaxFiles      *int    `yaml:"max_files,omitempty"`
}

type configuredPolicyRule struct {
	Name   *string                `yaml:"name"`
	Match  *configuredPolicyMatch `yaml:"match"`
	Action *string                `yaml:"action"`
}

type configuredPolicyMatch struct {
	EventTypes *[]string `yaml:"event_types,omitempty"`
	TextRegex  *string   `yaml:"text_regex,omitempty"`
	AgentIDs   *[]string `yaml:"agent_ids,omitempty"`
	RiskLevels *[]string `yaml:"risk_levels,omitempty"`
	Sensitive  *bool     `yaml:"sensitive,omitempty"`
}

type configuredAgent struct {
	ID      string            `yaml:"id"`
	Name    string            `yaml:"name"`
	Command *[]string         `yaml:"command"`
	Shell   *string           `yaml:"shell"`
	Cwd     string            `yaml:"cwd"`
	Env     map[string]string `yaml:"env"`
	Adapter string            `yaml:"adapter"`
	Backend string            `yaml:"backend"`
}

type decodedFile struct {
	Version  int
	Legacy   bool
	Backend  string
	Sessions SessionPolicy
	Agents   []configuredAgent
	Patterns []ConfigPattern
	Policies policy.Config
	Audit    audit.Config
}

// LoadOrCreate reads path before any PTY is started. It accepts both a direct
// list and the intercept_patterns wrapper documented in the README. A missing
// file is populated atomically with the built-in defaults.
//
// Creating a file is a first-run bootstrap behaviour and belongs to the two
// application entry points alone. Everything else - validating a temporary
// file, re-reading after a commit, answering a desktop query - must use
// LoadExisting, so a path that vanished cannot be resurrected with defaults
// and published over the user's real configuration.
func LoadOrCreate(path string) (Result, error) {
	if strings.TrimSpace(path) == "" {
		return Result{}, errors.New("configuration file path is empty")
	}

	data, err := os.ReadFile(path)
	created := false
	if errors.Is(err, os.ErrNotExist) {
		created, err = createDefault(path)
		if err != nil {
			return Result{}, err
		}
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return Result{}, fmt.Errorf("read %s: %w", path, err)
	}
	return decodeResult(path, data, created)
}

// LoadExisting reads and validates an existing configuration without ever
// creating, replacing or otherwise mutating the path. Everything except
// first-run bootstrap must use this entry point instead of LoadOrCreate, so a
// typo or a vanished file cannot materialize a new default configuration as a
// side effect.
func LoadExisting(path string) (Result, error) {
	if strings.TrimSpace(path) == "" {
		return Result{}, errors.New("configuration file path is empty")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Result{}, newExistingConfigReadError(err)
	}
	return decodeResult(path, data, false)
}

func decodeResult(path string, data []byte, created bool) (Result, error) {

	configured, err := decode(data)
	if err != nil {
		return Result{}, fmt.Errorf("invalid configuration %s: %w", path, err)
	}
	patterns, err := validate(configured.Patterns)
	if err != nil {
		return Result{}, fmt.Errorf("invalid configuration %s: %w", path, err)
	}

	var agents []agent.Spec
	if !configured.Legacy {
		baseDir, absoluteErr := filepath.Abs(filepath.Dir(path))
		if absoluteErr != nil {
			return Result{}, fmt.Errorf("resolve configuration directory %s: %w", path, absoluteErr)
		}
		agents, err = validateAgents(configured.Agents, baseDir, configured.Backend)
		if err != nil {
			return Result{}, fmt.Errorf("invalid configuration %s: %w", path, err)
		}
		if configured.Audit.Path != "" && !filepath.IsAbs(configured.Audit.Path) {
			configured.Audit.Path = filepath.Join(baseDir, configured.Audit.Path)
		}
	}

	return Result{
		Version:  configured.Version,
		Legacy:   configured.Legacy,
		Revision: contentRevision(data),
		Backend:  configured.Backend,
		Sessions: configured.Sessions,
		Agents:   agents,
		Patterns: patterns,
		Policies: configured.Policies,
		Audit:    configured.Audit,
		Created:  created,
	}, nil
}

// DefaultPatterns returns an independent copy of the built-in patterns.
func DefaultPatterns() []intercept.Pattern {
	return intercept.DefaultPatterns()
}

func createDefault(path string) (bool, error) {
	defaults := intercept.DefaultPatterns()
	configured := make([]ConfigPattern, 0, len(defaults))
	for _, pattern := range defaults {
		configured = append(configured, ConfigPattern{
			Pattern:     pattern.Expression,
			Description: pattern.Description,
		})
	}
	version := CurrentVersion
	backend := agent.BackendPTY
	agents := []configuredAgent{}
	defaultPolicies := configuredPoliciesFrom(policy.DefaultConfig())
	defaultAudit := configuredAuditFrom(audit.DefaultConfig())
	payload, err := yaml.Marshal(versionOneFile{
		Version:           &version,
		Backend:           &backend,
		Sessions:          configuredSessionPolicyPointer(defaultSessionPolicy()),
		Policies:          &defaultPolicies,
		Audit:             &defaultAudit,
		Agents:            &agents,
		InterceptPatterns: &configured,
	})
	if err != nil {
		return false, fmt.Errorf("serialize default configuration: %w", err)
	}
	payload = append([]byte("# Configuration de Relayer; agents: [] active les deux mocks.\n"), payload...)

	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return false, fmt.Errorf("create configuration directory %s: %w", directory, err)
	}

	// Publish only a fully written file. Link never replaces an existing user
	// configuration, even if another process creates it concurrently.
	temporary, err := os.CreateTemp(directory, ".relayer-config-*.tmp")
	if err != nil {
		return false, fmt.Errorf("create temporary file for %s: %w", path, err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)

	if err := temporary.Chmod(0o644); err != nil {
		_ = temporary.Close()
		return false, fmt.Errorf("permissions on %s: %w", temporaryPath, err)
	}
	if _, err := temporary.Write(payload); err != nil {
		_ = temporary.Close()
		return false, fmt.Errorf("write %s: %w", temporaryPath, err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return false, fmt.Errorf("sync %s: %w", temporaryPath, err)
	}
	if err := temporary.Close(); err != nil {
		return false, fmt.Errorf("close %s: %w", temporaryPath, err)
	}
	if err := os.Link(temporaryPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, nil
		}
		// Filesystems without hard links keep the no-overwrite guarantee via an
		// exclusive direct creation.
		return createExclusively(path, payload, err)
	}
	return true, nil
}

func configuredAuditFrom(configuration audit.Config) configuredAudit {
	mode := string(configuration.Mode)
	path := configuration.Path
	return configuredAudit{
		Enabled:       boolPointer(configuration.Enabled),
		Mode:          &mode,
		Path:          &path,
		MaxFileSizeMB: intPointer(configuration.MaxFileSizeMB),
		MaxFiles:      intPointer(configuration.MaxFiles),
	}
}

func intPointer(value int) *int {
	return &value
}

func configuredSessionPolicyPointer(policy SessionPolicy) *configuredSessionPolicy {
	return &configuredSessionPolicy{
		PersistOnExit:    boolPointer(policy.PersistOnExit),
		CleanupOnSuccess: boolPointer(policy.CleanupOnSuccess),
	}
}

func boolPointer(value bool) *bool {
	return &value
}

func stringPointer(value string) *string {
	return &value
}

func configuredPoliciesFrom(configuration policy.Config) configuredPolicies {
	defaultAction := string(configuration.DefaultAction)
	dryRun := configuration.DryRun
	rules := make([]configuredPolicyRule, 0, len(configuration.Rules))
	for _, rule := range configuration.Rules {
		name := rule.Name
		action := string(rule.Action)
		match := configuredPolicyMatch{Sensitive: rule.Match.Sensitive}
		if len(rule.Match.EventTypes) > 0 {
			values := make([]string, len(rule.Match.EventTypes))
			for index, value := range rule.Match.EventTypes {
				values[index] = string(value)
			}
			match.EventTypes = &values
		}
		if rule.Match.TextRegex != "" {
			match.TextRegex = stringPointer(rule.Match.TextRegex)
		}
		if len(rule.Match.AgentIDs) > 0 {
			values := append([]string(nil), rule.Match.AgentIDs...)
			match.AgentIDs = &values
		}
		if len(rule.Match.RiskLevels) > 0 {
			values := make([]string, len(rule.Match.RiskLevels))
			for index, value := range rule.Match.RiskLevels {
				values[index] = string(value)
			}
			match.RiskLevels = &values
		}
		rules = append(rules, configuredPolicyRule{
			Name:   &name,
			Match:  &match,
			Action: &action,
		})
	}
	return configuredPolicies{
		DefaultAction: &defaultAction,
		DryRun:        &dryRun,
		Rules:         &rules,
	}
}

func decodePolicies(configured *configuredPolicies) (policy.Config, error) {
	result := policy.DefaultConfig()
	if configured == nil {
		return result, nil
	}
	if configured.DefaultAction != nil {
		result.DefaultAction = policy.Action(strings.ToLower(strings.TrimSpace(*configured.DefaultAction)))
	}
	if configured.DryRun != nil {
		result.DryRun = *configured.DryRun
	}
	if configured.Rules == nil {
		return result, nil
	}
	result.Rules = make([]policy.Rule, 0, len(*configured.Rules))
	for index, configuredRule := range *configured.Rules {
		if configuredRule.Name == nil {
			return policy.Config{}, fmt.Errorf("missing policies.rules[%d].name", index)
		}
		if configuredRule.Match == nil {
			return policy.Config{}, fmt.Errorf("missing policies.rules[%d].match", index)
		}
		if configuredRule.Action == nil {
			return policy.Config{}, fmt.Errorf("policies.rules[%d].action manquante", index)
		}
		configuredMatch := configuredRule.Match
		match := policy.Match{Sensitive: configuredMatch.Sensitive}
		if configuredMatch.EventTypes != nil {
			match.EventTypes = make([]adapters.EventType, len(*configuredMatch.EventTypes))
			for valueIndex, value := range *configuredMatch.EventTypes {
				match.EventTypes[valueIndex] = adapters.EventType(strings.ToLower(strings.TrimSpace(value)))
			}
		}
		if configuredMatch.TextRegex != nil {
			if strings.TrimSpace(*configuredMatch.TextRegex) == "" {
				return policy.Config{}, fmt.Errorf("policies.rules[%d].match.text_regex cannot be empty", index)
			}
			match.TextRegex = *configuredMatch.TextRegex
		}
		if configuredMatch.AgentIDs != nil {
			match.AgentIDs = append([]string(nil), (*configuredMatch.AgentIDs)...)
		}
		if configuredMatch.RiskLevels != nil {
			match.RiskLevels = make([]adapters.RiskLevel, len(*configuredMatch.RiskLevels))
			for valueIndex, value := range *configuredMatch.RiskLevels {
				match.RiskLevels[valueIndex] = adapters.RiskLevel(strings.ToLower(strings.TrimSpace(value)))
			}
		}
		result.Rules = append(result.Rules, policy.Rule{
			Name:   *configuredRule.Name,
			Match:  match,
			Action: policy.Action(strings.ToLower(strings.TrimSpace(*configuredRule.Action))),
		})
	}
	return result, nil
}

func decodeAudit(configured *configuredAudit) (audit.Config, error) {
	if configured == nil {
		return disabledAuditConfig(), nil
	}
	result := audit.DefaultConfig()
	if configured.Enabled != nil {
		result.Enabled = *configured.Enabled
	}
	if configured.Mode != nil {
		result.Mode = audit.Mode(strings.ToLower(strings.TrimSpace(*configured.Mode)))
	}
	if configured.Path != nil {
		result.Path = strings.TrimSpace(*configured.Path)
	}
	if configured.MaxFileSizeMB != nil {
		result.MaxFileSizeMB = *configured.MaxFileSizeMB
	}
	if configured.MaxFiles != nil {
		result.MaxFiles = *configured.MaxFiles
	}
	if err := audit.Validate(result); err != nil {
		return audit.Config{}, fmt.Errorf("invalid audit: %w", err)
	}
	return result, nil
}

func disabledAuditConfig() audit.Config {
	result := audit.DefaultConfig()
	result.Enabled = false
	result.Mode = audit.ModeOff
	return result
}

func createExclusively(path string, payload []byte, linkErr error) (bool, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("publish %s (link unavailable: %v): %w", path, linkErr, err)
	}
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(path)
	}
	if _, err := file.Write(payload); err != nil {
		cleanup()
		return false, fmt.Errorf("write %s: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		cleanup()
		return false, fmt.Errorf("sync %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return false, fmt.Errorf("close %s: %w", path, err)
	}
	return true, nil
}

func decode(data []byte) (decodedFile, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return decodedFile{}, err
	}
	if len(document.Content) == 0 {
		return decodedFile{}, errors.New("YAML document is empty")
	}

	root := document.Content[0]
	switch root.Kind {
	case yaml.SequenceNode:
		if err := validatePatternNode(root); err != nil {
			return decodedFile{}, err
		}
		var direct []ConfigPattern
		if err := decodeStrict(data, &direct); err != nil {
			return decodedFile{}, err
		}
		return decodedFile{
			Legacy:   true,
			Backend:  agent.BackendPTY,
			Sessions: defaultSessionPolicy(),
			Patterns: direct,
			Policies: policy.DefaultConfig(),
			Audit:    disabledAuditConfig(),
		}, nil
	case yaml.MappingNode:
		if mappingHasKey(root, "version") {
			return decodeVersionOne(data, root)
		}
		if sequence := mappingValue(root, "intercept_patterns"); sequence != nil {
			if err := validatePatternNode(sequence); err != nil {
				return decodedFile{}, err
			}
		}
		var wrapped legacyFile
		if err := decodeStrict(data, &wrapped); err != nil {
			return decodedFile{}, err
		}
		return decodedFile{
			Legacy:   true,
			Backend:  agent.BackendPTY,
			Sessions: defaultSessionPolicy(),
			Patterns: wrapped.InterceptPatterns,
			Policies: policy.DefaultConfig(),
			Audit:    disabledAuditConfig(),
		}, nil
	default:
		return decodedFile{}, errors.New("YAML root must be a list or a configuration object")
	}
}

func decodeVersionOne(data []byte, root *yaml.Node) (decodedFile, error) {
	if err := rejectVersionOneIndirections(root); err != nil {
		return decodedFile{}, err
	}
	if err := validateVersionOneNode(root); err != nil {
		return decodedFile{}, err
	}

	var configured versionOneFile
	if err := decodeStrict(data, &configured); err != nil {
		return decodedFile{}, err
	}
	if configured.Version == nil {
		return decodedFile{}, errors.New("version manquante")
	}
	if *configured.Version != CurrentVersion {
		return decodedFile{}, fmt.Errorf("unsupported version %d (expected: %d)", *configured.Version, CurrentVersion)
	}
	if configured.Backend == nil {
		return decodedFile{}, errors.New("missing backend")
	}
	backend := strings.TrimSpace(*configured.Backend)
	if !agent.IsSupportedBackend(backend) {
		return decodedFile{}, fmt.Errorf(
			"unsupported backend %q (expected: %s, %s or %s)",
			*configured.Backend,
			agent.BackendPTY,
			agent.BackendTmux,
			agent.BackendAuto,
		)
	}
	if configured.Agents == nil {
		return decodedFile{}, errors.New("missing agents")
	}
	if len(*configured.Agents) > maxAgents {
		return decodedFile{}, fmt.Errorf("%d agents configured; maximum allowed: %d", len(*configured.Agents), maxAgents)
	}
	if configured.InterceptPatterns == nil {
		return decodedFile{}, errors.New("missing intercept_patterns")
	}
	sessionPolicy := defaultSessionPolicy()
	if configured.Sessions != nil {
		if configured.Sessions.PersistOnExit != nil {
			sessionPolicy.PersistOnExit = *configured.Sessions.PersistOnExit
		}
		if configured.Sessions.CleanupOnSuccess != nil {
			sessionPolicy.CleanupOnSuccess = *configured.Sessions.CleanupOnSuccess
		}
	}
	configuredPolicy, err := decodePolicies(configured.Policies)
	if err != nil {
		return decodedFile{}, err
	}
	if _, err := policy.New(configuredPolicy); err != nil {
		return decodedFile{}, fmt.Errorf("policies invalides: %w", err)
	}
	configuredAudit, err := decodeAudit(configured.Audit)
	if err != nil {
		return decodedFile{}, err
	}

	return decodedFile{
		Version:  *configured.Version,
		Backend:  backend,
		Sessions: sessionPolicy,
		Agents:   append([]configuredAgent(nil), (*configured.Agents)...),
		Patterns: append([]ConfigPattern(nil), (*configured.InterceptPatterns)...),
		Policies: configuredPolicy,
		Audit:    configuredAudit,
	}, nil
}

// Versioned configuration is intentionally explicit: aliases and merge keys
// can hide fields from the node-level type checks and make review of execution
// settings harder. Legacy pattern-only documents keep their historical YAML
// behavior, while v1 requires every executable field to appear directly.
func rejectVersionOneIndirections(node *yaml.Node) error {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.AliasNode {
		return errors.New("YAML aliases are not allowed in a versioned configuration")
	}
	if node.Kind == yaml.MappingNode {
		for index := 0; index+1 < len(node.Content); index += 2 {
			if node.Content[index].Value == "<<" {
				return errors.New("YAML merge keys are not allowed in a versioned configuration")
			}
		}
	}
	for _, child := range node.Content {
		if err := rejectVersionOneIndirections(child); err != nil {
			return err
		}
	}
	return nil
}

func mappingHasKey(root *yaml.Node, name string) bool {
	return mappingValue(root, name) != nil
}

func mappingValue(root *yaml.Node, name string) *yaml.Node {
	if root.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(root.Content); index += 2 {
		if root.Content[index].Value == name {
			return root.Content[index+1]
		}
	}
	return nil
}

func validateVersionOneNode(root *yaml.Node) error {
	for index := 0; index+1 < len(root.Content); index += 2 {
		name := root.Content[index].Value
		value := dereferenceAlias(root.Content[index+1])
		switch name {
		case "version":
			if err := requireScalar(value, "!!int", "version doit être un entier YAML"); err != nil {
				return err
			}
		case "backend":
			if err := requireScalar(value, "!!str", "backend doit être une chaîne YAML"); err != nil {
				return err
			}
		case "sessions":
			if err := validateSessionPolicyNode(value); err != nil {
				return err
			}
		case "policies":
			if err := validatePoliciesNode(value); err != nil {
				return err
			}
		case "audit":
			if err := validateAuditNode(value); err != nil {
				return err
			}
		case "agents":
			if err := validateAgentNode(value); err != nil {
				return err
			}
		case "intercept_patterns":
			if err := validatePatternNode(value); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateAuditNode(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return errors.New("audit must be a YAML object")
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		name := node.Content[index].Value
		value := dereferenceAlias(node.Content[index+1])
		switch name {
		case "enabled":
			if err := requireScalar(value, "!!bool", "audit.enabled doit être un booléen YAML"); err != nil {
				return err
			}
		case "mode", "path":
			if err := requireScalar(value, "!!str", "audit."+name+" doit être une chaîne YAML"); err != nil {
				return err
			}
		case "max_file_size_mb", "max_files":
			if err := requireScalar(value, "!!int", "audit."+name+" doit être un entier YAML"); err != nil {
				return err
			}
		}
	}
	return nil
}

func validatePoliciesNode(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return errors.New("policies must be a YAML object")
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		name := node.Content[index].Value
		value := dereferenceAlias(node.Content[index+1])
		switch name {
		case "default_action":
			if err := requireScalar(value, "!!str", "policies.default_action doit être une chaîne YAML"); err != nil {
				return err
			}
		case "dry_run":
			if err := requireScalar(value, "!!bool", "policies.dry_run doit être un booléen YAML"); err != nil {
				return err
			}
		case "rules":
			if value.Kind != yaml.SequenceNode {
				return errors.New("policies.rules must be a YAML list")
			}
			for ruleIndex, rawRule := range value.Content {
				if err := validatePolicyRuleNode(dereferenceAlias(rawRule), ruleIndex); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validatePolicyRuleNode(node *yaml.Node, index int) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("policies.rules[%d] must be a YAML object", index)
	}
	for fieldIndex := 0; fieldIndex+1 < len(node.Content); fieldIndex += 2 {
		name := node.Content[fieldIndex].Value
		value := dereferenceAlias(node.Content[fieldIndex+1])
		switch name {
		case "name", "action":
			if err := requireScalar(value, "!!str", fmt.Sprintf("policies.rules[%d].%s doit être une chaîne YAML", index, name)); err != nil {
				return err
			}
		case "match":
			if err := validatePolicyMatchNode(value, index); err != nil {
				return err
			}
		}
	}
	return nil
}

func validatePolicyMatchNode(node *yaml.Node, ruleIndex int) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("policies.rules[%d].match must be a YAML object", ruleIndex)
	}
	for fieldIndex := 0; fieldIndex+1 < len(node.Content); fieldIndex += 2 {
		name := node.Content[fieldIndex].Value
		value := dereferenceAlias(node.Content[fieldIndex+1])
		switch name {
		case "text_regex":
			if err := requireScalar(value, "!!str", fmt.Sprintf("policies.rules[%d].match.text_regex doit être une chaîne YAML", ruleIndex)); err != nil {
				return err
			}
		case "sensitive":
			if err := requireScalar(value, "!!bool", fmt.Sprintf("policies.rules[%d].match.sensitive doit être un booléen YAML", ruleIndex)); err != nil {
				return err
			}
		case "event_types", "agent_ids", "risk_levels":
			if value.Kind != yaml.SequenceNode {
				return fmt.Errorf("policies.rules[%d].match.%s must be a YAML list", ruleIndex, name)
			}
			if len(value.Content) == 0 {
				return fmt.Errorf("policies.rules[%d].match.%s cannot be empty", ruleIndex, name)
			}
			for elementIndex, element := range value.Content {
				if err := requireScalar(
					dereferenceAlias(element),
					"!!str",
					fmt.Sprintf("policies.rules[%d].match.%s[%d] doit être une chaîne YAML", ruleIndex, name, elementIndex),
				); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validateSessionPolicyNode(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return errors.New("sessions must be a YAML object")
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		name := node.Content[index].Value
		value := dereferenceAlias(node.Content[index+1])
		switch name {
		case "persist_on_exit", "cleanup_on_success":
			if err := requireScalar(value, "!!bool", "sessions."+name+" doit être un booléen YAML"); err != nil {
				return err
			}
		}
	}
	return nil
}

func validatePatternNode(sequence *yaml.Node) error {
	sequence = dereferenceAlias(sequence)
	if sequence.Kind != yaml.SequenceNode {
		return errors.New("intercept_patterns must be a YAML list")
	}
	for entryIndex, rawEntry := range sequence.Content {
		entry := dereferenceAlias(rawEntry)
		if entry.Kind != yaml.MappingNode {
			return fmt.Errorf("pattern %d must be a YAML object", entryIndex+1)
		}
		for fieldIndex := 0; fieldIndex+1 < len(entry.Content); fieldIndex += 2 {
			name := entry.Content[fieldIndex].Value
			if name == "pattern" || name == "description" {
				value := dereferenceAlias(entry.Content[fieldIndex+1])
				if err := requireScalar(value, "!!str", fmt.Sprintf("pattern %d: %s doit être une chaîne YAML", entryIndex+1, name)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validateAgentNode(sequence *yaml.Node) error {
	sequence = dereferenceAlias(sequence)
	if sequence.Kind != yaml.SequenceNode {
		return errors.New("agents must be a YAML list")
	}
	for entryIndex, rawEntry := range sequence.Content {
		entry := dereferenceAlias(rawEntry)
		if entry.Kind != yaml.MappingNode {
			return fmt.Errorf("agent %d must be a YAML object", entryIndex+1)
		}
		for fieldIndex := 0; fieldIndex+1 < len(entry.Content); fieldIndex += 2 {
			name := entry.Content[fieldIndex].Value
			value := dereferenceAlias(entry.Content[fieldIndex+1])
			switch name {
			case "id", "name", "shell", "cwd", "adapter", "backend":
				if err := requireScalar(value, "!!str", fmt.Sprintf("agent %d: %s doit être une chaîne YAML", entryIndex+1, name)); err != nil {
					return err
				}
			case "command":
				if value.Kind != yaml.SequenceNode {
					return fmt.Errorf("agent %d: command must be a YAML list of strings", entryIndex+1)
				}
				for argumentIndex, argument := range value.Content {
					if err := requireScalar(dereferenceAlias(argument), "!!str", fmt.Sprintf("agent %d: command[%d] doit être une chaîne YAML", entryIndex+1, argumentIndex)); err != nil {
						return err
					}
				}
			case "env":
				if value.Kind != yaml.MappingNode {
					return fmt.Errorf("agent %d: env must be a YAML string-to-string object", entryIndex+1)
				}
				for envIndex := 0; envIndex+1 < len(value.Content); envIndex += 2 {
					key := dereferenceAlias(value.Content[envIndex])
					envValue := dereferenceAlias(value.Content[envIndex+1])
					if err := requireScalar(key, "!!str", fmt.Sprintf("agent %d: les noms d'environnement doivent être des chaînes YAML", entryIndex+1)); err != nil {
						return err
					}
					if err := requireScalar(envValue, "!!str", fmt.Sprintf("agent %d: env[%s] doit être une chaîne YAML", entryIndex+1, key.Value)); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func requireScalar(node *yaml.Node, tag string, message string) error {
	if node.Kind != yaml.ScalarNode || node.Tag != tag {
		return errors.New(message)
	}
	return nil
}

func dereferenceAlias(node *yaml.Node) *yaml.Node {
	for node != nil && node.Kind == yaml.AliasNode && node.Alias != nil {
		node = node.Alias
	}
	return node
}

func validateAgents(configured []configuredAgent, baseDir, defaultBackend string) ([]agent.Spec, error) {
	specs := make([]agent.Spec, 0, len(configured))
	for index, entry := range configured {
		if entry.Command != nil && entry.Shell != nil {
			return nil, fmt.Errorf("agent %d: command and shell are mutually exclusive", index+1)
		}

		var command []string
		if entry.Command != nil {
			command = append([]string(nil), (*entry.Command)...)
		}
		shell := ""
		if entry.Shell != nil {
			shell = *entry.Shell
		}
		specs = append(specs, agent.Spec{
			ID:      entry.ID,
			Name:    entry.Name,
			Command: command,
			Shell:   shell,
			Cwd:     entry.Cwd,
			Env:     entry.Env,
			Adapter: entry.Adapter,
			Backend: entry.Backend,
		})
	}

	validated, err := agent.ValidateAll(specs, baseDir, defaultBackend)
	if err != nil {
		return nil, err
	}
	return validated, nil
}

func decodeStrict(data []byte, target any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple YAML documents are not allowed")
		}
		return err
	}
	return nil
}

func validate(configured []ConfigPattern) ([]intercept.Pattern, error) {
	if len(configured) == 0 {
		return nil, errors.New("no interception pattern is defined")
	}

	patterns := make([]intercept.Pattern, 0, len(configured))
	for index, pattern := range configured {
		if strings.TrimSpace(pattern.Pattern) == "" {
			return nil, fmt.Errorf("entry %d: missing pattern", index+1)
		}
		if strings.TrimSpace(pattern.Description) == "" {
			return nil, fmt.Errorf("entry %d: missing description", index+1)
		}
		if _, err := regexp.Compile(pattern.Pattern); err != nil {
			return nil, fmt.Errorf("entry %d: invalid regex: %w", index+1, err)
		}
		patterns = append(patterns, intercept.Pattern{
			Name:        inferName(pattern, index),
			Description: strings.TrimSpace(pattern.Description),
			Expression:  pattern.Pattern,
			Sensitive:   isSensitive(pattern),
		})
	}
	return patterns, nil
}

func inferName(pattern ConfigPattern, index int) string {
	text := strings.ToLower(pattern.Pattern + " " + pattern.Description)
	switch {
	case isSensitive(pattern):
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

// isSensitive combines the declared value with the inferred one. The two are
// OR-ed rather than overriding each other: an explicit declaration must be able
// to catch a prompt the word list misses, and must never be able to turn off a
// masking decision the inference already made.
func isSensitive(pattern ConfigPattern) bool {
	if pattern.Sensitive != nil && *pattern.Sensitive {
		return true
	}
	return intercept.IsSensitiveText(pattern.Pattern + " " + pattern.Description)
}
