//go:build darwin

package clipboard

import (
	"os/exec"
	"strings"
)

func copy(text string) error {
	cmd := exec.Command("pbcopy")
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run()
}
