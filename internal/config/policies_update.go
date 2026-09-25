package config

import (
	"errors"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"github.com/Hocsman/Relayer/internal/policy"
	"gopkg.in/yaml.v3"
)

// samePolicy reports whether two policies are the same configuration. They
// are compared in the form the file writes, where an absent and an empty list
// are the same.
func samePolicy(left, right policy.Config) bool {
	return reflect.DeepEqual(configuredPoliciesFrom(left), configuredPoliciesFrom(right))
}

// policiesNode returns the policies block to write for requested, given the
// block the file has, which loads as current.
//
// The block used to be rebuilt from the effective policy, so every security
// save dropped what that rebuild does not write: "profile: custom", each
// guardrail set to false, the relative workspace_root, which the loader had
// resolved and the save wrote back as an absolute path, and every comment and
// flow sequence of the block. The block is now edited: a field whose value
// the file already gives keeps its text, and each field that differs is
// written where it is, or added to its section. A profile the file names
// stays, as the base the fields it does not set come from; when the editor
// switches the policy to a preset, the file names that preset instead. Should
// no edit of the block load as requested — a profile whose own rules the
// requested ones do not end with — the profile is dropped, and failing that
// the block is rebuilt as before.
//
// chosen is the preset the editor shows as chosen, if the caller has one. A
// preset the editor switched to is recognized by it, and failing that, as it
// was before, by the requested policy matching a preset exactly: a preset
// chosen and then adjusted matched none, so the profile line was dropped with
// its comments, and every field of the preset was written out.
func policiesNode(existing *yaml.Node, current, requested policy.Config, chosen, configDir string) (*yaml.Node, error) {
	if existing != nil && existing.Kind == yaml.MappingNode {
		var candidates []func(*yaml.Node)
		keep := func(*yaml.Node) {}
		if fieldIndex(existing, "profile") >= 0 {
			shown := policy.SettingsFrom(current).Profile
			preset := policy.SettingsFrom(requested).Profile
			if profile, err := policy.ParseProfile(chosen); err == nil && profile != policy.ProfileCustom && string(profile) != shown {
				preset = string(profile)
			}
			if preset != string(policy.ProfileCustom) && preset != shown {
				candidates = append(candidates, func(node *yaml.Node) { setTypedField(node, "profile", "!!str", preset) })
			}
			candidates = append(candidates, keep, func(node *yaml.Node) { removeField(node, "profile") })
		} else {
			candidates = append(candidates, keep)
		}
		for _, prepare := range candidates {
			candidate := copyNode(existing)
			prepare(candidate)
			if convergePolicies(candidate, requested, configDir) {
				return candidate, nil
			}
		}
	}
	configured := configuredPoliciesFrom(requested)
	if guardrails := configured.Guardrails; guardrails != nil && guardrails.WorkspaceRoot != nil {
		root := workspaceRootText(*guardrails.WorkspaceRoot, configDir)
		guardrails.WorkspaceRoot = &root
	}
	rebuilt, err := yamlNodeFrom(configured)
	if err != nil {
		return nil, err
	}
	if decoded, err := decodePoliciesNode(rebuilt, configDir); err != nil || !samePolicy(decoded, requested) {
		return nil, errors.New("the policies could not be written as requested")
	}
	return rebuilt, nil
}

// convergePolicies writes into node each field that loads differently from
// requested, until node loads as requested. A field can depend on another —
// the workspace root on the profile, a guardrail on the root — so it takes
// a few rounds at most.
func convergePolicies(node *yaml.Node, requested policy.Config, configDir string) bool {
	for round := 0; round < 4; round++ {
		decoded, err := decodePoliciesNode(node, configDir)
		if err != nil {
			return false
		}
		if samePolicy(decoded, requested) {
			return true
		}
		writePolicyDifferences(node, decoded, requested, configDir)
	}
	return false
}

func decodePoliciesNode(node *yaml.Node, configDir string) (policy.Config, error) {
	var configured configuredPolicies
	if err := node.Decode(&configured); err != nil {
		return policy.Config{}, err
	}
	decoded, err := decodePolicies(&configured, configDir)
	if err != nil {
		return policy.Config{}, err
	}
	if _, err := policy.New(decoded); err != nil {
		return policy.Config{}, err
	}
	return decoded, nil
}

