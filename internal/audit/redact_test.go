package audit

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/adapters"
)

func TestRedactCredentialMatrixIsIdempotent(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		secrets   []string
		preserves []string
	}{
		{
			name:      "bearer and basic",
			input:     "Authorization: Bearer abc.DEF-123_secret Basic dXNlcjpwYXNzd29yZA==",
			secrets:   []string{"abc.DEF-123_secret", "dXNlcjpwYXNzd29yZA=="},
			preserves: []string{"Authorization", redactedValue},
		},
		{
			name:      "jwt",
			input:     "jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.signature12345 compact eyJhbGciOiJIUzI1NiJ9.e30.dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk end",
			secrets:   []string{"eyJhbGciOiJIUzI1NiJ9", "eyJzdWIiOiIxMjM0NTY3ODkwIn0", "signature12345", "e30", "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"},
			preserves: []string{"jwt", "end"},
		},
		{
			name:      "known token prefixes",
			input:     "openai=sk-abcdefghijk github=ghp_abcdefghijklmnopqrstuvwxyz",
			secrets:   []string{"sk-abcdefghijk", "ghp_abcdefghijklmnopqrstuvwxyz"},
			preserves: []string{"openai", "github"},
		},
		{
			name:      "URL user and query and fragment",
			input:     "visit https://alice:pass123@example.test/path?ok=yes&access_token=url-secret&API_KEY=key-secret#fragment-secret and postgres://dbuser:dbpass@db.example.test/app",
			secrets:   []string{"alice", "pass123", "url-secret", "key-secret", "fragment-secret", "dbuser", "dbpass"},
			preserves: []string{"example.test", "ok=yes", "db.example.test"},
		},
		{
			name:      "environment assignments",
			input:     `MODEL=llama API_KEY="quoted secret" OTP: 123456 PIN=9876 PASSPHRASE='two words' PRIVATE_KEY=private-secret BEARER=bearer-secret CREDENTIAL=credential-secret`,
			secrets:   []string{"quoted secret", "123456", "9876", "two words", "private-secret", "bearer-secret", "credential-secret"},
			preserves: []string{"MODEL=llama", "API_KEY"},
		},
		{
			name:      "quoted JSON keys",
			input:     `{"password":"hunter2","api_key":"key-secret","safe":"visible"}`,
			secrets:   []string{"hunter2", "key-secret"},
			preserves: []string{`"password":"[REDACTED]"`, `"api_key":"[REDACTED]"`, `"safe":"visible"`},
		},
		{
			name:      "linked credential phrases",
			input:     `password is hunter2 token value token-secret otp required: 123456`,
			secrets:   []string{"hunter2", "token-secret", "123456"},
			preserves: []string{"password is"},
		},
		{
			name:      "space separated credentials",
			input:     "password hunter2 token abcdefghijk otp 123456",
			secrets:   []string{"hunter2", "abcdefghijk", "123456"},
			preserves: []string{"password"},
		},
		{
			name:      "multi word and unknown connector",
			input:     "passphrase: correct horse battery staple\npassword was hunter2\npassword is required: second-secret\npassword value required: third-secret\nPIN code is 1234\nOTP code is 123456",
			secrets:   []string{"correct", "horse", "battery", "staple", "hunter2", "second-secret", "third-secret", "1234", "123456"},
			preserves: []string{"passphrase", "password"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			redacted := Redact(test.input)
			for _, secret := range test.secrets {
				if strings.Contains(redacted, secret) {
					t.Fatalf("secret %q survived: %q", secret, redacted)
				}
			}
			for _, safe := range test.preserves {
				if !strings.Contains(redacted, safe) {
					t.Fatalf("safe marker %q missing: %q", safe, redacted)
				}
			}
			if twice := Redact(redacted); twice != redacted {
				t.Fatalf("Redact is not idempotent:\nfirst  %q\nsecond %q", redacted, twice)
			}
		})
	}

	benign := "MODEL=llama url=https://example.test/path?region=eu message=all-good\npassword required\ntoken missing\notp code"
	if got := Redact(benign); got != benign {
		t.Fatalf("benign text changed: %q", got)
	}
}

