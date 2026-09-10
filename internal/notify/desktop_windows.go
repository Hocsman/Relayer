//go:build windows

package notify

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

func escapePowerShellString(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

func showDesktopNotification(title, body string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	script := fmt.Sprintf(`
[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] > $null
$template = [Windows.UI.Notifications.ToastNotificationManager]::GetTemplateContent([Windows.UI.Notifications.ToastTemplateType]::ToastText02)
$textNodes = $template.GetElementsByTagName("text")
$textNodes.Item(0).AppendChild($template.CreateTextNode('%s')) > $null
$textNodes.Item(1).AppendChild($template.CreateTextNode('%s')) > $null
$notifier = [Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier('Relayer')
$notification = [Windows.UI.Notifications.ToastNotification]::new($template)
$notifier.Show($notification)
`, escapePowerShellString(title), escapePowerShellString(body))

	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-Command", script)
	_ = cmd.Run()
}
