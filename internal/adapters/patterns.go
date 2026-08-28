package adapters

import "strings"

var defaultPatterns = []Pattern{
	{Name: "overwrite", Description: "confirmation d'écrasement", Expression: `(?i)overwrite.*\[y/n\]`},
	{Name: "confirmation", Description: "confirmation oui/non", Expression: `(?i)\[[yn]/[yn]\]`},
	{Name: "password", Description: "saisie d'un mot de passe", Expression: `(?im)password:[[:space:]]*$`, Sensitive: true},
	{Name: "continue", Description: "confirmation de poursuite", Expression: `(?i)do you want to continue`},
}

func DefaultPatterns() []Pattern {
	return append([]Pattern(nil), defaultPatterns...)
}

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
		strings.Contains(text, "totp") ||
		strings.Contains(text, "2fa") ||
		strings.Contains(text, "mfa") ||
		strings.Contains(text, "one-time") ||
		strings.Contains(text, "one time code") ||
		strings.Contains(text, "verification code") ||
		strings.Contains(text, "authentication code") ||
		strings.Contains(text, "security code") ||
		strings.Contains(text, "recovery code") ||
		strings.Contains(text, "backup code") ||
		strings.Contains(text, "private key") ||
		strings.Contains(text, "private_key") ||
		strings.Contains(text, "seed phrase") ||
		strings.Contains(text, "mnemonic") ||
		strings.Contains(text, "clé api") ||
		strings.Contains(text, "cle api") ||
		strings.Contains(text, "clé privée") ||
		strings.Contains(text, "cle privee") ||
		strings.Contains(text, "code de vérification") ||
		strings.Contains(text, "code de verification") ||
		strings.Contains(text, "code de sécurité") ||
		strings.Contains(text, "code de securite")
}
