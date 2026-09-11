package policy

import (
	"testing"
)

func TestParseProfile(t *testing.T) {
	tests := []struct {
		input   string
		want    Profile
		wantErr bool
	}{
		{"strict", ProfileStrict, false},
		{"STRICT", ProfileStrict, false},
		{"developer-friendly", ProfileDeveloperFriendly, false},
		{"dev-friendly", ProfileDeveloperFriendly, false},
		{"developer", ProfileDeveloperFriendly, false},
		{"dev", ProfileDeveloperFriendly, false},
		{"permissive", ProfilePermissive, false},
		{"custom", ProfileCustom, false},
		{"", ProfileCustom, false},
		{"invalid-profile", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseProfile(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseProfile(%q) err = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("ParseProfile(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestProfileConfig(t *testing.T) {
	ws := "/path/to/workspace"

	// 1. Strict profile
	strictCfg := ProfileConfig(ProfileStrict, ws)
	if strictCfg.DefaultAction != ActionAsk {
		t.Errorf("strict DefaultAction = %v, want %v", strictCfg.DefaultAction, ActionAsk)
	}
	if strictCfg.MaxConsecutiveAutoDecisions != 1 || strictCfg.RateLimitPerMinute != 10 {
		t.Errorf("strict limits mismatch: consecutive=%d, rate=%d",
			strictCfg.MaxConsecutiveAutoDecisions, strictCfg.RateLimitPerMinute)
	}
	if !strictCfg.Guardrails.BlockDestructive || !strictCfg.Guardrails.BlockExfiltration ||
		!strictCfg.Guardrails.BlockSensitivePaths || !strictCfg.Guardrails.BlockOutsideWorkspace {
		t.Errorf("strict guardrails not fully enabled: %#v", strictCfg.Guardrails)
	}
	if len(strictCfg.Rules) != 0 {
		t.Errorf("strict rules count = %d, want 0", len(strictCfg.Rules))
	}

	// 2. Developer-Friendly profile
	devCfg := ProfileConfig(ProfileDeveloperFriendly, ws)
	if devCfg.DefaultAction != ActionAsk {
		t.Errorf("dev DefaultAction = %v, want %v", devCfg.DefaultAction, ActionAsk)
	}
	if devCfg.MaxConsecutiveAutoDecisions != 10 || devCfg.RateLimitPerMinute != 30 {
		t.Errorf("dev limits mismatch: consecutive=%d, rate=%d",
			devCfg.MaxConsecutiveAutoDecisions, devCfg.RateLimitPerMinute)
	}
	if !devCfg.Guardrails.BlockDestructive || !devCfg.Guardrails.BlockExfiltration ||
		!devCfg.Guardrails.BlockSensitivePaths || !devCfg.Guardrails.BlockOutsideWorkspace {
		t.Errorf("dev guardrails not fully enabled: %#v", devCfg.Guardrails)
	}
	if len(devCfg.Rules) != 3 {
		t.Errorf("dev rules count = %d, want 3", len(devCfg.Rules))
	}

	// 3. Permissive profile
	permCfg := ProfileConfig(ProfilePermissive, ws)
	if permCfg.DefaultAction != ActionAllow {
		t.Errorf("permissive DefaultAction = %v, want %v", permCfg.DefaultAction, ActionAllow)
	}
	if permCfg.MaxConsecutiveAutoDecisions != 50 || permCfg.RateLimitPerMinute != 120 {
		t.Errorf("permissive limits mismatch: consecutive=%d, rate=%d",
			permCfg.MaxConsecutiveAutoDecisions, permCfg.RateLimitPerMinute)
	}
	if !permCfg.Guardrails.BlockDestructive || !permCfg.Guardrails.BlockExfiltration ||
		!permCfg.Guardrails.BlockSensitivePaths || permCfg.Guardrails.BlockOutsideWorkspace {
		t.Errorf("permissive guardrails mismatch: %#v", permCfg.Guardrails)
	}

	// 4. Custom profile
	customCfg := ProfileConfig(ProfileCustom, ws)
	if customCfg.DefaultAction != ActionAsk || len(customCfg.Rules) != 0 {
		t.Errorf("custom profile mismatch: %#v", customCfg)
	}
}
