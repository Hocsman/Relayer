package fixturecapture

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validFixture(t *testing.T) Fixture {
	t.Helper()
	code := 0
	fixture, err := testAnonymizer(t).fixture(
		"codex-cli", "generic", BackendPTY, OutcomeExited, &code, false,
		[]byte("prompt from /Users/tester/project for fixture@example.invalid\r\n"),
	)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestFixtureJSONRoundTripIsVersionedStrictAndDefensive(t *testing.T) {
	anonymizer := testAnonymizer(t)
	fixture := validFixture(t)
	encoded, err := Marshal(fixture, anonymizer)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"fixture@example.invalid", "/Users/tester", "environment", "manual_input", "argv"} {
		if bytes.Contains(encoded, []byte(forbidden)) {
			t.Fatalf("serialized fixture contains forbidden value %q: %s", forbidden, encoded)
		}
	}
	decoded, err := Decode(encoded, anonymizer)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.SchemaVersion != SchemaVersion || decoded.Backend != BackendPTY || len(decoded.Chunks) != 1 {
		t.Fatalf("decoded fixture = %#v", decoded)
	}
	fixture.Chunks[0].Data = "mutated"
	if decoded.Chunks[0].Data == "mutated" {
		t.Fatal("decoded fixture aliases source chunks")
	}

	unsafeField := bytes.Replace(encoded, []byte(`"chunks":`), []byte(`"environment":{"TOKEN":"value"},"chunks":`), 1)
	if _, err := Decode(unsafeField, anonymizer); !errors.Is(err, errUnknownFixtureField) {
		t.Fatalf("unknown environment field error = %v", err)
	}
}

func TestDecodeRejectsDuplicateNonCanonicalAndInvalidUTF8JSON(t *testing.T) {
	const secret = "sk-fixturevalue123456"
	tests := []struct {
		name  string
		input []byte
	}{
		{
			name:  "duplicate top-level tool",
			input: []byte(`{"schema_version":1,"tool":"` + secret + `","tool":"fixture-cli","adapter":"generic","backend":"pty","outcome":"exited","exit_code":0,"chunks":[]}`),
		},
		{
			name:  "duplicate chunks",
			input: []byte(`{"schema_version":1,"tool":"fixture-cli","adapter":"generic","backend":"pty","outcome":"exited","exit_code":0,"chunks":[{"sequence":0,"data":"` + secret + `"}],"chunks":[{"sequence":0,"data":"safe"}]}`),
		},
		{
			name:  "duplicate chunk data",
			input: []byte(`{"schema_version":1,"tool":"fixture-cli","adapter":"generic","backend":"pty","outcome":"exited","exit_code":0,"chunks":[{"sequence":0,"data":"` + secret + `","data":"safe"}]}`),
		},
		{
			name:  "case variant is not canonical",
			input: []byte(`{"schema_version":1,"tool":"fixture-cli","Tool":"` + secret + `","adapter":"generic","backend":"pty","outcome":"exited","exit_code":0,"chunks":[]}`),
		},
		{
			name:  "invalid UTF-8",
			input: []byte("{\"schema_version\":1,\"tool\":\"\xff\"}"),
		},
		{
			name:  "schema version must be an integer",
			input: []byte(`{"schema_version":"1","tool":"fixture-cli","adapter":"generic","backend":"pty","outcome":"exited","exit_code":0,"chunks":[]}`),
		},
		{
			name:  "tool must be a string",
			input: []byte(`{"schema_version":1,"tool":{"value":"` + secret + `"},"adapter":"generic","backend":"pty","outcome":"exited","exit_code":0,"chunks":[]}`),
		},
		{
			name:  "exit code must be an integer or null",
			input: []byte(`{"schema_version":1,"tool":"fixture-cli","adapter":"generic","backend":"pty","outcome":"exited","exit_code":true,"chunks":[]}`),
		},
		{
			name:  "truncated must be a boolean",
			input: []byte(`{"schema_version":1,"tool":"fixture-cli","adapter":"generic","backend":"pty","outcome":"exited","exit_code":0,"truncated":1,"chunks":[]}`),
		},
		{
			name:  "chunks must be an array",
			input: []byte(`{"schema_version":1,"tool":"fixture-cli","adapter":"generic","backend":"pty","outcome":"exited","exit_code":0,"chunks":{}}`),
		},
		{
			name:  "sequence must be an integer",
			input: []byte(`{"schema_version":1,"tool":"fixture-cli","adapter":"generic","backend":"pty","outcome":"exited","exit_code":0,"chunks":[{"sequence":"0","data":"safe"}]}`),
		},
		{
			name:  "sequence is required",
			input: []byte(`{"schema_version":1,"tool":"fixture-cli","adapter":"generic","backend":"pty","outcome":"exited","exit_code":0,"chunks":[{"data":"safe"}]}`),
		},
		{
			name:  "data must be a string",
			input: []byte(`{"schema_version":1,"tool":"fixture-cli","adapter":"generic","backend":"pty","outcome":"exited","exit_code":0,"chunks":[{"sequence":0,"data":["` + secret + `"]}]}`),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Decode(test.input, testAnonymizer(t))
			if err == nil || strings.Contains(err.Error(), secret) {
				t.Fatalf("Decode unsafe JSON error = %v", err)
			}
		})
	}
}

