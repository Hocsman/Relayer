package intercept

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestDefaultPatternsReturnsIndependentCopy(t *testing.T) {
	first := DefaultPatterns()
	if len(first) == 0 {
		t.Fatal("DefaultPatterns returned no patterns")
	}
	originalDescription := first[0].Description
	first[0].Description = "mutated"

	second := DefaultPatterns()
	if second[0].Description != originalDescription {
		t.Fatal("DefaultPatterns returned storage shared with a previous caller")
	}
}

func TestNewRejectsInvalidRegex(t *testing.T) {
	_, err := New([]Pattern{{Name: "broken", Expression: "("}}, 128, Hooks{})
	if err == nil {
		t.Fatal("New accepted an invalid regular expression")
	}
}

func TestDetectsPromptAcrossChunksAndSplitANSI(t *testing.T) {
	var detections []Detection
	outputNotifications := 0
	target, err := New(
		[]Pattern{{
			Name:        "overwrite",
			Description: "overwrite confirmation",
			Expression:  `(?i)overwrite.*\[y/n\]`,
		}},
		128,
		Hooks{
			OnOutput: func() { outputNotifications++ },
			OnPrompt: func(detection Detection) {
				detections = append(detections, detection)
			},
		},
	)
	if err != nil {
		t.Fatalf("New returned an error: %v", err)
	}

	for _, chunk := range []string{
		"\x1b[3",
		"1mOver",
		"write? [Y",
		"/n]\x1b[0",
		"m",
	} {
		target.Consume([]byte(chunk))
	}

	if len(detections) != 1 {
		t.Fatalf("got %d detections, want 1: %#v", len(detections), detections)
	}
	want := Detection{
		Pattern:     "overwrite",
		Description: "overwrite confirmation",
		Match:       "Overwrite? [Y/n]",
	}
	if detections[0] != want {
		t.Fatalf("detection = %#v, want %#v", detections[0], want)
	}
	if outputNotifications != 3 {
		t.Fatalf("got %d clean-output notifications, want 3", outputNotifications)
	}
	if !target.IsBlocked() {
		t.Fatal("interceptor should be blocked after prompt detection")
	}
	if got := target.Output(); got != "Overwrite? [Y/n]" {
		t.Fatalf("sanitized output = %q, want %q", got, "Overwrite? [Y/n]")
	}
}

func TestDeduplicatesUntilAcknowledgedThenRearms(t *testing.T) {
	var detections []Detection
	target, err := New(
		[]Pattern{{
			Name:        "overwrite",
			Description: "overwrite confirmation",
			Expression:  `(?i)overwrite.*\[y/n\]`,
		}},
		128,
		Hooks{OnPrompt: func(detection Detection) {
			detections = append(detections, detection)
		}},
	)
	if err != nil {
		t.Fatalf("New returned an error: %v", err)
	}

	target.Consume([]byte("Overwrite first? [Y/n]"))
	target.Consume([]byte("\nadditional output while still blocked\n"))
	if len(detections) != 1 {
		t.Fatalf("got %d detections while blocked, want 1", len(detections))
	}

	target.Acknowledge()
	if target.IsBlocked() {
		t.Fatal("interceptor remained blocked after Acknowledge")
	}
	target.Consume([]byte("OVERWRITE second? [y/n]"))
	if len(detections) != 2 {
		t.Fatalf("got %d detections after rearming, want 2", len(detections))
	}
	if detections[1].Match != "OVERWRITE second? [y/n]" {
		t.Fatalf("second match = %q", detections[1].Match)
	}
	if !target.IsBlocked() {
		t.Fatal("interceptor should be blocked after the second prompt")
	}
}

func TestReblockRestoresWaitingState(t *testing.T) {
	target, err := New(nil, 16, Hooks{})
	if err != nil {
		t.Fatalf("New returned an error: %v", err)
	}
	target.Reblock()
	if !target.IsBlocked() {
		t.Fatal("Reblock did not restore the blocked state")
	}
	target.Acknowledge()
	if target.IsBlocked() {
		t.Fatal("Acknowledge did not clear the reblocked state")
	}
}

