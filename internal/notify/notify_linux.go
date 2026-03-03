//go:build linux

package notify

import "os/exec"

func send(title, body, sound string) {
	exec.Command("notify-send", "--hint", "string:sound-name:"+sound, title, body).Run()
}
