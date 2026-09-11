//go:build linux || freebsd || openbsd || netbsd

package notify

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

func showDesktopNotification(title, body string) {
	notifySend, err := exec.LookPath("notify-send")
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	args := []string{"--app-name=Relayer"}
	if strings.Contains(title, "CRITICAL") || strings.Contains(title, "Guardrail") {
		args = append(args, "--urgency=critical", "--icon=security-high")
	} else {
		args = append(args, "--urgency=normal", "--icon=dialog-information")
	}
	args = append(args, title, body)

	cmd := exec.CommandContext(ctx, notifySend, args...)
	_ = cmd.Run()
}
