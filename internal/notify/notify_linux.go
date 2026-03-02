//go:build linux

package notify

import "os/exec"

func send(title, body string) {
	exec.Command("notify-send", title, body).Run()
}
