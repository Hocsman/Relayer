package fixturecapture

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

// encoding/json may expand one safe output byte (notably '<', '>', or '&') to
// a six-byte escape. Keep validation bounded while accepting every fixture
// whose decoded stream is within HardMaxBytes.
const maxJSONBytes = HardMaxBytes*6 + 1024*1024

var (
	errInvalidFixtureJSON   = errors.New("fixture JSON is invalid")
	errUnknownFixtureField  = errors.New("fixture JSON contains a non-canonical field")
	errDuplicateJSONField   = errors.New("fixture JSON contains a duplicate field")
	errInvalidFixtureType   = errors.New("fixture JSON field has an invalid type")
	errTooManyFixtureChunks = errors.New("fixture JSON contains too many chunks")
	errMissingFixtureField  = errors.New("fixture JSON is missing a required field")
)

type fixtureJSONField uint8

const (
	fixtureJSONString fixtureJSONField = iota
	fixtureJSONInteger
	fixtureJSONOptionalInteger
	fixtureJSONBoolean
	fixtureJSONChunks
)

var (
	fixtureJSONFields = map[string]fixtureJSONField{
		"schema_version": fixtureJSONInteger,
		"tool":           fixtureJSONString,
		"adapter":        fixtureJSONString,
		"backend":        fixtureJSONString,
		"outcome":        fixtureJSONString,
		"exit_code":      fixtureJSONOptionalInteger,
		"truncated":      fixtureJSONBoolean,
		"chunks":         fixtureJSONChunks,
	}
	chunkJSONFields = map[string]fixtureJSONField{
		"sequence": fixtureJSONInteger,
		"data":     fixtureJSONString,
	}
	fixtureJSONRequired = []string{"schema_version", "tool", "adapter", "backend", "outcome", "chunks"}
	chunkJSONRequired   = []string{"sequence", "data"}
)

// Marshal validates and serializes one fixture with a trailing newline. The
// returned slice never aliases fixture storage.
func Marshal(fixture Fixture, anonymizer *Anonymizer) ([]byte, error) {
	if err := Validate(fixture, anonymizer); err != nil {
		return nil, err
	}
	encoded, err := json.MarshalIndent(fixture, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode fixture: %w", err)
	}
	encoded = append(encoded, '\n')
	return bytes.Clone(encoded), nil
}

// Decode accepts exactly one strict JSON value. Unknown fields are rejected so
// argv, environment, manual input, or other unsafe ad-hoc fields cannot hide
// in a nominally valid fixture.
func Decode(input []byte, anonymizer *Anonymizer) (Fixture, error) {
	if len(input) > maxJSONBytes {
		return Fixture{}, fmt.Errorf("fixture JSON exceeds %d bytes", maxJSONBytes)
	}
	if err := prevalidateFixtureJSON(input); err != nil {
		return Fixture{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.DisallowUnknownFields()
	var fixture Fixture
	if err := decoder.Decode(&fixture); err != nil {
		return Fixture{}, fmt.Errorf("decode fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Fixture{}, errors.New("fixture contains more than one JSON value")
		}
		return Fixture{}, fmt.Errorf("decode trailing fixture data: %w", err)
	}
	if err := Validate(fixture, anonymizer); err != nil {
		return Fixture{}, err
	}
	return cloneFixture(fixture), nil
}

func prevalidateFixtureJSON(input []byte) error {
	if !utf8.Valid(input) {
		return errInvalidFixtureJSON
	}
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return errInvalidFixtureJSON
	}
	if err := validateFixtureJSONObject(decoder, fixtureJSONFields, fixtureJSONRequired); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errInvalidFixtureJSON
	}
	return nil
}

func validateFixtureJSONObject(decoder *json.Decoder, allowed map[string]fixtureJSONField, required []string) error {
	seen := make(map[string]struct{}, len(allowed))
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return errInvalidFixtureJSON
		}
		key, ok := token.(string)
		if !ok {
			return errInvalidFixtureJSON
		}
		field, ok := allowed[key]
		if !ok {
			return errUnknownFixtureField
		}
		if _, duplicate := seen[key]; duplicate {
			return errDuplicateJSONField
		}
		seen[key] = struct{}{}
		if field == fixtureJSONChunks {
			if err := validateFixtureChunks(decoder); err != nil {
				return err
			}
			continue
		}
		if err := validateFixtureScalar(decoder, field); err != nil {
			return err
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return errInvalidFixtureJSON
	}
	for _, key := range required {
		if _, ok := seen[key]; !ok {
			return errMissingFixtureField
		}
	}
	return nil
}

