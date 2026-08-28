package fixturecapture

import (
	"errors"
	"strings"
	"testing"
)

func testAnonymizer(t *testing.T) *Anonymizer {
	t.Helper()
	anonymizer, err := NewAnonymizer([]string{"/Users/tester", `C:\Users\tester`})
	if err != nil {
		t.Fatal(err)
	}
	return anonymizer
}

func TestAnonymizerRejectsCredentialShapes(t *testing.T) {
	tests := []string{
		"token=fixture-token-value",
		"Authorization: Bearer abcdefghijklmnop",
		"eyJhbGciOiJIUzI1NiJ9.e30.abcdefghijklmnop",
		"https://fixture-user:fixture-password@example.invalid/path",
		"OPENAI_API_KEY=sk-fixturevalue123456",
		"to\x1b[31mken\x1b[0m=fixture-secret-value",
		"eyJhbGci\x1b[32mOiJIUzI1NiJ9\x1b[0m.e30.abcdefghijklmnop",
		"-----BEGIN OPENSSH PRIVATE KEY-----\nfixture-data",
		"AKIAABCDEFGHIJKLMNOP",
		"xox" + "b-1234567890-abcdefghijklmnop",
		"-----BEGIN OPEN\x1b[31mSSH PRIVATE KEY-----\nfixture-data",
		"AKIAABCDEFGH\x1b[32mIJKLMNOP",
		"xoxb-1234567890-abc\x1b[33mdefghijklmnop",
	}
	for _, input := range tests {
		_, err := testAnonymizer(t).Anonymize([]byte(input))
		if !errors.Is(err, ErrSensitiveContent) {
			t.Fatalf("Anonymize(%q) error = %v, want ErrSensitiveContent", input, err)
		}
	}
}

func TestAnonymizerNormalizesFragmentableANSISequences(t *testing.T) {
	input := []byte("safe \x1b]0;private title\x07\x1b[31mcolored\x1b[0m prompt")
	got, err := testAnonymizer(t).Anonymize(input)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "safe colored prompt" {
		t.Fatalf("ANSI-normalized output = %q", got)
	}
}

func TestAnonymizerReplacesIdentityWithoutRetainingCallerStorage(t *testing.T) {
	homePaths := []string{"/Users/tester"}
	anonymizer, err := NewAnonymizer(homePaths)
	if err != nil {
		t.Fatal(err)
	}
	homePaths[0] = "/mutated"
	input := []byte("/Users/tester/project alice@example.invalid C:\\Users\\someone\\repo /home/bob/work")
	got, err := anonymizer.Anonymize(input)
	if err != nil {
		t.Fatal(err)
	}
	want := "[HOME]/project [EMAIL] [HOME]\\repo [HOME]/work"
	if string(got) != want {
		t.Fatalf("Anonymize() = %q, want %q", got, want)
	}
	input[0] = 'X'
	if string(got) != want {
		t.Fatal("anonymized bytes alias caller input")
	}
	if strings.Contains(string(got), "tester") || strings.Contains(string(got), "alice") || strings.Contains(string(got), "someone") || strings.Contains(string(got), "bob") {
		t.Fatalf("identity remained in anonymized output: %q", got)
	}
}

func TestAnonymizerRejectsInvalidUTF8(t *testing.T) {
	if _, err := testAnonymizer(t).Anonymize([]byte{0xff, 0xfe}); err == nil {
		t.Fatal("invalid UTF-8 accepted")
	}
}

func TestNormalizeOptionsCopiesArgvAndRejectsSensitiveArgument(t *testing.T) {
	arguments := []string{"tool", "safe"}
	normalized, err := normalizeOptions(Options{
		Tool: "custom", Adapter: "generic", Command: arguments, Anonymizer: testAnonymizer(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	arguments[1] = "mutated"
	if normalized.Command[1] != "safe" {
		t.Fatalf("normalized argv retained caller storage: %#v", normalized.Command)
	}

	_, err = normalizeOptions(Options{
		Tool: "custom", Adapter: "generic",
		Command: []string{"tool", "token=fixture-secret"}, Anonymizer: testAnonymizer(t),
	})
	if !errors.Is(err, ErrSensitiveContent) || strings.Contains(err.Error(), "fixture-secret") {
		t.Fatalf("sensitive argv error = %v", err)
	}

	_, err = normalizeOptions(Options{
		Tool: "custom", Adapter: "generic",
		Command: []string{"tool", "token=", "fixture-secret"}, Anonymizer: testAnonymizer(t),
	})
	if !errors.Is(err, ErrSensitiveContent) || strings.Contains(err.Error(), "fixture-secret") {
		t.Fatalf("split sensitive argv error = %v", err)
	}

	secret := "sk-fixturevalue123456"
	tests := []struct {
		name    string
		options Options
	}{
		{
			name: "persisted tool",
			options: Options{
				Tool: secret, Adapter: "generic", Command: []string{"tool"}, Anonymizer: testAnonymizer(t),
			},
		},
		{
			name: "persisted adapter",
			options: Options{
				Tool: "custom", Adapter: secret, Command: []string{"tool"}, Anonymizer: testAnonymizer(t),
			},
		},
		{
			name: "executable",
			options: Options{
				Tool: "custom", Adapter: "generic", Command: []string{secret}, Anonymizer: testAnonymizer(t),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := normalizeOptions(test.options)
			if !errors.Is(err, ErrSensitiveContent) || strings.Contains(err.Error(), secret) {
				t.Fatalf("secret-shaped option error = %v", err)
			}
		})
	}
}