func TestSanitizeEntryModesSensitiveManualAndPolicyFailure(t *testing.T) {
	base := Entry{
		Kind:       KindPolicyEvaluated,
		SessionID:  "session-a",
		AgentID:    "agent-a",
		Backend:    "pty",
		Adapter:    "generic",
		EventID:    "evt-a",
		EventType:  adapters.EventConfirmation,
		Risk:       adapters.RiskLow,
		Rule:       "allow-safe",
		Decision:   DecisionAllow,
		DecisionBy: DecisionByPolicy,
		Outcome:    OutcomeApplied,
		Reason:     "rule_match",
		Summary:    "safe summary with API_KEY=summary-secret",
		Metadata: map[string]string{
			"automatic":        "true",
			"effective_action": "allow",
			"mode":             "enforce",
			"proposed_action":  "allow",
			"manualInput":      "manual-secret",
			"raw-error":        "raw-error-secret",
			"stdout":           "terminal-output-secret",
			"terminal_input":   "terminal-input-secret",
			"API_TOKEN":        "token-secret",
		},
	}
	metadata := SanitizeEntry(base, ModeMetadata)
	if metadata.Summary != "" || metadata.Metadata != nil || metadata.Kind != base.Kind || metadata.EventID != base.EventID {
		t.Fatalf("metadata mode = %#v", metadata)
	}

	detailed := SanitizeEntry(base, ModeDetailed)
	visible := fmt.Sprintf("%#v", detailed)
	for _, secret := range []string{"summary-secret", "manual-secret", "raw-error-secret", "terminal-output-secret", "terminal-input-secret", "token-secret"} {
		if strings.Contains(visible, secret) {
			t.Fatalf("secret %q survived detailed entry: %s", secret, visible)
		}
	}
	if detailed.Metadata["automatic"] != "true" || detailed.Metadata["mode"] != "enforce" ||
		len(detailed.Metadata) != 4 || detailed.Summary == "" {
		t.Fatalf("detailed mode = %#v", detailed)
	}
	if !reflect.DeepEqual(base.Metadata, map[string]string{
		"automatic": "true", "effective_action": "allow", "mode": "enforce", "proposed_action": "allow",
		"manualInput": "manual-secret", "raw-error": "raw-error-secret", "stdout": "terminal-output-secret",
		"terminal_input": "terminal-input-secret", "API_TOKEN": "token-secret",
	}) {
		t.Fatalf("SanitizeEntry mutated input: %#v", base.Metadata)
	}

	sensitiveKeys := base
	sensitiveKeys.Metadata = map[string]string{
		"OTP":         "otp-plain-value",
		"PIN":         "pin-plain-value",
		"PASSPHRASE":  "passphrase-plain-value",
		"BEARER":      "bearer-plain-value",
		"CREDENTIAL":  "credential-plain-value",
		"PRIVATE_KEY": "private-key-plain-value",
		"API_KEY":     "api-key-plain-value",
		"safe":        "retained",
	}
	gotSensitiveKeys := SanitizeEntry(sensitiveKeys, ModeDetailed)
	if gotSensitiveKeys.Metadata != nil {
		t.Fatalf("sensitive metadata keys survived: %#v", gotSensitiveKeys.Metadata)
	}

	for _, mutate := range []func(*Entry){
		func(entry *Entry) { entry.Sensitive = true },
		func(entry *Entry) { entry.EventType = adapters.EventCredential },
		func(entry *Entry) { entry.Risk = adapters.RiskHigh },
	} {
		entry := base
		entry.Metadata = map[string]string{"private": "sensitive-marker"}
		mutate(&entry)
		got := SanitizeEntry(entry, ModeDetailed)
		if !got.Sensitive || got.EventID != "" || got.Summary != "sensitive_event" || got.Metadata != nil || strings.Contains(fmt.Sprintf("%#v", got), "sensitive-marker") {
			t.Fatalf("sensitive entry = %#v", got)
		}
	}

	human := base
	human.Kind = KindDecision
	human.Decision = DecisionAsk
	human.DecisionBy = DecisionByHuman
	human.Summary = "plain-manual-secret"
	human.Metadata = map[string]string{"note": "plain-manual-secret"}
	gotHuman := SanitizeEntry(human, ModeDetailed)
	if gotHuman.Summary != "" || gotHuman.Metadata != nil || strings.Contains(fmt.Sprintf("%#v", gotHuman), "plain-manual-secret") {
		t.Fatalf("manual input capable fields survived: %#v", gotHuman)
	}

	failure := base
	failure.Kind = KindBackendError
	failure.Outcome = OutcomeFallbackDeliveryUncertain
	failure.Reason = "disk failed: plain-secret-error"
	failure.Summary = "plain-secret-error"
	failure.Metadata = map[string]string{"error": "plain-secret-error"}
	gotFailure := SanitizeEntry(failure, ModeDetailed)
	if gotFailure.Reason != "unknown" || gotFailure.Summary != "backend_error" || gotFailure.Metadata != nil ||
		strings.Contains(fmt.Sprintf("%#v", gotFailure), "plain-secret-error") {
		t.Fatalf("policy/backend failure leaked raw error: %#v", gotFailure)
	}

	for _, summary := range []string{
		"{\"password\":\n\"hunter2\"}",
		"password:\nhunter2",
	} {
		newlineEntry := base
		newlineEntry.Summary = summary
		got := SanitizeEntry(newlineEntry, ModeDetailed)
		if strings.Contains(got.Summary, "hunter2") {
			t.Fatalf("newline-separated credential survived: %q", got.Summary)
		}
	}
}

