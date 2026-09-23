package supervise

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// allowedRelayerImports are the only Relayer packages the core may import:
// the vocabularies it decides with. Everything else a front end has — the
// notifier, telemetry, the runtime that owns processes, a user interface — is
// reached through the Engine and Sink interfaces, so that neither front end
// can pull its transport into the code that decides what reaches an agent,
// and so that the core stays testable with a fake engine.
var allowedRelayerImports = map[string]bool{
	"github.com/Hocsman/Relayer/internal/adapters": true,
	"github.com/Hocsman/Relayer/internal/audit":    true,
	"github.com/Hocsman/Relayer/internal/policy":   true,
	"github.com/Hocsman/Relayer/internal/session":  true,
	"github.com/Hocsman/Relayer/internal/terminal": true,
}

// forbiddenStandardImports are standard packages the core must not use even
// though they are standard: the core never talks to the network itself.
var forbiddenStandardImports = []string{"net"}

func TestThePackageImportsOnlyTheSupervisionVocabularies(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list package files: %v", err)
	}
	fileSet := token.NewFileSet()
	checked := 0
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		checked++
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("%s: import %s: %v", name, spec.Path.Value, err)
			}
			if reason := importRefusal(path); reason != "" {
				t.Errorf("%s imports %q: %s", name, path, reason)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no package file was checked")
	}
}

func TestTheImportRuleRefusesWhatItMustRefuse(t *testing.T) {
	for _, path := range []string{
		"github.com/Hocsman/Relayer/internal/notify",
		"github.com/Hocsman/Relayer/internal/telemetry",
		"github.com/Hocsman/Relayer/internal/app",
		"github.com/Hocsman/Relayer/internal/server",
		"github.com/Hocsman/Relayer/internal/tui",
		"github.com/wailsapp/wails/v2/pkg/runtime",
		"github.com/gorilla/websocket",
		"net/http",
		"net",
	} {
		if importRefusal(path) == "" {
			t.Errorf("import %q is accepted, want it refused", path)
		}
	}
	for _, path := range []string{"context", "sync", "strings", "github.com/Hocsman/Relayer/internal/policy"} {
		if reason := importRefusal(path); reason != "" {
			t.Errorf("import %q is refused (%s), want it accepted", path, reason)
		}
	}
}

// importRefusal says why the core may not import path, or "" when it may.
func importRefusal(path string) string {
	first, _, _ := strings.Cut(path, "/")
	if !strings.Contains(first, ".") {
		for _, forbidden := range forbiddenStandardImports {
			if path == forbidden || strings.HasPrefix(path, forbidden+"/") {
				return "the core never talks to the network"
			}
		}
		return ""
	}
	if allowedRelayerImports[path] {
		return ""
	}
	return "outside the allowed vocabularies (adapters, audit, policy, session, terminal)"
}
