package record

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFrameMarshalsToTheAsciicastArrayForm(t *testing.T) {
	encoded, err := json.Marshal(Frame{Offset: 1.234567, Kind: KindOutput, Data: "hello\r\n"})
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	const expected = `[1.234567,"o","hello\r\n"]`
	if string(encoded) != expected {
		t.Fatalf("frame = %s, want %s", encoded, expected)
	}
}

func TestFrameOffsetAlwaysHasSixDecimals(t *testing.T) {
	cases := []struct {
		offset float64
		want   string
	}{
		{offset: 0, want: "0.000000"},
		{offset: 2, want: "2.000000"},
		{offset: 1.2345678, want: "1.234568"},
		{offset: 0.0000004, want: "0.000000"},
	}
	for _, testCase := range cases {
		encoded, err := json.Marshal(Frame{Offset: testCase.offset, Kind: KindOutput, Data: ""})
		if err != nil {
			t.Fatalf("marshal frame %v: %v", testCase.offset, err)
		}
		offset, _, found := strings.Cut(strings.TrimPrefix(string(encoded), "["), ",")
		if !found {
			t.Fatalf("frame %s has no offset field", encoded)
		}
		if offset != testCase.want {
			t.Fatalf("offset = %s, want %s", offset, testCase.want)
		}
	}
}

func TestFrameRoundTrip(t *testing.T) {
	cases := []Frame{
		{Offset: 0, Kind: KindOutput, Data: "plain"},
		{Offset: 12.5, Kind: KindInput, Data: "\x03"},
		{Offset: 3.25, Kind: KindResize, Data: "120x40"},
		{Offset: 9.125, Kind: KindMarker, Data: truncatedMarker},
		{Offset: 1.5, Kind: KindOutput, Data: "quoted \" backslash \\ unicode é"},
	}
	for _, original := range cases {
		encoded, err := json.Marshal(original)
		if err != nil {
			t.Fatalf("marshal %v: %v", original, err)
		}
		var decoded Frame
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatalf("unmarshal %s: %v", encoded, err)
		}
		if decoded != original {
			t.Fatalf("round trip = %v, want %v", decoded, original)
		}
	}
}

func TestFrameRejectsMalformedInput(t *testing.T) {
	cases := []string{
		`{"offset":1,"kind":"o","data":"x"}`,
		`[1.0,"o"]`,
		`[1.0,"o","x","extra"]`,
		`[1.0,"x","x"]`,
		`["nope","o","x"]`,
		`[1.0,5,"x"]`,
		`[1.0,"o",5]`,
		`not json at all`,
		`[]`,
	}
	for _, line := range cases {
		var frame Frame
		if err := json.Unmarshal([]byte(line), &frame); err == nil {
			t.Fatalf("accepted malformed frame %s", line)
		}
	}
}

func TestFrameRejectsAnUnknownKindOnMarshal(t *testing.T) {
	if _, err := json.Marshal(Frame{Offset: 1, Kind: Kind("x"), Data: "x"}); err == nil {
		t.Fatal("marshalled an unknown frame kind")
	}
}

func TestValidateHeaderRejectsForeignHeaders(t *testing.T) {
	cases := []Header{
		{Version: 1, Width: 80, Height: 24},
		{Version: CastVersion, Width: 0, Height: 24},
		{Version: CastVersion, Width: 80, Height: -1},
	}
	for _, header := range cases {
		if err := validateHeader(header); err == nil {
			t.Fatalf("accepted header %+v", header)
		}
	}
	if err := validateHeader(Header{Version: CastVersion, Width: 80, Height: 24}); err != nil {
		t.Fatalf("rejected a valid header: %v", err)
	}
}
