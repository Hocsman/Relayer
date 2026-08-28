package adapters

import "testing"

// A missed word here is an unmasked credential field, so the vocabulary is
// pinned rather than left to drift.
func TestIsSensitiveTextRecognizesSecondFactorVocabulary(t *testing.T) {
	for _, sensitive := range []string{
		"Password:",
		"Enter your passphrase",
		"API key",
		"api_key",
		"Paste your token",
		"Enter the OTP",
		"TOTP code",
		"Enter your 2FA code",
		"MFA challenge",
		"Enter the one-time code we sent you",
		"Verification code:",
		"Authentication code",
		"Security code",
		"Recovery code",
		"Backup code",
		"Paste the private key",
		"private_key",
		"Enter your seed phrase",
		"Enter the mnemonic",
		"Mot de passe :",
		"Clé API",
		"Code de vérification",
		"Code de sécurité",
		"Clé privée",
	} {
		if !IsSensitiveText(sensitive) {
			t.Errorf("IsSensitiveText(%q) = false, want true", sensitive)
		}
	}

	for _, ordinary := range []string{
		"Overwrite file? [Y/n]",
		"Do you want to continue?",
		"Trust the files in this folder?",
		"Run npm install?",
		"",
	} {
		if IsSensitiveText(ordinary) {
			t.Errorf("IsSensitiveText(%q) = true, want false", ordinary)
		}
	}
}
