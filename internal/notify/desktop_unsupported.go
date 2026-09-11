//go:build !windows && !darwin && !linux && !freebsd && !openbsd && !netbsd

package notify

func showDesktopNotification(title, body string) {}