func writePolicyDifferences(node *yaml.Node, decoded, requested policy.Config, configDir string) {
	if decoded.DefaultAction != requested.DefaultAction {
		setTypedField(node, "default_action", "!!str", string(requested.DefaultAction))
	}
	if decoded.DryRun != requested.DryRun {
		setTypedField(node, "dry_run", "!!bool", strconv.FormatBool(requested.DryRun))
	}
	if decoded.RateLimitPerMinute != requested.RateLimitPerMinute {
		setTypedField(node, "rate_limit_per_minute", "!!int", strconv.Itoa(requested.RateLimitPerMinute))
	}
	if decoded.MaxConsecutiveAutoDecisions != requested.MaxConsecutiveAutoDecisions {
		setTypedField(node, "max_consecutive_auto_decisions", "!!int", strconv.Itoa(requested.MaxConsecutiveAutoDecisions))
	}

	was, want := decoded.Guardrails, requested.Guardrails
	guardrails := func() *yaml.Node {
		section := mappingValue(node, "guardrails")
		if section == nil || section.Kind != yaml.MappingNode {
			section = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			setNodeField(node, "guardrails", section)
		}
		return section
	}
	for _, flag := range []struct {
		key       string
		was, want bool
	}{
		{"block_destructive", was.BlockDestructive, want.BlockDestructive},
		{"block_exfiltration", was.BlockExfiltration, want.BlockExfiltration},
		{"block_sensitive_paths", was.BlockSensitivePaths, want.BlockSensitivePaths},
		{"block_outside_workspace", was.BlockOutsideWorkspace, want.BlockOutsideWorkspace},
	} {
		if flag.was != flag.want {
			setTypedField(guardrails(), flag.key, "!!bool", strconv.FormatBool(flag.want))
		}
	}
	// The workspace guardrail brings its own root, the configuration's
	// directory, when the file names none. A root is written only once the
	// guardrail is as requested and it still differs: written in the same
	// round, the default root was pinned as an absolute path, which no longer
	// follows the file when it is moved.
	if was.WorkspaceRoot != want.WorkspaceRoot && was.BlockOutsideWorkspace == want.BlockOutsideWorkspace {
		setTypedField(guardrails(), "workspace_root", "!!str", workspaceRootText(want.WorkspaceRoot, configDir))
	}
	if !reflect.DeepEqual(nonEmpty(was.BlockedPatterns), nonEmpty(want.BlockedPatterns)) {
		section := guardrails()
		style := yaml.Style(0)
		if previous := mappingValue(section, "blocked_patterns"); previous != nil {
			style = previous.Style & yaml.FlowStyle
		}
		setNodeField(section, "blocked_patterns", stringSequenceNode(want.BlockedPatterns, style))
	}

	if !reflect.DeepEqual(configuredPoliciesFrom(policy.Config{Rules: decoded.Rules}).Rules,
		configuredPoliciesFrom(policy.Config{Rules: requested.Rules}).Rules) {
		// The rules a profile brings follow the file's own; the file writes
		// only the requested rules that come before them.
		explicit := requested.Rules
		if base := profileRules(node); len(base) > 0 && len(explicit) >= len(base) &&
			samePolicy(policy.Config{Rules: explicit[len(explicit)-len(base):]}, policy.Config{Rules: base}) {
			explicit = explicit[:len(explicit)-len(base)]
		}
		if rules, err := yamlNodeFrom(configuredPoliciesFrom(policy.Config{Rules: explicit})); err == nil {
			if value := mappingValue(rules, "rules"); value != nil {
				setNodeField(node, "rules", value)
			}
		}
	}
}

// workspaceRootText is the text a save writes for root: "." when root is the
// configuration's directory, else root as the editor gave it. That directory
// is the root the workspace guardrail brings when the file names none, and a
// file that leaves it implicit still has to name it once the guardrail is off
// and the editor keeps the root it shows, as it does when switching from
// strict to permissive. It was written as an absolute path, naming the user's
// home directory and pinned to where the file was; "." keeps following the
// file, as the implicit root did.
func workspaceRootText(root, configDir string) string {
	if strings.TrimSpace(root) == "" || strings.TrimSpace(configDir) == "" {
		return root
	}
	if absolute, err := filepath.Abs(configDir); err == nil {
		configDir = absolute
	}
	if filepath.Clean(root) == filepath.Clean(configDir) {
		return "."
	}
	return root
}

// profileRules are the rules of the profile node names, which the loader
// appends to the file's own.
func profileRules(node *yaml.Node) []policy.Rule {
	value := mappingValue(node, "profile")
	if value == nil || value.Kind != yaml.ScalarNode {
		return nil
	}
	profile, err := policy.ParseProfile(strings.TrimSpace(value.Value))
	if err != nil {
		return nil
	}
	return policy.ProfileConfig(profile, "").Rules
}

func nonEmpty(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	return values
}

// setTypedField writes a scalar of the given tag under key, keeping the
// comments of the value it replaces and, for a string, its quoting.
func setTypedField(mapping *yaml.Node, key, tag, value string) {
	previous := mappingValue(mapping, key)
	node := &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value}
	if tag == "!!str" {
		node = scalarLike(previous, value)
	} else if previous != nil {
		node.HeadComment = previous.HeadComment
		node.LineComment = previous.LineComment
		node.FootComment = previous.FootComment
	}
	setNodeField(mapping, key, node)
}

// copyNode copies node and everything under it, so a candidate edit leaves
// the document as it was. An alias keeps pointing at its anchor.
func copyNode(node *yaml.Node) *yaml.Node {
	if node == nil {
		return nil
	}
	copied := *node
	if node.Kind == yaml.AliasNode {
		return &copied
	}
	copied.Content = make([]*yaml.Node, len(node.Content))
	for index, child := range node.Content {
		copied.Content[index] = copyNode(child)
	}
	return &copied
}