func TestBoundsMalformedANSICarryAndKeepsDetecting(t *testing.T) {
	var detections []Detection
	target, err := New(
		[]Pattern{{
			Name:        "password",
			Description: "password prompt",
			Expression:  `(?im)password:[[:space:]]*$`,
		}},
		256,
		Hooks{OnPrompt: func(detection Detection) {
			detections = append(detections, detection)
		}},
	)
	if err != nil {
		t.Fatalf("New returned an error: %v", err)
	}

	target.Consume([]byte("\x1b]" + strings.Repeat("x", maxANSICarrySize+32)))
	if got := len(target.ansiCarry); got > maxANSICarrySize {
		t.Fatalf("malformed ANSI carry grew to %d bytes", got)
	}
	target.Consume([]byte("\nPassword:"))
	if len(detections) != 1 {
		t.Fatalf("detector did not recover after malformed ANSI input; got %d detections", len(detections))
	}
}

func TestStreamingOutputRemainsRingBounded(t *testing.T) {
	const capacity = 64
	outputNotifications := 0
	target, err := New(nil, capacity, Hooks{
		OnOutput: func() { outputNotifications++ },
	})
	if err != nil {
		t.Fatalf("New returned an error: %v", err)
	}

	stream := strings.Repeat("0123456789", 20)
	for offset := 0; offset < len(stream); offset += 17 {
		end := min(offset+17, len(stream))
		target.Consume([]byte(stream[offset:end]))
		if got := len(target.Output()); got > capacity {
			t.Fatalf("output grew to %d bytes after offset %d, capacity is %d", got, offset, capacity)
		}
	}

	if outputNotifications == 0 {
		t.Fatal("stream emitted no output notifications")
	}
	want := stream[len(stream)-capacity:]
	if got := target.Output(); got != want {
		t.Fatalf("retained output = %q, want newest bytes %q", got, want)
	}
}

func TestSensitiveMatchOverridesObfuscatedPatternMetadata(t *testing.T) {
	var detected Detection
	target, err := New(
		[]Pattern{{
			Name:        "auth",
			Description: "authentication gate",
			Expression:  `(?i)p[a]ssword:`,
		}},
		128,
		Hooks{OnPrompt: func(detection Detection) { detected = detection }},
	)
	if err != nil {
		t.Fatalf("New returned an error: %v", err)
	}
	target.Consume([]byte("Password:"))
	if !detected.Sensitive {
		t.Fatal("password match was not marked sensitive at runtime")
	}
}

func TestIsSensitiveTextRecognizesCredentialPrompts(t *testing.T) {
	for _, prompt := range []string{
		"Password:",
		"Enter passphrase:",
		"API key:",
		"Access token:",
		"PIN:",
		"OTP:",
		"Clé API:",
	} {
		t.Run(prompt, func(t *testing.T) {
			if !IsSensitiveText(prompt) {
				t.Fatalf("credential prompt %q was not classified as sensitive", prompt)
			}
		})
	}
	if IsSensitiveText("Overwrite file? [Y/n]") {
		t.Fatal("ordinary confirmation was classified as sensitive")
	}
}

func TestRunConsumesReaderThroughEOF(t *testing.T) {
	var detected Detection
	target, err := New(
		[]Pattern{{Name: "continue", Expression: `(?i)continue\?`}},
		64,
		Hooks{OnPrompt: func(detection Detection) { detected = detection }},
	)
	if err != nil {
		t.Fatalf("New returned an error: %v", err)
	}

	if err := target.Run(context.Background(), strings.NewReader("Continue?")); err != nil {
		t.Fatalf("Run returned an error: %v", err)
	}
	if detected.Pattern != "continue" || detected.Match != "Continue?" {
		t.Fatalf("detection = %#v", detected)
	}
	if got := target.Output(); got != "Continue?" {
		t.Fatalf("Output = %q, want %q", got, "Continue?")
	}
}

func TestRunSuppressesReadErrorAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	target, err := New(nil, 16, Hooks{})
	if err != nil {
		t.Fatalf("New returned an error: %v", err)
	}

	reader := readerFunc(func([]byte) (int, error) {
		return 0, errors.New("closed reader")
	})
	if err := target.Run(ctx, reader); err != nil {
		t.Fatalf("Run returned cancellation read error: %v", err)
	}
}

func TestRunReturnsUnexpectedReadError(t *testing.T) {
	want := errors.New("read failed")
	target, err := New(nil, 16, Hooks{})
	if err != nil {
		t.Fatalf("New returned an error: %v", err)
	}

	reader := readerFunc(func([]byte) (int, error) { return 0, want })
	if got := target.Run(context.Background(), reader); !errors.Is(got, want) {
		t.Fatalf("Run error = %v, want %v", got, want)
	}
}

type readerFunc func([]byte) (int, error)

func (read readerFunc) Read(p []byte) (int, error) {
	return read(p)
}

var _ io.Reader = readerFunc(nil)
