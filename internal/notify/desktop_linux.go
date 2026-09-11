//go:build linux || freebsd || openbsd || netbsd

package notify

import (
	"context"
	"os/exec"
	"time"
)

func showDesktopNotification(title, body string) {
	notifySend, err := exec.LookPath("notify-send")
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, notifySend, "--app-name=Relayer", title, body)
	_ = cmd.Run()
}
