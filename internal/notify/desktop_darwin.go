//go:build darwin

package notify

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

func escapeAppleScriptString(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\\", "\\\\"), "\"", "\\\"")
}

func showDesktopNotification(title, body string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	script := fmt.Sprintf(`display notification "%s" with title "%s"`, escapeAppleScriptString(body), escapeAppleScriptString(title))
	cmd := exec.CommandContext(ctx, "osascript", "-e", script)
	_ = cmd.Run()
}
