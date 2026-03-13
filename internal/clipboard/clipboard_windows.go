//go:build windows

package clipboard

import (
	"os/exec"
	"strings"
)

func copy(text string) error {
	cmd := exec.Command("powershell", "-NoProfile", "-Command", "Set-Clipboard -Value $input")
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run()
}
