package intercept

import "strings"

// Pattern describes one interactive prompt that requires human input.
type Pattern struct {
	Name        string
	Description string
	Expression  string
	Sensitive   bool
}

var defaultPatterns = []Pattern{
	{
		Name:        "overwrite",
		Description: "confirmation d'écrasement",
		Expression:  `(?i)overwrite.*\[y/n\]`,
	},
	{
		Name:        "confirmation",
		Description: "confirmation oui/non",
		Expression:  `(?i)\[[yn]/[yn]\]`,
	},
	{
		Name:        "password",
		Description: "saisie d'un mot de passe",
		Expression:  `(?im)password:[[:space:]]*$`,
		Sensitive:   true,
	},
	{
		Name:        "continue",
		Description: "confirmation de poursuite",
		Expression:  `(?i)do you want to continue`,
	},
}

// DefaultPatterns returns an independent copy of the built-in prompt list.
func DefaultPatterns() []Pattern {
	return append([]Pattern(nil), defaultPatterns...)
}

// IsSensitiveText reports whether text looks like a credential or secret
// prompt. It complements static Pattern.Sensitive metadata by classifying the
// actual matched output at runtime.
func IsSensitiveText(value string) bool {
	text := strings.ToLower(value)
	return strings.Contains(text, "password") ||
		strings.Contains(text, "passphrase") ||
		strings.Contains(text, "mot de passe") ||
		strings.Contains(text, "credential") ||
		strings.Contains(text, "secret") ||
		strings.Contains(text, "sensitive") ||
		strings.Contains(text, "api key") ||
		strings.Contains(text, "api_key") ||
		strings.Contains(text, "apikey") ||
		strings.Contains(text, "access key") ||
		strings.Contains(text, "token") ||
		strings.Contains(text, "pin:") ||
		strings.Contains(text, "pin code") ||
		strings.Contains(text, "code pin") ||
		strings.Contains(text, "otp") ||
		strings.Contains(text, "clé api") ||
		strings.Contains(text, "cle api")
}
