package notify

import "strings"

// MergeWebhookHeaders returns edited with each webhook's headers carried over
// from existing when the edit did not supply any.
//
// A settings editor never receives header values — they are usually an
// Authorization credential — so every webhook it sends back has none. Saving
// the form used to replace the whole notifications block with that, erasing
// every header on every save; on the desktop the notifier was rebuilt at once,
// so the next alert went out unauthenticated.
//
// Headers follow a webhook by its URL and name. Webhooks that share both are
// paired in order when there are as many of them before the save as after it,
// which covers unnamed webhooks; otherwise none of them is given any. A
// webhook is matched by its URL alone, as a rename, only when that URL has a
// single webhook both before and after the save. Anything else is ambiguous
// and carries nothing: a webhook added at an existing URL, or a sibling there
// without headers, never takes another's credential, and a webhook whose URL
// changed does not take the old endpoint's with it.
func MergeWebhookHeaders(existing, edited []WebhookConfig) []WebhookConfig {
	if len(edited) == 0 {
		return edited
	}
	pairKey := func(webhook WebhookConfig) string {
		return strings.TrimSpace(webhook.URL) + "\x00" + strings.TrimSpace(webhook.Name)
	}

	existingPairs := make(map[string][]map[string]string, len(existing))
	existingURLs := make(map[string][]map[string]string, len(existing))
	for _, webhook := range existing {
		existingPairs[pairKey(webhook)] = append(existingPairs[pairKey(webhook)], webhook.Headers)
		url := strings.TrimSpace(webhook.URL)
		existingURLs[url] = append(existingURLs[url], webhook.Headers)
	}
	editedPairs := make(map[string]int, len(edited))
	editedURLs := make(map[string]int, len(edited))
	for _, webhook := range edited {
		editedPairs[pairKey(webhook)]++
		editedURLs[strings.TrimSpace(webhook.URL)]++
	}

	merged := make([]WebhookConfig, len(edited))
	seen := make(map[string]int, len(edited))
	for index, webhook := range edited {
		merged[index] = webhook
		key := pairKey(webhook)
		position := seen[key]
		seen[key]++
		if len(webhook.Headers) > 0 {
			continue
		}

		var headers map[string]string
		if candidates := existingPairs[key]; len(candidates) > 0 {
			if len(candidates) == editedPairs[key] {
				headers = candidates[position]
			}
		} else if url := strings.TrimSpace(webhook.URL); len(existingURLs[url]) == 1 && editedURLs[url] == 1 {
			headers = existingURLs[url][0]
		}
		if len(headers) > 0 {
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
