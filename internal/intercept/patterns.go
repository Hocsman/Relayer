package intercept

import "github.com/Hocsman/Relayer/internal/adapters"

// Pattern remains an alias so existing intercept_patterns callers keep their
// source compatibility while GenericRegexAdapter owns regex interpretation.
type Pattern = adapters.Pattern

// DefaultPatterns returns an independent copy of the built-in prompt list.
func DefaultPatterns() []Pattern {
	return adapters.DefaultPatterns()
}

// IsSensitiveText reports whether text looks like a credential or secret
// prompt. It complements static Pattern.Sensitive metadata by classifying the
// actual matched output at runtime.
func IsSensitiveText(value string) bool {
	return adapters.IsSensitiveText(value)
}