func TestSanitizeEntryOperatorInputNeverRetainsContent(t *testing.T) {
	const secret = "operator-input-fixture-secret"
	entry := Entry{
		Kind:       KindOperatorInput,
		SessionID:  "agent-a",
		DecisionBy: DecisionByHuman,
		Outcome:    OutcomeInFlight,
		Reason:     "operator_input_started",
		Summary:    secret,
		Metadata: map[string]string{
			"input":  secret,
			"length": "29",
		},
	}

	for _, mode := range []Mode{ModeMetadata, ModeDetailed} {
		got := SanitizeEntry(entry, mode)
		if got.Kind != KindOperatorInput || got.SessionID != "agent-a" ||
			got.DecisionBy != DecisionByHuman || got.Outcome != OutcomeInFlight ||
			got.Reason != "operator_input_started" {
			t.Fatalf("SanitizeEntry(%q) metadata = %#v", mode, got)
		}
		if got.Summary != "" || got.Metadata != nil {
			t.Fatalf("SanitizeEntry(%q) retained operator content: %#v", mode, got)
		}
	}
}

func TestSanitizeEntryOperatorInputShapeIsClosedForEveryActor(t *testing.T) {
	const secret = "operator-input-shape-secret"
	for _, actor := range []DecisionBy{DecisionByHuman, DecisionBySystem, DecisionByPolicy} {
		got := SanitizeEntry(Entry{
			Kind:       KindOperatorInput,
			EventID:    secret,
			EventType:  adapters.EventCredential,
			Risk:       adapters.RiskHigh,
			Rule:       secret,
			Decision:   DecisionAllow,
			DecisionBy: actor,
			Outcome:    OutcomeInFlight,
			Reason:     secret,
			Summary:    secret,
			Sensitive:  true,
			Metadata:   map[string]string{"input": secret},
		}, ModeDetailed)
		if got.EventID != "" || got.EventType != "" || got.Risk != "" || got.Rule != "" ||
			got.Decision != "" || got.Reason != "unknown" || got.Summary != "" || got.Metadata != nil || got.Sensitive {
			t.Fatalf("actor %q retained operator content: %#v", actor, got)
		}
	}
}

