package audit

import (
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/agent"
)

const redactedValue = "[REDACTED]"

const (
	maxSummaryRunes       = 512
	maxMetadataEntries    = 32
	maxMetadataKeyRunes   = 64
	maxMetadataValueRunes = 512
)

var (
	urlPattern              = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s<>"']+`)
	bearerPattern           = regexp.MustCompile(`(?i)\b(bearer[ \t]+)[A-Za-z0-9._~+/=-]+`)
	basicPattern            = regexp.MustCompile(`(?i)\b(basic[ \t]+)[A-Za-z0-9+/=]+`)
	jwtPattern              = regexp.MustCompile(`\b[A-Za-z0-9_-]{2,}\.[A-Za-z0-9_-]*\.[A-Za-z0-9_-]{8,}\b`)
	prefixedTokenPattern    = regexp.MustCompile(`(?i)\b(sk-[A-Za-z0-9_-]{8,}|gh[pousr]_[A-Za-z0-9_]{8,})\b`)
	jsonSecretPattern       = regexp.MustCompile(`(?i)("(?:password|passwd|passphrase|secret|token|api[_-]?key|private[_-]?key|credential|authorization|bearer|otp|pin)"[ \t]*:[ \t]*)("(?:\\.|[^"\\\r\n])*"|[^,}\s]+)`)
	assignmentPattern       = regexp.MustCompile(`(?i)\b([A-Za-z_][A-Za-z0-9_.-]*)([ \t]*(=|:)[ \t]*)("[^"\r\n]*"|'[^'\r\n]*'|[^\s,;&]+)`)
	credentialPhrasePattern = regexp.MustCompile(`(?i)\b(password|passwd|passphrase|secret|token|api[_-]?key|private[_-]?key|credential|authorization|bearer|otp|pin)([ \t]+(?:is|was|value|required|code)(?:[ \t]+(?:is|was|value|required|needed|requested|code))*[ \t]*:?[ \t]+)([^\r\n]+)`)
	linkedSecretPattern     = regexp.MustCompile(`(?i)\b(password|passwd|passphrase|secret|token|api[_-]?key|private[_-]?key|credential|authorization|bearer|otp|pin)([ \t]+(?:is|value(?:[ \t]+is)?|required)[ \t]*:?\s+)("[^"\r\n]*"|'[^'\r\n]*'|[^\s,;&]+)`)
	spacedSecretPattern     = regexp.MustCompile(`(?i)\b(password|passwd|passphrase|secret|token|api[_-]?key|private[_-]?key|credential|bearer|otp|pin)([ \t]+)("[^"\r\n]*"|'[^'\r\n]*'|[^\s,;&]+)`)
	redactedTailPattern     = regexp.MustCompile(`(?i)\b(password|passwd|passphrase|secret|token|api[_-]?key|private[_-]?key|credential|authorization|bearer|otp|pin)([ \t]*(?:=|:)[ \t]*|[ \t]+(?:is|was|value(?:[ \t]+is)?|required)?[ \t]*:?[ \t]*)\[REDACTED\][^\r\n]*`)
	safeCodePattern         = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
)

// Redact removes common credential forms from arbitrary text. It is
// intentionally conservative and idempotent.
func Redact(value string) string {
	value = urlPattern.ReplaceAllStringFunc(value, redactURL)
	value = bearerPattern.ReplaceAllString(value, `${1}`+redactedValue)
	value = basicPattern.ReplaceAllString(value, `${1}`+redactedValue)
	value = jwtPattern.ReplaceAllString(value, redactedValue)
	value = prefixedTokenPattern.ReplaceAllString(value, redactedValue)
	value = jsonSecretPattern.ReplaceAllString(value, `${1}"`+redactedValue+`"`)
	value = assignmentPattern.ReplaceAllStringFunc(value, redactAssignment)
	value = credentialPhrasePattern.ReplaceAllStringFunc(value, redactCredentialPhrase)
	value = linkedSecretPattern.ReplaceAllStringFunc(value, redactLinkedSecret)
	value = spacedSecretPattern.ReplaceAllStringFunc(value, redactSpacedSecret)
	value = redactedTailPattern.ReplaceAllStringFunc(value, collapseRedactedTail)
	return value
}