func TestDecodePrevalidationBoundsChunkCount(t *testing.T) {
	const secret = "sk-fixturevalue123456"
	var input strings.Builder
	input.WriteString(`{"schema_version":1,"tool":"fixture-cli","adapter":"generic","backend":"pty","outcome":"exited","exit_code":0,"chunks":[`)
	for index := 0; index <= maxArtifactChunks; index++ {
		if index > 0 {
			input.WriteByte(',')
		}
		data := "safe"
		if index == 0 {
			data = secret
		}
		input.WriteString(`{"sequence":0,"data":"`)
		input.WriteString(data)
		input.WriteString(`"}`)
	}
	input.WriteString(`]}`)
	_, err := Decode([]byte(input.String()), testAnonymizer(t))
	if !errors.Is(err, errTooManyFixtureChunks) || strings.Contains(err.Error(), secret) {
		t.Fatalf("oversized chunk-array error = %v", err)
	}
}

func TestFixtureChunkingDoesNotSplitUTF8(t *testing.T) {
	code := 0
	raw := []byte(strings.Repeat("x", artifactChunkBytes-1) + "é" + strings.Repeat("y", 20))
	fixture, err := testAnonymizer(t).fixture("fixture-cli", "generic", BackendPTY, OutcomeExited, &code, false, raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(fixture.Chunks) != 2 || fixture.Chunks[0].Data != strings.Repeat("x", artifactChunkBytes-1) {
		t.Fatalf("UTF-8 chunks = %#v", fixture.Chunks)
	}
	if err := Validate(fixture, testAnonymizer(t)); err != nil {
		t.Fatal(err)
	}
}

func TestFixtureEmptyOutputUsesAnExplicitBoundedChunkArray(t *testing.T) {
	fixture := validFixture(t)
	fixture.Chunks = []Chunk{}
	encoded, err := Marshal(fixture, testAnonymizer(t))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte(`"chunks": []`)) {
		t.Fatalf("empty chunks were not encoded as an array: %s", encoded)
	}
	decoded, err := Decode(encoded, testAnonymizer(t))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Chunks == nil || len(decoded.Chunks) != 0 {
		t.Fatalf("decoded empty chunks = %#v", decoded.Chunks)
	}

	fixture.Chunks = nil
	if err := Validate(fixture, testAnonymizer(t)); err == nil {
		t.Fatal("null chunks accepted")
	}
	fixture.Chunks = make([]Chunk, maxArtifactChunks+1)
	for index := range fixture.Chunks {
		fixture.Chunks[index] = Chunk{Sequence: index, Data: "x"}
	}
	if err := Validate(fixture, testAnonymizer(t)); err == nil || !strings.Contains(err.Error(), "chunks") {
		t.Fatalf("excessive chunk count error = %v", err)
	}
}