func validateFixtureChunks(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil || token != json.Delim('[') {
		return errInvalidFixtureJSON
	}
	count := 0
	for decoder.More() {
		count++
		if count > maxArtifactChunks {
			return errTooManyFixtureChunks
		}
		token, err := decoder.Token()
		if err != nil || token != json.Delim('{') {
			return errInvalidFixtureJSON
		}
		if err := validateFixtureJSONObject(decoder, chunkJSONFields, chunkJSONRequired); err != nil {
			return err
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim(']') {
		return errInvalidFixtureJSON
	}
	return nil
}

func validateFixtureScalar(decoder *json.Decoder, field fixtureJSONField) error {
	token, err := decoder.Token()
	if err != nil {
		return errInvalidFixtureJSON
	}
	switch field {
	case fixtureJSONString:
		if _, ok := token.(string); !ok {
			return errInvalidFixtureType
		}
	case fixtureJSONInteger:
		if !validFixtureJSONInteger(token, false) {
			return errInvalidFixtureType
		}
	case fixtureJSONOptionalInteger:
		if !validFixtureJSONInteger(token, true) {
			return errInvalidFixtureType
		}
	case fixtureJSONBoolean:
		if _, ok := token.(bool); !ok {
			return errInvalidFixtureType
		}
	default:
		return errInvalidFixtureJSON
	}
	return nil
}

func validFixtureJSONInteger(token any, optional bool) bool {
	if token == nil {
		return optional
	}
	number, ok := token.(json.Number)
	if !ok {
		return false
	}
	_, err := strconv.ParseInt(string(number), 10, strconv.IntSize)
	return err == nil
}

// Validate checks the schema, all memory bounds, and that the persisted chunk
// stream is already in canonical anonymized form.
func Validate(fixture Fixture, anonymizer *Anonymizer) error {
	if anonymizer == nil {
		return errors.New("fixture anonymizer must not be nil")
	}
	if fixture.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported fixture schema version %d", fixture.SchemaVersion)
	}
	if !safeIdentifier.MatchString(fixture.Tool) {
		return errors.New("fixture tool is invalid")
	}
	if err := validatePersistedIdentifier("fixture tool", fixture.Tool, anonymizer); err != nil {
		return err
	}
	if !safeIdentifier.MatchString(fixture.Adapter) {
		return errors.New("fixture adapter is invalid")
	}
	if err := validatePersistedIdentifier("fixture adapter", fixture.Adapter, anonymizer); err != nil {
		return err
	}
	if fixture.Backend != BackendPTY && fixture.Backend != BackendTmux {
		return errors.New("fixture backend is invalid")
	}
	switch fixture.Outcome {
	case OutcomeExited:
		if fixture.ExitCode == nil {
			return errors.New("exited fixture requires an exit code")
		}
		if *fixture.ExitCode < -1 || *fixture.ExitCode > 255 {
			return errors.New("exited fixture has an invalid exit code")
		}
		if fixture.Truncated {
			return errors.New("exited fixture cannot be marked truncated")
		}
	case OutcomeTimedOut:
		if fixture.ExitCode != nil || fixture.Truncated {
			return errors.New("timed-out fixture cannot have an exit code or truncation marker")
		}
	case OutcomeOutputLimit:
		if fixture.ExitCode != nil || !fixture.Truncated {
			return errors.New("output-limit fixture must be truncated and have no exit code")
		}
	default:
		return errors.New("fixture outcome is invalid")
	}
	if fixture.Chunks == nil {
		return errors.New("fixture chunks must be a JSON array")
	}
	if len(fixture.Chunks) > maxArtifactChunks {
		return fmt.Errorf("fixture exceeds %d chunks", maxArtifactChunks)
	}

	var stream strings.Builder
	for index, chunk := range fixture.Chunks {
		if chunk.Sequence != index {
			return fmt.Errorf("fixture chunk %d has sequence %d", index, chunk.Sequence)
		}
		if chunk.Data == "" {
			return fmt.Errorf("fixture chunk %d is empty", index)
		}
		if !utf8.ValidString(chunk.Data) {
			return fmt.Errorf("fixture chunk %d is not valid UTF-8", index)
		}
		if len(chunk.Data) > artifactChunkBytes {
			return fmt.Errorf("fixture chunk %d exceeds %d bytes", index, artifactChunkBytes)
		}
		if stream.Len()+len(chunk.Data) > HardMaxBytes {
			return fmt.Errorf("fixture output exceeds %d bytes", HardMaxBytes)
		}
		stream.WriteString(chunk.Data)
	}
	raw := []byte(stream.String())
	sanitized, err := anonymizer.Anonymize(raw)
	if err != nil {
		return fmt.Errorf("validate fixture output: %w", err)
	}
	if !bytes.Equal(raw, sanitized) {
		return errors.New("fixture output is not anonymized")
	}
	return nil
}

// WriteFile atomically publishes a validated fixture. New directories are
// private and the final file is always mode 0600 on Unix.
func WriteFile(path string, fixture Fixture, anonymizer *Anonymizer) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("fixture output path must not be blank")
	}
	encoded, err := Marshal(fixture, anonymizer)
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create fixture directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".relayer-fixture-*")
	if err != nil {
		return fmt.Errorf("create temporary fixture: %w", err)
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("restrict temporary fixture permissions: %w", err)
	}
	if _, err := temporary.Write(encoded); err != nil {
		return fmt.Errorf("write temporary fixture: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync temporary fixture: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary fixture: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("publish fixture: %w", err)
	}
	committed = true
	if err := syncDirectory(directory); err != nil {
		return fmt.Errorf("sync fixture directory: %w", err)
	}
	return nil
}

// ReadFile performs a bounded read and strict dry validation.
func ReadFile(path string, anonymizer *Anonymizer) (Fixture, error) {
	file, err := os.Open(path)
	if err != nil {
		return Fixture{}, fmt.Errorf("open fixture: %w", err)
	}
	defer file.Close()
	encoded, err := io.ReadAll(io.LimitReader(file, maxJSONBytes+1))
	if err != nil {
		return Fixture{}, fmt.Errorf("read fixture: %w", err)
	}
	if len(encoded) > maxJSONBytes {
		return Fixture{}, fmt.Errorf("fixture JSON exceeds %d bytes", maxJSONBytes)
	}
	return Decode(encoded, anonymizer)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
