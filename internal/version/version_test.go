package version

import "testing"

func TestStringReportsInjectedReleaseMetadata(t *testing.T) {
	previousVersion, previousCommit := Version, Commit
	t.Cleanup(func() {
		Version, Commit = previousVersion, previousCommit
	})
	Version = "0.1.0-alpha.1"
	Commit = "0123456789abcdef"

	if got, want := String(), "relayer 0.1.0-alpha.1 (commit 0123456789abcdef)"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestStringKeepsDevelopmentBuildHonestAndSingleLine(t *testing.T) {
	previousVersion, previousCommit := Version, Commit
	t.Cleanup(func() {
		Version, Commit = previousVersion, previousCommit
	})

	for _, test := range []struct {
		name    string
		version string
		commit  string
		want    string
	}{
		{name: "defaults", version: "dev", commit: "unknown", want: "relayer dev (commit unknown)"},
		{name: "blank", version: "  ", commit: "", want: "relayer dev (commit unknown)"},
		{name: "control", version: "1.0.0\nforged", commit: "abc\tdef", want: "relayer dev (commit unknown)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			Version, Commit = test.version, test.commit
			if got := String(); got != test.want {
				t.Fatalf("String() = %q, want %q", got, test.want)
			}
		})
	}
}
