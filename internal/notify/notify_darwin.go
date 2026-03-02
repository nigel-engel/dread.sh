//go:build darwin

package notify

import (
	"os/exec"
	"strings"
)

func send(title, body string) {
	// Escape double quotes for AppleScript
	title = strings.ReplaceAll(title, `"`, `\"`)
	body = strings.ReplaceAll(body, `"`, `\"`)
	script := `display notification "` + body + `" with title "` + title + `" sound name "Funk"`
	exec.Command("osascript", "-e", script).Run()
}
