//go:build windows

package notify

import "os/exec"

func send(title, body, sound string) {
	script := `
[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] | Out-Null
[Windows.Data.Xml.Dom.XmlDocument, Windows.Data.Xml.Dom, ContentType = WindowsRuntime] | Out-Null
$xml = New-Object Windows.Data.Xml.Dom.XmlDocument
$xml.LoadXml('<toast><visual><binding template="ToastText02"><text id="1">' + $env:DREAD_TITLE + '</text><text id="2">' + $env:DREAD_BODY + '</text></binding></visual><audio default="true"/></toast>')
$toast = [Windows.UI.Notifications.ToastNotification]::new($xml)
[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier('dread.sh').Show($toast)
`
	cmd := exec.Command("powershell", "-NoProfile", "-Command", script)
	cmd.Env = append(cmd.Environ(), "DREAD_TITLE="+title, "DREAD_BODY="+body)
	cmd.Run()
}
