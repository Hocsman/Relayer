// Package record writes and reads asciicast v2 transcripts so an interactive
// terminal session can be replayed later for audit or training. It owns the
// on-disk recording layout and never interprets the bytes a session produced.
package record

import (
	"encoding/json"
	"errors"
	"math"
	"strconv"
)

// CastVersion identifies the only asciicast representation written and read.
const CastVersion = 2

// offsetDecimals is the fixed fractional precision of every frame offset. A
// constant width keeps a transcript diffable and its lines comparable.
const offsetDecimals = 6

// Header is the first line of a .cast file.
type Header struct {
	Version   int               `json:"version"`
	Width     int               `json:"width"`
	Height    int               `json:"height"`
	Timestamp int64             `json:"timestamp,omitempty"`
	Title     string            `json:"title,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
}

// Kind is the closed asciicast event vocabulary.
type Kind string

const (
	KindOutput Kind = "o"
	KindInput  Kind = "i"
	KindResize Kind = "r"
	KindMarker Kind = "m"
)

// Frame is one asciicast event. It marshals to the array form the format
// requires rather than to an object.
type Frame struct {
	Offset float64
	Kind   Kind
	Data   string
}

// MarshalJSON emits the asciicast array form [1.234567,"o","text"].
func (f Frame) MarshalJSON() ([]byte, error) {
	if !knownKind(f.Kind) {
		return nil, errors.New("unknown asciicast frame kind")
	}
	if math.IsNaN(f.Offset) || math.IsInf(f.Offset, 0) {
		return nil, errors.New("asciicast frame offset is not finite")
	}
	kind, err := json.Marshal(string(f.Kind))
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(f.Data)
	if err != nil {
		return nil, err
	}
	encoded := make([]byte, 0, len(kind)+len(data)+offsetDecimals+8)
	encoded = append(encoded, '[')
	encoded = strconv.AppendFloat(encoded, f.Offset, 'f', offsetDecimals, 64)
	encoded = append(encoded, ',')
	encoded = append(encoded, kind...)
	encoded = append(encoded, ',')
	encoded = append(encoded, data...)
	encoded = append(encoded, ']')
	return encoded, nil
}

// UnmarshalJSON parses the asciicast array form and rejects any kind outside
// the closed vocabulary.
func (f *Frame) UnmarshalJSON(data []byte) error {
	var fields []json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return errors.New("asciicast frame is not an array")
	}
	if len(fields) != 3 {
		return errors.New("asciicast frame does not hold three elements")
	}
	var offset float64
	if err := json.Unmarshal(fields[0], &offset); err != nil {
		return errors.New("asciicast frame offset is not a number")
	}
	if math.IsNaN(offset) || math.IsInf(offset, 0) {
		return errors.New("asciicast frame offset is not finite")
	}
	var kind string
	if err := json.Unmarshal(fields[1], &kind); err != nil {
		return errors.New("asciicast frame kind is not a string")
	}
	if !knownKind(Kind(kind)) {
		return errors.New("unknown asciicast frame kind")
	}
	var payload string
	if err := json.Unmarshal(fields[2], &payload); err != nil {
		return errors.New("asciicast frame data is not a string")
	}
	f.Offset = offset
	f.Kind = Kind(kind)
	f.Data = payload
	return nil
}

func knownKind(kind Kind) bool {
	switch kind {
	case KindOutput, KindInput, KindResize, KindMarker:
		return true
	}
	return false
}

// validateHeader rejects a header Relayer cannot replay.
func validateHeader(header Header) error {
	if header.Version != CastVersion {
		return errors.New("unsupported asciicast version")
	}
	if header.Width <= 0 || header.Height <= 0 {
		return errors.New("asciicast header size is not positive")
	}
	return nil
}
