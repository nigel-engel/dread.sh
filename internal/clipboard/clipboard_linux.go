//go:build linux

package clipboard

import (
	"os/exec"
	"strings"
)

func copy(text string) error {
	// Try xclip first, fall back to xsel
	if path, err := exec.LookPath("xclip"); err == nil {
		cmd := exec.Command(path, "-selection", "clipboard")
		cmd.Stdin = strings.NewReader(text)
		return cmd.Run()
	}
	cmd := exec.Command("xsel", "--clipboard", "--input")
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run()
}
