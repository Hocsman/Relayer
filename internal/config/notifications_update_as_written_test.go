package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/notify"
	"github.com/Hocsman/Relayer/internal/policy"
)

// handWrittenNotificationsDocument has a notifications block a person wrote:
// comments, fields left to their defaults, a quoted URL, and a webhook's
// headers in flow style.
const handWrittenNotificationsDocument = `# Relayer configuration (hand-written)
version: 1
backend: pty
policies:
  dry_run: false # rehearse first
notifications:
  # who hears about a prompt
  enabled: true # master switch
  desktop: false
  webhooks:
    - name: ops # the on-call channel
      url: https://hooks.example.test/ops
      format: slack
      headers: {Authorization: Bearer ops-token}
    - url: 'https://hooks.example.test/audit' # unnamed
      min_severity: critical
agents:
  - id: reviewer
    name: Reviewer
    command: [reviewer]
intercept_patterns:
  - pattern: (?i)\[[yn]/[yn]\]
    description: yes/no confirmation
`

func writeHandWrittenNotifications(t *testing.T) (string, Result) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(handWrittenNotificationsDocument), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadExisting(path)
	if err != nil {
		t.Fatalf("LoadExisting: %v", err)
	}
	return path, loaded
}

// editorNotifications is what the desktop's settings editor sends back for
// loaded, before the save carries the headers over: every default a webhook
// leaves out filled in, and no header values, which an editor never receives.
func editorNotifications(loaded notify.Config) notify.Config {
	sent := notify.Config{
		Enabled:     loaded.Enabled,
		Bell:        loaded.Bell,
		Desktop:     loaded.Desktop,
		MinSeverity: loaded.MinSeverity,
	}
	if sent.MinSeverity == "" {
		sent.MinSeverity = notify.SeverityInfo
	}
	for _, webhook := range loaded.Webhooks {
		format := strings.ToLower(strings.TrimSpace(webhook.Format))
		if format == "" {
			format = string(notify.FormatGeneric)
		}
		severity := strings.ToLower(strings.TrimSpace(webhook.MinSeverity))
		if severity == "" {
			severity = notify.SeverityWarning
		}
		timeout := strings.TrimSpace(webhook.Timeout)
		if timeout == "" {
			timeout = "5s"
		}
		sent.Webhooks = append(sent.Webhooks, notify.WebhookConfig{
			Name:        strings.TrimSpace(webhook.Name),
			URL:         strings.TrimSpace(webhook.URL),
			Format:      format,
			MinSeverity: severity,
			Timeout:     timeout,
		})
	}
	return sent
}

// notificationsSave saves the notifications as both front ends do: the
// editor's form, changed by change, with the headers carried over.
func notificationsSave(t *testing.T, path string, loaded Result, change func(*notify.Config)) Result {
	t.Helper()
	requested := editorNotifications(loaded.Notifications)
	change(&requested)
	requested.Webhooks = notify.MergeWebhookHeaders(loaded.Notifications.Webhooks, requested.Webhooks)
	updated, _, err := UpdateFullConfiguration(path, loaded.Revision, FullConfigurationUpdate{Notifications: &requested})
	if err != nil {
		t.Fatalf("UpdateFullConfiguration: %v", err)
	}
	// The editor shows the saved notifications as it sent them.
	shown := editorNotifications(updated.Notifications)
	shown.Webhooks = notify.MergeWebhookHeaders(updated.Notifications.Webhooks, shown.Webhooks)
	if !reflect.DeepEqual(shown, requested) {
		t.Fatalf("the saved notifications load back as\n %+v\nwant\n %+v", shown, requested)
	}
	return updated
}

// A notifications save that changes nothing writes nothing, whether it sends
// the notifications as loaded or as the editor fills them in: a webhook's
// format, severity and timeout left to their defaults are the defaults the
// editor sends. The block was rebuilt on every save: its comments went, the
// headers' flow style too, and every field left out was written, a webhook's
// empty name, severity and timeout included, so that each save changed the
// revision every open editor holds.
func TestANotificationsSaveThatChangesNothingWritesNothing(t *testing.T) {
	for _, test := range []struct {
		name      string
		requested func(notify.Config) notify.Config
	}{
		{"as loaded", func(loaded notify.Config) notify.Config { return loaded }},
		{"as the editor sends them", func(loaded notify.Config) notify.Config {
			sent := editorNotifications(loaded)
			sent.Webhooks = notify.MergeWebhookHeaders(loaded.Webhooks, sent.Webhooks)
			return sent
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path, loaded := writeHandWrittenNotifications(t)
			requested := test.requested(loaded.Notifications)
			updated, revision, err := UpdateFullConfiguration(path, loaded.Revision, FullConfigurationUpdate{Notifications: &requested})
			if err != nil {
				t.Fatalf("UpdateFullConfiguration: %v", err)
			}
			if got := readText(t, path); got != handWrittenNotificationsDocument || revision != loaded.Revision || updated.Revision != loaded.Revision {
				t.Fatalf("an unchanged notifications save rewrote the file:\n%s", got)
			}
		})
	}
}