func TestSanitizeEntryBoundsMetadataDeterministicallyAndWhitelistsEnums(t *testing.T) {
	metadata := map[string]string{
		"automatic":        strings.Repeat("é", maxMetadataValueRunes+50),
		"effective_action": "allow",
		"mode":             "enforce",
		"proposed_action":  "allow",
		"arbitrary":        "must-be-omitted",
	}
	entry := Entry{
		Kind:       KindPolicyEvaluated,
		EventType:  adapters.EventType("tool-call-secret"),
		Risk:       adapters.RiskLevel("critical-secret"),
		Decision:   Decision("approve-secret"),
		DecisionBy: DecisionBy("actor-secret"),
		Outcome:    Outcome("outcome-secret"),
		Reason:     "reason with raw secret",
		Summary:    strings.Repeat("界", maxSummaryRunes+50),
		Metadata:   metadata,
	}
	got := SanitizeEntry(entry, ModeDetailed)
	if got.Kind != KindPolicyEvaluated || got.EventType != "unknown" || got.Risk != adapters.RiskUnknown ||
		got.Decision != DecisionUnknown || got.DecisionBy != DecisionByUnknown || got.Outcome != OutcomeUnknown || got.Reason != "unknown" {
		t.Fatalf("unsafe enums survived: %#v", got)
	}
	if len([]rune(got.Summary)) != maxSummaryRunes || len(got.Metadata) != 4 {
		t.Fatalf("bounds = summary %d metadata %d", len([]rune(got.Summary)), len(got.Metadata))
	}
	for key, value := range got.Metadata {
		if len([]rune(key)) > maxMetadataKeyRunes || len([]rune(value)) > maxMetadataValueRunes {
			t.Fatalf("unbounded metadata %q=%q", key, value)
		}
	}
	again := SanitizeEntry(entry, ModeDetailed)
	if !reflect.DeepEqual(got, again) {
		t.Fatalf("bounded metadata selection is nondeterministic")
	}
}

func TestSanitizeEntryOffIsEmpty(t *testing.T) {
	if got := SanitizeEntry(Entry{Kind: KindRunStarted, Summary: "secret"}, ModeOff); !reflect.DeepEqual(got, Entry{}) {
		t.Fatalf("off entry = %#v", got)
	}
}

func TestSanitizeEntryPreservesRecordingAndControlKinds(t *testing.T) {
	for _, kind := range []Kind{
		KindRecordingStarted, KindRecordingFinished, KindRecordingExported, KindRecordingDeleted,
		KindControlRequested, KindControlGranted, KindControlDeclined, KindControlReleased, KindControlForced,
	} {
		for _, mode := range []Mode{ModeMetadata, ModeDetailed} {
			got := SanitizeEntry(Entry{Kind: kind, Outcome: OutcomeStarted, Operator: "alice"}, mode)
			if got.Kind != kind {
				t.Fatalf("SanitizeEntry(%q, %q) kind = %q", kind, mode, got.Kind)
			}
			if got.Operator != "alice" {
				t.Fatalf("SanitizeEntry(%q, %q) operator = %q", kind, mode, got.Operator)
			}
		}
	}
}

func TestSanitizeEntryRecordingAndControlMetadataAllowlist(t *testing.T) {
	const secret = "recording-fixture-secret"
	for _, test := range []struct {
		name     string
		kinds    []Kind
		metadata map[string]string
		want     map[string]string
	}{
		{
			name: "recording",
			kinds: []Kind{
				KindRecordingStarted, KindRecordingFinished,
				KindRecordingExported, KindRecordingDeleted,
			},
			metadata: map[string]string{
				"operator": "alice", "role": "operator", "recording_id": "rec-1",
				"frames": "128", "bytes": "4096", "truncated": "false",
				"redacted": "true", "input_recorded": "false",
				// Keys belonging to another branch or to no branch at all.
				"conn_id": "conn-1", "stdout": secret, "terminal_input": secret, "API_KEY": secret,
			},
			want: map[string]string{
				"operator": "alice", "role": "operator", "recording_id": "rec-1",
				"frames": "128", "bytes": "4096", "truncated": "false",
				"redacted": "true", "input_recorded": "false",
			},
		},
		{
			name: "control",
			kinds: []Kind{
				KindControlRequested, KindControlGranted, KindControlDeclined,
				KindControlReleased, KindControlForced,
			},
			metadata: map[string]string{
				"operator": "alice", "role": "operator", "conn_id": "conn-1",
				"target_operator": "bob", "target_conn_id": "conn-2",
				// "reason" duplicates a field safeCode bounds to a code, so it
				// is dropped rather than admitted as free-form text.
				"reason": "operator typed " + secret,
				"frames": "128", "input": secret, "API_KEY": secret,
			},
			want: map[string]string{
				"operator": "alice", "role": "operator", "conn_id": "conn-1",
				"target_operator": "bob", "target_conn_id": "conn-2",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, kind := range test.kinds {
				entry := Entry{Kind: kind, Metadata: test.metadata}
				if metadata := SanitizeEntry(entry, ModeMetadata); metadata.Metadata != nil {
					t.Fatalf("kind %q retained metadata in metadata mode: %#v", kind, metadata.Metadata)
				}
				got := SanitizeEntry(entry, ModeDetailed)
				if !reflect.DeepEqual(got.Metadata, test.want) {
					t.Fatalf("kind %q metadata = %#v, want %#v", kind, got.Metadata, test.want)
				}
				if strings.Contains(fmt.Sprintf("%#v", got), secret) {
					t.Fatalf("kind %q leaked a dropped value: %#v", kind, got)
				}
			}
		})
	}
}

