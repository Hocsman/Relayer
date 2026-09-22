package notify

import "strings"

// MergeWebhookHeaders returns edited with each webhook's headers carried over
// from existing when the edit did not supply any.
//
// A settings editor never receives header values — they are usually an
// Authorization credential — so every webhook it sends back has none. Saving
// the form used to replace the whole notifications block with that, erasing
// every header on every save; on the desktop the notifier was rebuilt at once,
// so the next alert went out unauthenticated. Headers follow a webhook by its
// URL and name, or by its URL alone when that is unambiguous. A webhook whose
// URL changed does not take the old endpoint's credential with it.
func MergeWebhookHeaders(existing, edited []WebhookConfig) []WebhookConfig {
	if len(edited) == 0 {
		return edited
	}
	byURLAndName := make(map[string]map[string]string, len(existing))
	byURL := make(map[string]map[string]string, len(existing))
	urlCount := make(map[string]int, len(existing))
	for _, webhook := range existing {
		url := strings.TrimSpace(webhook.URL)
		// Counted whether or not the webhook has headers: a URL shared by a
		// webhook with a credential and one without is ambiguous, and taking
		// the lone credential for both copied it onto the sibling that never
		// had one.
		urlCount[url]++
		if len(webhook.Headers) == 0 {
			continue
		}
		byURLAndName[url+"\x00"+strings.TrimSpace(webhook.Name)] = webhook.Headers
		byURL[url] = webhook.Headers
	}

	merged := make([]WebhookConfig, len(edited))
	for index, webhook := range edited {
		merged[index] = webhook
		if len(webhook.Headers) > 0 {
			continue
		}
		url := strings.TrimSpace(webhook.URL)
		headers, found := byURLAndName[url+"\x00"+strings.TrimSpace(webhook.Name)]
		if !found && urlCount[url] == 1 {
			headers, found = byURL[url]
		}
		if found {
			merged[index].Headers = cloneHeaders(headers)
		}
	}
	return merged
}

func cloneHeaders(headers map[string]string) map[string]string {
	cloned := make(map[string]string, len(headers))
	for name, value := range headers {
		cloned[name] = value
	}
	return cloned
}