// A save that sends every tab, as a client of the gateway's API may, and
// changes only the security tab changes the policy's line and leaves the
// notifications block as written.
func TestASettingsSaveKeepsTheNotificationsItDidNotChange(t *testing.T) {
	path, loaded := writeHandWrittenNotifications(t)
	settings := policy.SettingsFrom(loaded.Policies)
	settings.DryRun = true
	policies, err := policy.ApplySettings(loaded.Policies, settings, filepath.Dir(path))
	if err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}
	notifications := editorNotifications(loaded.Notifications)
	notifications.Webhooks = notify.MergeWebhookHeaders(loaded.Notifications.Webhooks, notifications.Webhooks)
	if _, _, err := UpdateFullConfiguration(path, loaded.Revision, FullConfigurationUpdate{
		Agents:        loaded.Agents,
		UpdateAgents:  true,
		Policies:      &policies,
		PolicyPreset:  settings.Profile,
		Notifications: &notifications,
	}); err != nil {
		t.Fatalf("UpdateFullConfiguration: %v", err)
	}
	want := strings.Replace(handWrittenNotificationsDocument, "  dry_run: false # rehearse first\n", "  dry_run: true # rehearse first\n", 1)
	if got := readText(t, path); got != want {
		t.Fatalf("the settings save rewrote more than dry_run:\n%s\nwant:\n%s", got, want)
	}
}

// A notifications save changes the fields it changes and nothing else: a
// field is written where it is, or added to its section, a webhook the save
// did not change keeps its entry as written, and a changed one keeps every
// field it did not change, its comments and its headers' flow style.
func TestANotificationsSaveChangesOnlyWhatItChanged(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*notify.Config)
		// replace gives the lines the save changes: old, new, and so on.
		replace []string
	}{
		{
			name:    "desktop turned on",
			change:  func(notifications *notify.Config) { notifications.Desktop = true },
			replace: []string{"  desktop: false\n", "  desktop: true\n"},
		},
		{
			name:    "notifications turned off",
			change:  func(notifications *notify.Config) { notifications.Enabled = false },
			replace: []string{"  enabled: true # master switch\n", "  enabled: false # master switch\n"},
		},
		{
			name:   "bell turned off",
			change: func(notifications *notify.Config) { notifications.Bell = false },
			replace: []string{"      min_severity: critical\nagents:\n",
				"      min_severity: critical\n  bell: false\nagents:\n"},
		},
		{
			name:   "minimum severity raised",
			change: func(notifications *notify.Config) { notifications.MinSeverity = notify.SeverityWarning },
			replace: []string{"      min_severity: critical\nagents:\n",
				"      min_severity: critical\n  min_severity: warning\nagents:\n"},
		},
		{
			name:   "a webhook's severity changed",
			change: func(notifications *notify.Config) { notifications.Webhooks[0].MinSeverity = notify.SeverityCritical },
			replace: []string{"      headers: {Authorization: Bearer ops-token}\n",
				"      headers: {Authorization: Bearer ops-token}\n      min_severity: critical\n"},
		},
		{
			name:   "a webhook's timeout set",
			change: func(notifications *notify.Config) { notifications.Webhooks[1].Timeout = "10s" },
			replace: []string{"      min_severity: critical\nagents:\n",
				"      min_severity: critical\n      timeout: 10s\nagents:\n"},
		},
		{
			name:    "a webhook's format changed",
			change:  func(notifications *notify.Config) { notifications.Webhooks[0].Format = string(notify.FormatDiscord) },
			replace: []string{"      format: slack\n", "      format: discord\n"},
		},
		{
			name:    "a webhook renamed",
			change:  func(notifications *notify.Config) { notifications.Webhooks[0].Name = "on-call" },
			replace: []string{"    - name: ops # the on-call channel\n", "    - name: on-call # the on-call channel\n"},
		},
		{
			name: "a webhook added",
			change: func(notifications *notify.Config) {
				notifications.Webhooks = append(notifications.Webhooks, notify.WebhookConfig{
					Name:        "pager",
					URL:         "https://hooks.example.test/pager",
					Format:      string(notify.FormatDiscord),
					MinSeverity: notify.SeverityCritical,
					Timeout:     "5s",
				})
			},
			replace: []string{"      min_severity: critical\nagents:\n", `      min_severity: critical
    - name: pager
      url: https://hooks.example.test/pager
      format: discord
      min_severity: critical
      timeout: 5s
agents:
`},
		},
		{
			name:   "a webhook removed",
			change: func(notifications *notify.Config) { notifications.Webhooks = notifications.Webhooks[1:] },
			replace: []string{`    - name: ops # the on-call channel
      url: https://hooks.example.test/ops
      format: slack
      headers: {Authorization: Bearer ops-token}
`, ""},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			path, loaded := writeHandWrittenNotifications(t)
			notificationsSave(t, path, loaded, test.change)
			want := handWrittenNotificationsDocument
			for index := 0; index+1 < len(test.replace); index += 2 {
				replaced := strings.Replace(want, test.replace[index], test.replace[index+1], 1)
				if replaced == want {
					t.Fatalf("the document has no %q to replace", test.replace[index])
				}
				want = replaced
			}
			if got := readText(t, path); got != want {
				t.Fatalf("the save rewrote more than it changed:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}