func TestSanitizeEntryRedactsRecordingAndControlMetadataValues(t *testing.T) {
	const secret = "control-metadata-fixture-secret"
	for _, kind := range []Kind{KindRecordingStarted, KindControlGranted} {
		got := SanitizeEntry(Entry{
			Kind: kind,
			Metadata: map[string]string{
				"operator": "alice api_key=" + secret,
				"role":     strings.Repeat("é", maxMetadataValueRunes+50),
			},
		}, ModeDetailed)
		if strings.Contains(fmt.Sprintf("%#v", got), secret) {
			t.Fatalf("kind %q kept a credential in metadata: %#v", kind, got.Metadata)
		}
		if len([]rune(got.Metadata["role"])) != maxMetadataValueRunes {
			t.Fatalf("kind %q metadata value is unbounded: %d runes", kind, len([]rune(got.Metadata["role"])))
		}
	}
}

func TestSanitizeEntryOperatorFieldAndMetadata(t *testing.T) {
	entry := Entry{
		Kind:       KindDecision,
		DecisionBy: DecisionByHuman,
		Decision:   DecisionAllow,
		Operator:   "alice-operator",
		Metadata: map[string]string{
			"operator": "alice-operator",
			"role":     "operator",
			"secret":   "leaked-value",
		},
	}

	// ModeMetadata preserves Operator field, strips Metadata
	meta := SanitizeEntry(entry, ModeMetadata)
	if meta.Operator != "alice-operator" {
		t.Fatalf("expected operator alice-operator, got %q", meta.Operator)
	}
	if meta.Metadata != nil {
		t.Fatalf("expected nil metadata in ModeMetadata, got %#v", meta.Metadata)
	}
	if meta.Summary != "" {
		t.Fatalf("expected empty summary for human decision, got %q", meta.Summary)
	}

	// ModeDetailed preserves Operator field and whitelisted metadata
	detailed := SanitizeEntry(entry, ModeDetailed)
	if detailed.Operator != "alice-operator" {
		t.Fatalf("expected operator alice-operator, got %q", detailed.Operator)
	}
	if detailed.Metadata["operator"] != "alice-operator" || detailed.Metadata["role"] != "operator" {
		t.Fatalf("expected operator and role in metadata, got %#v", detailed.Metadata)
	}
	if _, found := detailed.Metadata["secret"]; found {
		t.Fatalf("unwhitelisted metadata key survived: %#v", detailed.Metadata)
	}
	if detailed.Summary != "" {
		t.Fatalf("expected empty summary for human decision, got %q", detailed.Summary)
	}

	// Operator input kind also preserves Operator
	opInput := SanitizeEntry(Entry{
		Kind:     KindOperatorInput,
		Operator: "bob",
		Reason:   "operator_input_applied",
	}, ModeMetadata)
	if opInput.Operator != "bob" {
		t.Fatalf("expected operator bob, got %q", opInput.Operator)
	}
}
