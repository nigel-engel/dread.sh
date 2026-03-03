//go:build darwin

package notify

import (
	"os/exec"
	"strings"
)

func send(title, body, sound string) {
	// Escape double quotes for AppleScript
	title = strings.ReplaceAll(title, `"`, `\"`)
	body = strings.ReplaceAll(body, `"`, `\"`)
	sound = strings.ReplaceAll(sound, `"`, `\"`)
	script := `display notification "` + body + `" with title "` + title + `" sound name "` + sound + `"`
	exec.Command("osascript", "-e", script).Run()
}
