package server

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/config"
)

// handWrittenWebNotifications is a notifications block a person wrote:
// comments, fields left to their defaults, a format in capitals, a quoted URL
// and a webhook's headers in flow style.
const handWrittenWebNotifications = `notifications:
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

// A notifications save from the web interface changes the lines it changes
// and nothing else, and one that sends the tab back unchanged writes nothing
// and keeps the revision. The block was rebuilt by every save that sent it:
// its comments and the headers' flow style went, and every field the file
// left to its default was written out.
func TestAWebNotificationsSaveKeepsTheBlockAsWritten(t *testing.T) {
	const written = "notifications:\n  enabled: true\n  bell: false\n  desktop: false\n  min_severity: info\n"
	document := strings.Replace(handWrittenAgents, written, handWrittenWebNotifications, 1)
	if document == handWrittenAgents {
		t.Fatal("the document has no notifications block to replace")
	}
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
			ctrl, configPath := handWrittenController(t, document)
			view, err := ctrl.GetFullSettings()
			if err != nil {
				t.Fatalf("GetFullSettings: %v", err)
			}
			notifications := view.Notifications
			notifications.Webhooks = append([]NotificationWebhookSetting(nil), notifications.Webhooks...)
			test.change(&notifications)
			saved, err := ctrl.SaveFullSettings("", decodedAsTheGatewayDoes[SaveFullSettingsRequest](t, map[string]any{
				"expectedRevision": view.Revision,
				"notifications":    notifications,
			}))
			if err != nil {
				t.Fatalf("SaveFullSettings: %v", err)
			}
			if after, _ := os.ReadFile(configPath); string(after) != test.want {
				t.Fatalf("the notifications save rewrote more than it changed:\n%s\nwant:\n%s", after, test.want)
			}
			if unchanged := test.want == document; unchanged != (saved.Revision == view.Revision) {
				t.Fatalf("revision %q after the save, %q before it", saved.Revision, view.Revision)
			}
		})
	}
}

// Switching from strict to permissive in the web interface keeps a workspace
// root the file leaves implicit relative, with the configuration addressed
// either way on the command line. The editor keeps the root it shows, which
// the save had to name once the guardrail no longer brought it, and wrote as
// an absolute path.
func TestAWebSwitchFromStrictToPermissiveKeepsTheWorkspaceRootRelative(t *testing.T) {
	policies := handWrittenAgents[strings.Index(handWrittenAgents, "policies:\n"):strings.Index(handWrittenAgents, "agents:\n")]
	document := strings.Replace(handWrittenAgents, policies, "policies:\n  profile: strict # base\n", 1)
	for _, relative := range []bool{false, true} {
		name := "absolute --config"
		if relative {
			name = "relative --config"
		}
		t.Run(name, func(t *testing.T) {
			ctrl, absolutePath := handWrittenController(t, document)
			if relative {
				t.Chdir(filepath.Dir(filepath.Dir(absolutePath)))
				var err error
				if ctrl, err = NewController(filepath.Join("cfg", "config.yaml"), io.Discard); err != nil {
					t.Fatalf("NewController: %v", err)
				}
			}
			view, err := ctrl.GetFullSettings()
			if err != nil {
				t.Fatalf("GetFullSettings: %v", err)
			}
			security := uiPresetSettings(view.Security, view.SecurityPresets, "permissive")
			if _, err := ctrl.SaveFullSettings("", decodedAsTheGatewayDoes[SaveFullSettingsRequest](t, map[string]any{
				"expectedRevision": view.Revision, "security": security,
			})); err != nil {
				t.Fatalf("SaveFullSettings: %v", err)
			}
			want := strings.Replace(document, "  profile: strict # base\n",
				"  profile: permissive # base\n  guardrails:\n    workspace_root: .\n", 1)
			if after, _ := os.ReadFile(absolutePath); string(after) != want {
				t.Fatalf("the switch to permissive did not keep the root relative:\n%s\nwant:\n%s", after, want)
			}
			loaded, err := config.LoadExisting(absolutePath)
			if err != nil {
				t.Fatalf("LoadExisting: %v", err)
			}
			if got, want := loaded.Policies.Guardrails.WorkspaceRoot, filepath.Dir(absolutePath); got != want {
				t.Fatalf("workspace root = %q, want the configuration's directory %q", got, want)
			}
		})
	}
}

// The Windows trial in the web interface, in sequence: a hand-written
// configuration with Windows line endings and a byte-order mark is switched
// from strict to permissive as the editor switches it, then its notifications
// tab is sent back as the editor shows it after that save. The switch writes
// "workspace_root: ." and changes nothing else, the line endings and the mark
// included, and the second save writes nothing and keeps the revision the
// first one returned.
func TestAWebSwitchToPermissiveThenAnUnchangedNotificationsSaveKeepAWindowsFileAsWritten(t *testing.T) {
	const mark = "\xef\xbb\xbf"
	windows := func(text string) string { return mark + strings.ReplaceAll(text, "\n", "\r\n") }
	policies := handWrittenAgents[strings.Index(handWrittenAgents, "policies:\n"):strings.Index(handWrittenAgents, "agents:\n")]
	const notifications = "notifications:\n  enabled: true\n  bell: false\n  desktop: false\n  min_severity: info\n"
	written := strings.Replace(strings.Replace(handWrittenAgents, policies, "policies:\n  profile: strict # base\n", 1),
		notifications, handWrittenWebNotifications, 1)
	if !strings.Contains(written, handWrittenWebNotifications) {
		t.Fatal("the document has no notifications block to replace")
	}
	ctrl, configPath := handWrittenController(t, windows(written))
	view, err := ctrl.GetFullSettings()
	if err != nil {
		t.Fatalf("GetFullSettings: %v", err)
	}
	switched, err := ctrl.SaveFullSettings("", decodedAsTheGatewayDoes[SaveFullSettingsRequest](t, map[string]any{
		"expectedRevision": view.Revision,
		"security":         uiPresetSettings(view.Security, view.SecurityPresets, "permissive"),
	}))
	if err != nil {
		t.Fatalf("SaveFullSettings (security): %v", err)
	}
	want := windows(strings.Replace(written, "  profile: strict # base\n",
		"  profile: permissive # base\n  guardrails:\n    workspace_root: .\n", 1))
	after, _ := os.ReadFile(configPath)
	if string(after) != want {
		t.Fatalf("the switch to permissive rewrote more than the policy:\n%q\nwant:\n%q", after, want)
	}
	loaded, err := config.LoadExisting(configPath)
	if err != nil {
		t.Fatalf("LoadExisting: %v", err)
	}
	if got, want := loaded.Policies.Guardrails.WorkspaceRoot, filepath.Dir(configPath); got != want {
		t.Fatalf("workspace root = %q, want the configuration's directory %q", got, want)
	}
	saved, err := ctrl.SaveFullSettings("", decodedAsTheGatewayDoes[SaveFullSettingsRequest](t, map[string]any{
		"expectedRevision": switched.Revision,
		"notifications":    switched.Notifications,
	}))
	if err != nil {
		t.Fatalf("SaveFullSettings (notifications): %v", err)
	}
	if again, _ := os.ReadFile(configPath); string(again) != string(after) {
		t.Fatalf("the unchanged notifications save rewrote the file:\n%q\nwant:\n%q", again, after)
	}
	if saved.Revision != switched.Revision {
		t.Fatalf("revision %q after the notifications save, %q before it", saved.Revision, switched.Revision)
	}
}
