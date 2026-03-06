//go:build darwin

package notify

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func send(title, body, sound string) {
	// Prefer Dread.app (custom notifier with D_ icon)
	home, _ := os.UserHomeDir()
	if home != "" {
		dreadBin := filepath.Join(home, ".config", "dread", "Dread.app", "Contents", "MacOS", "Dread")
		if _, err := os.Stat(dreadBin); err == nil {
			exec.Command(dreadBin, "-title", title, "-message", body, "-sound", sound).Run()
			return
		}
	}

	// Fallback to osascript
	title = strings.ReplaceAll(title, `"`, `\"`)
	body = strings.ReplaceAll(body, `"`, `\"`)
	sound = strings.ReplaceAll(sound, `"`, `\"`)
	script := `display notification "` + body + `" with title "` + title + `" sound name "` + sound + `"`
	exec.Command("osascript", "-e", script).Run()
}
