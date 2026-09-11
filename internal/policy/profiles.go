package policy

import (
	"fmt"
	"strings"

	"github.com/Hocsman/Relayer/internal/adapters"
)

// Profile identifies a predefined security and automation stance.
type Profile string

const (
	ProfileStrict            Profile = "strict"
	ProfileDeveloperFriendly Profile = "developer-friendly"
	ProfilePermissive        Profile = "permissive"
	ProfileCustom            Profile = "custom"
)

// ParseProfile parses a profile name string into a validated Profile constant.
// It accepts common aliases (e.g. dev-friendly, developer).
func ParseProfile(name string) (Profile, error) {
	norm := strings.ToLower(strings.TrimSpace(name))
	switch norm {
	case "strict":
		return ProfileStrict, nil
	case "developer-friendly", "developer_friendly", "dev-friendly", "dev_friendly", "developer", "dev":
		return ProfileDeveloperFriendly, nil
	case "permissive":
		return ProfilePermissive, nil
	case "custom", "":
		return ProfileCustom, nil
	default:
		return "", fmt.Errorf("unknown policy profile %q (valid: strict, developer-friendly, permissive, custom)", name)
	}
}

func boolPtr(b bool) *bool {
	return &b
}

// ProfileConfig generates the base policy configuration for the chosen profile.
// The caller may override or append individual settings afterwards.
func ProfileConfig(profile Profile, workspaceRoot string) Config {
	switch profile {
	case ProfileStrict:
		return Config{
			DefaultAction:               ActionAsk,
			MaxConsecutiveAutoDecisions: 1,
			RateLimitPerMinute:          10,
			Guardrails: GuardrailsConfig{
				BlockDestructive:      true,
				BlockExfiltration:     true,
				BlockSensitivePaths:   true,
				BlockOutsideWorkspace: workspaceRoot != "",
				WorkspaceRoot:         workspaceRoot,
			},
			Rules: nil,
		}
	case ProfileDeveloperFriendly:
		return Config{
			DefaultAction:               ActionAsk,
			MaxConsecutiveAutoDecisions: 10,
			RateLimitPerMinute:          30,
			Guardrails: GuardrailsConfig{
				BlockDestructive:      true,
				BlockExfiltration:     true,
				BlockSensitivePaths:   true,
				BlockOutsideWorkspace: workspaceRoot != "",
				WorkspaceRoot:         workspaceRoot,
			},
			Rules: []Rule{
				{
					Name: "dev-friendly-git-readonly",
					Match: Match{
						RiskLevels:   []adapters.RiskLevel{adapters.RiskLow},
						CommandRegex: `(?i)^git\s+(status|diff|log|show|branch|tag|rev-parse|ls-files|describe|remote|stash|check-ignore)\b`,
						ReadOnly:     boolPtr(true),
					},
					Action: ActionAllow,
				},
				{
					Name: "dev-friendly-inspect-readonly",
					Match: Match{
						RiskLevels:   []adapters.RiskLevel{adapters.RiskLow},
						CommandRegex: `(?i)^(ls|dir|cat|head|tail|more|less|pwd|echo|which|where|type|file|stat|wc|findstr|grep|rg)\b`,
						ReadOnly:     boolPtr(true),
					},
					Action: ActionAllow,
				},
				{
					Name: "dev-friendly-test-readonly",
					Match: Match{
						RiskLevels:   []adapters.RiskLevel{adapters.RiskLow},
						CommandRegex: `(?i)^(go\s+(test|vet|doc)|(npm|pnpm|yarn)\s+(test|run\s+test|run\s+lint|lint)|pytest|cargo\s+(test|check)|python\s+-m\s+(unittest|pytest))\b`,
						ReadOnly:     boolPtr(true),
					},
					Action: ActionAllow,
				},
			},
		}
	case ProfilePermissive:
		return Config{
			DefaultAction:               ActionAllow,
			MaxConsecutiveAutoDecisions: 50,
			RateLimitPerMinute:          120,
			Guardrails: GuardrailsConfig{
				BlockDestructive:      true,
				BlockExfiltration:     true,
				BlockSensitivePaths:   true,
				BlockOutsideWorkspace: false,
			},
			Rules: nil,
		}
	default:
		return DefaultConfig()
	}
}
