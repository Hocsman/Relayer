package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/config"
)

// handWrittenDesktopNotifications is a notifications block a person wrote:
// comments, fields left to their defaults, a format in capitals, a quoted URL
// and a webhook's headers in flow style. The desktop sends each webhook back
// with its format and severity in lower case and every default filled in.
const handWrittenDesktopNotifications = `notifications:
  # who hears about a prompt
  enabled: true # master switch
  desktop: false
  webhooks:
    - name: ops # the on-call channel
      url: https://hooks.example.test/ops
      format: Slack
      headers: {Authorization: Bearer ops-token}
    - url: 'https://hooks.example.test/audit' # unnamed
      min_severity: critical
`

func desktopSettingsApp(t *testing.T, document string) (*App, string, FullSettingsView) {
	t.Helper()
	application, path := profileTestApp(t, nil)
	if err := os.Mkdir(filepath.Join(filepath.Dir(path), "ws"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	view, err := application.GetFullSettings()
	if err != nil {
		t.Fatalf("GetFullSettings: %v", err)
	}
	return application, path, view
}

// A notifications save on the desktop changes the lines it changes and
// nothing else, and one that sends the tab back unchanged writes nothing and
// keeps the revision. The block was rebuilt by every save that sent it: its
// comments and the headers' flow style went, and every default the desktop
// fills in was written out.
func TestADesktopNotificationsSaveKeepsTheBlockAsWritten(t *testing.T) {
	document := handWrittenDesktopAgents + handWrittenDesktopNotifications
	for _, test := range []struct {
		name   string
		change func(*NotificationSettings)
		want   string
	}{
		{"nothing changed", func(*NotificationSettings) {}, document},
		{"desktop turned on", func(notifications *NotificationSettings) { notifications.Desktop = true },
			strings.Replace(document, "  desktop: false\n", "  desktop: true\n", 1)},
		{"a webhook removed", func(notifications *NotificationSettings) {
			notifications.Webhooks = notifications.Webhooks[:1]
		}, strings.Replace(document, "    - url: 'https://hooks.example.test/audit' # unnamed\n      min_severity: critical\n", "", 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			application, path, view := desktopSettingsApp(t, document)
			notifications := view.Notifications
			notifications.Webhooks = append([]NotificationWebhookSetting(nil), notifications.Webhooks...)
			test.change(&notifications)
			saved, err := application.SaveFullSettings(activeRunIDForTest(application), SaveFullSettingsRequest{
				ExpectedRevision: view.Revision,
				Notifications:    &notifications,
			})
			if err != nil {
				t.Fatalf("SaveFullSettings: %v", err)
			}
			if after, _ := os.ReadFile(path); string(after) != test.want {
				t.Fatalf("the notifications save rewrote more than it changed:\n%s\nwant:\n%s", after, test.want)
			}
			if unchanged := test.want == document; unchanged != (saved.Revision == view.Revision) {
				t.Fatalf("revision %q after the save, %q before it", saved.Revision, view.Revision)
			}
		})
	}
}

// Switching from strict to permissive on the desktop keeps a workspace root
// the file leaves implicit relative. The editor keeps the root it shows, which
// the save had to name once the guardrail no longer brought it, and wrote as
// an absolute path.
func TestADesktopSwitchFromStrictToPermissiveKeepsTheWorkspaceRootRelative(t *testing.T) {
	document := strings.Replace(handWrittenDesktopAgents, "backend: pty\nagents:\n",
		"backend: pty\npolicies:\n  profile: strict # base\nagents:\n", 1)
	application, path, view := desktopSettingsApp(t, document)
	// The interface's presetSettings: the preset's values, the user's
	// workspace root and dry-run kept.
	security := view.SecurityPresets["permissive"]
	security.Profile = "permissive"
	security.WorkspaceRoot, security.DryRun = view.Security.WorkspaceRoot, view.Security.DryRun
	if _, err := application.SaveFullSettings(activeRunIDForTest(application), SaveFullSettingsRequest{
		ExpectedRevision: view.Revision,
		Security:         &security,
	}); err != nil {
		t.Fatalf("SaveFullSettings: %v", err)
	}
	want := strings.Replace(document, "  profile: strict # base\n",
		"  profile: permissive # base\n  guardrails:\n    workspace_root: .\n", 1)
	if after, _ := os.ReadFile(path); string(after) != want {
		t.Fatalf("the switch to permissive did not keep the root relative:\n%s\nwant:\n%s", after, want)
	}
	loaded, err := config.LoadExisting(path)
	if err != nil {
		t.Fatalf("LoadExisting: %v", err)
	}
	if got, want := loaded.Policies.Guardrails.WorkspaceRoot, filepath.Dir(path); got != want {
		t.Fatalf("workspace root = %q, want the configuration's directory %q", got, want)
	}
}

// The Windows trial on the desktop, in sequence: a hand-written configuration
// with Windows line endings and a byte-order mark is switched from strict to
// permissive as the editor switches it, then its notifications tab is sent
// back as the editor shows it after that save. The switch writes
// "workspace_root: ." and changes nothing else, the line endings and the mark
// included, and the second save writes nothing and keeps the revision the
// first one returned.
func TestADesktopSwitchToPermissiveThenAnUnchangedNotificationsSaveKeepAWindowsFileAsWritten(t *testing.T) {
	const mark = "\xef\xbb\xbf"
	windows := func(text string) string { return mark + strings.ReplaceAll(text, "\n", "\r\n") }
	written := strings.Replace(handWrittenDesktopAgents, "backend: pty\nagents:\n",
		"backend: pty\npolicies:\n  profile: strict # base\nagents:\n", 1) + handWrittenDesktopNotifications
	application, path, view := desktopSettingsApp(t, windows(written))
	security := view.SecurityPresets["permissive"]
	security.Profile = "permissive"
	security.WorkspaceRoot, security.DryRun = view.Security.WorkspaceRoot, view.Security.DryRun
	switched, err := application.SaveFullSettings(activeRunIDForTest(application), SaveFullSettingsRequest{
		ExpectedRevision: view.Revision,
		Security:         &security,
	})
	if err != nil {
		t.Fatalf("SaveFullSettings (security): %v", err)
	}
	want := windows(strings.Replace(written, "  profile: strict # base\n",
		"  profile: permissive # base\n  guardrails:\n    workspace_root: .\n", 1))
	after, _ := os.ReadFile(path)
	if string(after) != want {
		t.Fatalf("the switch to permissive rewrote more than the policy:\n%q\nwant:\n%q", after, want)
	}
	loaded, err := config.LoadExisting(path)
	if err != nil {
		t.Fatalf("LoadExisting: %v", err)
	}
	if got, want := loaded.Policies.Guardrails.WorkspaceRoot, filepath.Dir(path); got != want {
		t.Fatalf("workspace root = %q, want the configuration's directory %q", got, want)
	}
	notifications := switched.Notifications
	saved, err := application.SaveFullSettings(activeRunIDForTest(application), SaveFullSettingsRequest{
		ExpectedRevision: switched.Revision,
		Notifications:    &notifications,
	})
	if err != nil {
		t.Fatalf("SaveFullSettings (notifications): %v", err)
	}
	if again, _ := os.ReadFile(path); string(again) != string(after) {
		t.Fatalf("the unchanged notifications save rewrote the file:\n%q\nwant:\n%q", again, after)
	}
	if saved.Revision != switched.Revision {
		t.Fatalf("revision %q after the notifications save, %q before it", saved.Revision, switched.Revision)
	}
}