// SanitizeEntry returns a deep, redacted copy suitable for the selected mode.
func SanitizeEntry(entry Entry, mode Mode) Entry {
	if mode == ModeOff {
		return Entry{}
	}
	result := entry
	result.EntryID = sanitizeText(entry.EntryID)
	result.RunID = sanitizeText(entry.RunID)
	result.Kind = safeKind(entry.Kind)
	result.SessionID = sanitizeText(entry.SessionID)
	result.AgentID = sanitizeText(entry.AgentID)
	result.Backend = sanitizeText(entry.Backend)
	result.Adapter = sanitizeText(entry.Adapter)
	result.EventID = sanitizeText(entry.EventID)
	result.EventType = safeEventType(entry.EventType)
	result.Risk = safeRisk(entry.Risk)
	result.Rule = sanitizeText(entry.Rule)
	result.Decision = safeDecision(entry.Decision)
	result.DecisionBy = safeDecisionBy(entry.DecisionBy)
	result.Outcome = safeOutcome(entry.Outcome)
	result.Reason = safeCode(entry.Reason)
	result.Sensitive = entry.Sensitive || entry.EventType == adapters.EventCredential || entry.Risk == adapters.RiskHigh
	if result.Sensitive {
		// Generic adapter occurrence IDs are derived from a fingerprint which
		// includes the normalized match. Omitting the ID prevents offline
		// guessing of low-entropy passwords or OTPs from the audit file.
		result.EventID = ""
	}
	result.Summary = ""
	result.Metadata = nil

	if mode != ModeDetailed {
		return result
	}
	if result.Sensitive {
		result.Summary = "sensitive_event"
		return result
	}
	if result.Kind == KindBackendError {
		result.Summary = "backend_error"
		return result
	}
	if result.DecisionBy == DecisionByHuman {
		// Human decision records deliberately carry no free-form field that a
		// caller could accidentally populate with terminal input.
		return result
	}
	result.Summary = truncateRunes(sanitizeText(entry.Summary), maxSummaryRunes)
	if entry.Metadata != nil {
		keys := make([]string, 0, len(entry.Metadata))
		for key := range entry.Metadata {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		result.Metadata = make(map[string]string, minInt(len(keys), maxMetadataEntries))
		for _, key := range keys {
			if len(result.Metadata) >= maxMetadataEntries {
				break
			}
			if !allowedMetadataKey(result.Kind, key) {
				continue
			}
			cleanKey := truncateRunes(strings.ToLower(strings.TrimSpace(key)), maxMetadataKeyRunes)
			if cleanKey == "" {
				continue
			}
			result.Metadata[cleanKey] = truncateRunes(sanitizeText(entry.Metadata[key]), maxMetadataValueRunes)
		}
		if len(result.Metadata) == 0 {
			result.Metadata = nil
		}
	}
	return result
}

func sanitizeText(value string) string {
	value = strings.Map(func(character rune) rune {
		if unicode.IsControl(character) {
			return ' '
		}
		return character
	}, value)
	// Normalize first, then redact. Pretty-printed JSON and terminal text may
	// split a credential label from its value with a newline or carriage
	// return; redacting first would miss that value and flatten it afterwards.
	value = strings.Join(strings.Fields(value), " ")
	return Redact(value)
}

func allowedMetadataKey(kind Kind, value string) bool {
	compact := strings.ToLower(strings.TrimSpace(value))
	switch kind {
	case KindPolicyEvaluated:
		switch compact {
		case "automatic", "effective_action", "mode", "proposed_action":
			return true
		}
	case KindEventDetected:
		switch compact {
		case "exit_code", "failed":
			return true
		}
	}
	return false
}

func redactAssignment(value string) string {
	parts := assignmentPattern.FindStringSubmatch(value)
	if len(parts) < 3 || !sensitiveQueryKey(parts[1]) {
		return value
	}
	return parts[1] + parts[2] + redactedValue
}

func redactCredentialPhrase(value string) string {
	parts := credentialPhrasePattern.FindStringSubmatch(value)
	if len(parts) < 4 || !credentialValueLooksSecret(parts[3]) {
		return value
	}
	return parts[1] + parts[2] + redactedValue
}

func redactSpacedSecret(value string) string {
	parts := spacedSecretPattern.FindStringSubmatch(value)
	if len(parts) < 4 || !credentialValueLooksSecret(parts[3]) {
		return value
	}
	return parts[1] + parts[2] + redactedValue
}

func redactLinkedSecret(value string) string {
	parts := linkedSecretPattern.FindStringSubmatch(value)
	if len(parts) < 4 || !credentialValueLooksSecret(parts[3]) {
		return value
	}
	return parts[1] + parts[2] + redactedValue
}

func collapseRedactedTail(value string) string {
	index := strings.Index(value, redactedValue)
	if index < 0 {
		return value
	}
	return value[:index+len(redactedValue)]
}

func credentialValueLooksSecret(value string) bool {
	value = strings.Trim(strings.TrimSpace(value), `"'`)
	if value == "" {
		return false
	}
	if value == redactedValue {
		return true
	}
	switch strings.ToLower(strings.TrimRight(value, ".:!?")) {
	case "is", "value", "required", "requested", "needed", "missing", "unset", "empty",
		"prompt", "prompted", "accepted", "rejected", "invalid", "authentication",
		"field", "policy", "manager", "reset", "code", "password", "passwd",
		"passphrase", "secret", "token", "apikey", "api_key", "api-key",
		"privatekey", "private_key", "private-key", "credential", "bearer", "otp", "pin":
		return false
	default:
		return true
	}
}

func redactURL(value string) string {
	trimmed, suffix := trimURLPunctuation(value)
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return value
	}
	if parsed.User != nil {
		parsed.User = url.User(redactedValue)
	}
	query := parsed.Query()
	for key := range query {
		if sensitiveQueryKey(key) {
			query[key] = []string{redactedValue}
		}
	}
	parsed.RawQuery = query.Encode()
	if parsed.Fragment != "" {
		parsed.Fragment = redactedValue
	}
	return parsed.String() + suffix
}

