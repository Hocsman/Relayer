package config

import (
	"strconv"
	"strings"

	"github.com/Hocsman/Relayer/internal/notify"
	"gopkg.in/yaml.v3"
)

// sameNotifications reports whether two notification settings behave the
// same. A value the notifier reads as its default is that default: a webhook
// with no format, severity or timeout is the generic, "warning", "5s" one the
// desktop's editor sends back for it, and no minimum severity is "info".
// Severities and formats are read without case or surrounding spaces, as the
// notifier reads them. An absent and an empty list of webhooks or headers are
// the same.
func sameNotifications(left, right notify.Config) bool {
	return left.Enabled == right.Enabled && left.Bell == right.Bell && left.Desktop == right.Desktop &&
		behavingMinimumSeverity(left.MinSeverity) == behavingMinimumSeverity(right.MinSeverity) &&
		sameWebhooks(left.Webhooks, right.Webhooks)
}

func sameWebhooks(left, right []notify.WebhookConfig) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !sameWebhook(left[index], right[index]) {
			return false
		}
	}
	return true
}

func sameWebhook(left, right notify.WebhookConfig) bool {
	return left.Name == right.Name && left.URL == right.URL &&
		behavingWebhookFormat(left.Format) == behavingWebhookFormat(right.Format) &&
		behavingWebhookSeverity(left.MinSeverity) == behavingWebhookSeverity(right.MinSeverity) &&
		behavingWebhookTimeout(left.Timeout) == behavingWebhookTimeout(right.Timeout) &&
		sameEnvironment(left.Headers, right.Headers)
}

// behavingMinimumSeverity is the severity notifications are sent from, an
// empty one being "info". One of spaces alone lets every notification
// through too, but the loader refuses it once notifications are on, so it is
// not the default: a save that turns them on writes the severity asked for.
func behavingMinimumSeverity(severity string) string {
	if severity == "" {
		return notify.SeverityInfo
	}
	return strings.ToLower(strings.TrimSpace(severity))
}

func behavingWebhookFormat(format string) string {
	if normalized := strings.ToLower(strings.TrimSpace(format)); normalized != "" {
		return normalized
	}
	return string(notify.FormatGeneric)
}

// behavingWebhookSeverity is the severity a webhook is sent from. The
// notifier gives only an empty one the default; one of spaces alone lets
// every notification through, so it is not the default.
func behavingWebhookSeverity(severity string) string {
	if severity == "" {
		return notify.SeverityWarning
	}
	return strings.ToLower(strings.TrimSpace(severity))
}

// behavingWebhookTimeout is a webhook's timeout as written, the empty one
// being the notifier's default. Any other text is compared as it is.
func behavingWebhookTimeout(timeout string) string {
	if timeout == "" {
		return "5s"
	}
	return timeout
}

// notificationsNode returns the notifications block to write for requested,
// given the block the file has, which loads as current.
//
// The block used to be rebuilt from the requested settings by every save
// that sent them, changed or not: its comments went, a flow mapping of
// headers became a block, and every field the file left to its default was
// written out, down to a webhook's empty name, severity and timeout. The
// block is now edited, as the agents are: a field that behaves as requested
// keeps its text, and each one that does not is written where it is, or
// added to its section. A block the file does not have is written whole, as
// before.
func notificationsNode(existing *yaml.Node, current, requested notify.Config) (*yaml.Node, error) {
	if existing == nil || existing.Kind != yaml.MappingNode {
		return yamlNodeFrom(configuredNotificationsPointer(requested))
	}
	node := copyNode(existing)
	for _, flag := range []struct {
		key       string
		was, want bool
	}{
		{"enabled", current.Enabled, requested.Enabled},
		{"bell", current.Bell, requested.Bell},
		{"desktop", current.Desktop, requested.Desktop},
	} {
		if flag.was != flag.want {
			setTypedField(node, flag.key, "!!bool", strconv.FormatBool(flag.want))
		}
	}
	if behavingMinimumSeverity(current.MinSeverity) != behavingMinimumSeverity(requested.MinSeverity) {
		setStringFieldOrRemove(node, "min_severity", requested.MinSeverity)
	}
	if !sameWebhooks(current.Webhooks, requested.Webhooks) {
		if len(requested.Webhooks) == 0 {
			removeField(node, "webhooks")
		} else {
			setNodeField(node, "webhooks", webhookSequenceNode(mappingValue(node, "webhooks"), current.Webhooks, requested.Webhooks))
		}
	}
	return node, nil
}