func TestFixtureDryValidationRejectsSecretsPathsEmailAndBounds(t *testing.T) {
	anonymizer := testAnonymizer(t)
	tests := []struct {
		name string
		data string
	}{
		{name: "token", data: "token=fixture-secret-value"},
		{name: "jwt", data: "eyJhbGciOiJIUzI1NiJ9.e30.abcdefghijklmnop"},
		{name: "URL credentials", data: "https://user:pass@example.invalid/x"},
		{name: "email", data: "person@example.invalid"},
		{name: "home path", data: "/Users/person/project"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := validFixture(t)
			fixture.Chunks = []Chunk{{Sequence: 0, Data: test.data}}
			if err := Validate(fixture, anonymizer); err == nil {
				t.Fatalf("unsafe fixture data %q accepted", test.data)
			}
		})
	}

	fixture := validFixture(t)
	fixture.Chunks = []Chunk{{Sequence: 0, Data: strings.Repeat("x", artifactChunkBytes+1)}}
	if err := Validate(fixture, anonymizer); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized chunk error = %v", err)
	}
	fixture = validFixture(t)
	fixture.Chunks[0].Sequence = 4
	if err := Validate(fixture, anonymizer); err == nil || !strings.Contains(err.Error(), "sequence") {
		t.Fatalf("invalid sequence error = %v", err)
	}
}

func TestValidateAndDecodeRejectSecretShapedPersistedIdentifiers(t *testing.T) {
	const secret = "sk-fixturevalue123456"
	tests := []struct {
		name   string
		mutate func(*Fixture)
	}{
		{name: "tool", mutate: func(fixture *Fixture) { fixture.Tool = secret }},
		{name: "adapter", mutate: func(fixture *Fixture) { fixture.Adapter = secret }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := validFixture(t)
			test.mutate(&fixture)
			err := Validate(fixture, testAnonymizer(t))
			if !errors.Is(err, ErrSensitiveContent) || strings.Contains(err.Error(), secret) {
				t.Fatalf("Validate secret identifier error = %v", err)
			}
			encoded, marshalErr := json.Marshal(fixture)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			_, err = Decode(encoded, testAnonymizer(t))
			if !errors.Is(err, ErrSensitiveContent) || strings.Contains(err.Error(), secret) {
				t.Fatalf("Decode secret identifier error = %v", err)
			}
		})
	}
}

func TestWriteFileUsesPrivatePermissionsAndReadFileIsBounded(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "private", "fixtures")
	path := filepath.Join(directory, "capture.json")
	anonymizer := testAnonymizer(t)
	if err := WriteFile(path, validFixture(t), anonymizer); err != nil {
		t.Fatal(err)
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("fixture mode = %o, want 600", fileInfo.Mode().Perm())
	}
	directoryInfo, err := os.Stat(filepath.Join(root, "private"))
	if err != nil {
		t.Fatal(err)
	}
	if directoryInfo.Mode().Perm()&0o077 != 0 {
		t.Fatalf("fixture directory mode = %o, want private", directoryInfo.Mode().Perm())
	}
	if _, err := ReadFile(path, anonymizer); err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	oversized := filepath.Join(root, "oversized.json")
	if err := os.WriteFile(oversized, bytes.Repeat([]byte("x"), maxJSONBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(oversized, anonymizer); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized read error = %v", err)
	}
}

func TestWriteFileDoesNotReplaceDestinationOnInvalidFixture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capture.json")
	before := []byte("existing-safe-content")
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	fixture := validFixture(t)
	fixture.Chunks[0].Data = "api_key=fixture-secret"
	err := WriteFile(path, fixture, testAnonymizer(t))
	if !errors.Is(err, ErrSensitiveContent) {
		t.Fatalf("WriteFile error = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("invalid write replaced destination: %q", after)
	}
}