func sensitiveQueryKey(value string) bool {
	if agent.IsSensitiveEnvName(value) {
		return true
	}
	compact := strings.ToLower(strings.NewReplacer("-", "", "_", "", ".", "").Replace(strings.TrimSpace(value)))
	switch compact {
	case "key", "sig", "signature", "auth", "authorization", "code",
		"otp", "pin", "bearer", "credential", "privatekey", "apikey", "passphrase":
		return true
	default:
		return false
	}
}

func safeKind(value Kind) Kind {
	switch value {
	case KindRunStarted, KindRunFinished, KindSessionStarted, KindSupervisionFinished, KindSessionFinished,
		KindEventDetected, KindPolicyEvaluated, KindDecision, KindDelivery,
		KindAttachStarted, KindAttachFinished, KindBackendError, KindSessionCleanup:
		return value
	case "":
		return ""
	default:
		return KindUnknown
	}
}

func safeDecisionBy(value DecisionBy) DecisionBy {
	switch value {
	case DecisionBySystem, DecisionByHuman, DecisionByPolicy:
		return value
	case "":
		return ""
	default:
		return DecisionByUnknown
	}
}

func safeOutcome(value Outcome) Outcome {
	switch value {
	case OutcomeStarted, OutcomeFinished, OutcomeDetected, OutcomePending,
		OutcomeInFlight, OutcomeApplied, OutcomeAsk, OutcomeDryRun,
		OutcomeFallbackUnsupported, OutcomeFallbackStale,
		OutcomeFallbackDeliveryUncertain, OutcomeSucceeded, OutcomeFailed,
		OutcomeCancelled, OutcomeSkipped:
		return value
	case "":
		return ""
	default:
		return OutcomeUnknown
	}
}

func safeEventType(value adapters.EventType) adapters.EventType {
	switch value {
	case adapters.EventConfirmation, adapters.EventPermission, adapters.EventCredential, adapters.EventProcessExit:
		return value
	case "":
		return ""
	default:
		return adapters.EventType("unknown")
	}
}

func safeRisk(value adapters.RiskLevel) adapters.RiskLevel {
	switch value {
	case adapters.RiskLow, adapters.RiskUnknown, adapters.RiskHigh:
		return value
	case "":
		return ""
	default:
		return adapters.RiskUnknown
	}
}

func safeDecision(value Decision) Decision {
	switch value {
	case DecisionAllow, DecisionAsk, DecisionDeny:
		return value
	case "":
		return ""
	default:
		return DecisionUnknown
	}
}

func safeCode(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return ""
	}
	if !safeCodePattern.MatchString(value) {
		return "unknown"
	}
	return value
}

func truncateRunes(value string, maximum int) string {
	characters := []rune(value)
	if len(characters) <= maximum {
		return value
	}
	return string(characters[:maximum])
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func trimURLPunctuation(value string) (string, string) {
	index := len(value)
	for index > 0 && strings.ContainsRune(".,!)]}", rune(value[index-1])) {
		index--
	}
	return value[:index], value[index:]
}