// webhookSequenceNode returns the webhooks sequence to write for requested.
// previous is the file's sequence, whose entries loaded, in order, as
// loaded.
//
// A requested webhook that behaves as one the file has keeps that entry as
// written. One that does not is written into the entry of the same webhook,
// keeping every field it did not change: the entry at the same URL under the
// same name, else, as a rename, the one entry at its URL when the save has
// only it there too, the rule notify.MergeWebhookHeaders carries headers by.
// Any other webhook is a new entry, and takes nothing from an entry it
// replaces, its comments included.
func webhookSequenceNode(previous *yaml.Node, loaded, requested []notify.WebhookConfig) *yaml.Node {
	sequence := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	var entries []*yaml.Node
	if previous != nil && previous.Kind == yaml.SequenceNode {
		updated := *previous
		sequence = &updated
		// An entry is paired with the webhook it loaded as only when the
		// loader read every entry of the sequence.
		if len(previous.Content) == len(loaded) {
			entries = previous.Content
		}
	}
	for _, entry := range entries {
		if entry.Kind != yaml.MappingNode {
			entries = nil
			break
		}
	}

	written := make([]*yaml.Node, len(requested))
	used := make([]bool, len(entries))
	pair := func(matches func(requestedIndex, entryIndex int) bool, write func(requestedIndex, entryIndex int) *yaml.Node) {
		for requestedIndex := range requested {
			if written[requestedIndex] != nil {
				continue
			}
			for entryIndex := range entries {
				if !used[entryIndex] && matches(requestedIndex, entryIndex) {
					used[entryIndex] = true
					written[requestedIndex] = write(requestedIndex, entryIndex)
					break
				}
			}
		}
	}
	asWritten := func(_, entryIndex int) *yaml.Node { return entries[entryIndex] }
	changed := func(requestedIndex, entryIndex int) *yaml.Node {
		return changedWebhookEntry(entries[entryIndex], loaded[entryIndex], requested[requestedIndex])
	}
	pair(func(requestedIndex, entryIndex int) bool {
		return sameWebhook(loaded[entryIndex], requested[requestedIndex])
	}, asWritten)
	pair(func(requestedIndex, entryIndex int) bool {
		return webhookURL(loaded[entryIndex]) == webhookURL(requested[requestedIndex]) &&
			strings.TrimSpace(loaded[entryIndex].Name) == strings.TrimSpace(requested[requestedIndex].Name)
	}, changed)
	inFile, inSave := make(map[string]int, len(loaded)), make(map[string]int, len(requested))
	for _, webhook := range loaded {
		inFile[webhookURL(webhook)]++
	}
	for _, webhook := range requested {
		inSave[webhookURL(webhook)]++
	}
	pair(func(requestedIndex, entryIndex int) bool {
		url := webhookURL(requested[requestedIndex])
		return webhookURL(loaded[entryIndex]) == url && inFile[url] == 1 && inSave[url] == 1
	}, changed)

	sequence.Content = make([]*yaml.Node, len(requested))
	for index, webhook := range requested {
		if written[index] == nil {
			written[index] = newWebhookEntry(webhook)
		}
		sequence.Content[index] = written[index]
	}
	return sequence
}

func webhookURL(webhook notify.WebhookConfig) string {
	return strings.TrimSpace(webhook.URL)
}

// newWebhookEntry builds the entry of a webhook the file does not have. A
// field left empty is left out, which loads the same.
func newWebhookEntry(webhook notify.WebhookConfig) *yaml.Node {
	entry := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, field := range []struct{ key, value string }{
		{"name", webhook.Name},
		{"url", webhook.URL},
		{"format", webhook.Format},
		{"min_severity", webhook.MinSeverity},
		{"timeout", webhook.Timeout},
	} {
		if field.value != "" {
			appendStringField(entry, field.key, field.value)
		}
	}
	if len(webhook.Headers) > 0 {
		appendNodeField(entry, "headers", environmentNode(webhook.Headers))
	}
	return entry
}

// changedWebhookEntry is a copy of entry, whose webhook the file gives as
// was, with each field that behaves differently in webhook written in place.
// A field that did not change keeps its node, its text and its comments; a
// header that did not change keeps its line.
func changedWebhookEntry(entry *yaml.Node, was, webhook notify.WebhookConfig) *yaml.Node {
	updated := *entry
	updated.Content = append([]*yaml.Node(nil), entry.Content...)
	if was.Name != webhook.Name {
		setStringFieldOrRemove(&updated, "name", webhook.Name)
	}
	if was.URL != webhook.URL {
		setStringFieldOrRemove(&updated, "url", webhook.URL)
	}
	if behavingWebhookFormat(was.Format) != behavingWebhookFormat(webhook.Format) {
		setStringFieldOrRemove(&updated, "format", webhook.Format)
	}
	if behavingWebhookSeverity(was.MinSeverity) != behavingWebhookSeverity(webhook.MinSeverity) {
		setStringFieldOrRemove(&updated, "min_severity", webhook.MinSeverity)
	}
	if behavingWebhookTimeout(was.Timeout) != behavingWebhookTimeout(webhook.Timeout) {
		setStringFieldOrRemove(&updated, "timeout", webhook.Timeout)
	}
	if !sameEnvironment(was.Headers, webhook.Headers) {
		if len(webhook.Headers) == 0 {
			removeField(&updated, "headers")
		} else {
			// Headers are edited as an agent's environment is.
			setNodeField(&updated, "headers", changedEnvironmentNode(mappingValue(&updated, "headers"), webhook.Headers))
		}
	}
	return &updated
}

// setStringFieldOrRemove writes value under key, keeping the quoting and the
// comments of the value it replaces, or removes the field when value is
// empty: an absent string field of the block behaves as an empty one.
func setStringFieldOrRemove(mapping *yaml.Node, key, value string) {
	if value == "" {
		removeField(mapping, key)
		return
	}
	setTypedField(mapping, key, "!!str", value)
}
