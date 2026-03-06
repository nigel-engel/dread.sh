package selfupdate

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Check queries the server for the latest version and auto-updates if behind.
// Returns true if the binary was replaced (caller should restart).
func Check(serverURL, currentVersion string) bool {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(serverURL + "/api/version")
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	var v struct {
		Latest string `json:"latest"`
	}
	if json.NewDecoder(resp.Body).Decode(&v) != nil || v.Latest == "" {
		return false
	}

	latest := strings.TrimPrefix(v.Latest, "v")
	current := strings.TrimPrefix(currentVersion, "v")
	if latest == current {
		return false
	}

	log.Printf("update available: v%s → v%s, downloading...", current, latest)

	exe, err := os.Executable()
	if err != nil {
		log.Printf("selfupdate: cannot find executable path: %v", err)
		return false
	}

	tarball := fmt.Sprintf("dread_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	url := fmt.Sprintf("https://github.com/nigel-engel/dread.sh/releases/latest/download/%s", tarball)

	tmpDir, err := os.MkdirTemp("", "dread-update-*")
	if err != nil {
		log.Printf("selfupdate: tmpdir: %v", err)
		return false
	}
	defer os.RemoveAll(tmpDir)

	// Download tarball
	dlResp, err := http.Get(url)
	if err != nil || dlResp.StatusCode != 200 {
		log.Printf("selfupdate: download failed: %v", err)
		return false
	}
	defer dlResp.Body.Close()

	tarPath := tmpDir + "/" + tarball
	f, err := os.Create(tarPath)
	if err != nil {
		log.Printf("selfupdate: create: %v", err)
		return false
	}
	if _, err := io.Copy(f, dlResp.Body); err != nil {
		f.Close()
		log.Printf("selfupdate: download write: %v", err)
		return false
	}
	f.Close()

	// Extract
	cmd := exec.Command("tar", "-xzf", tarPath, "-C", tmpDir)
	if err := cmd.Run(); err != nil {
		log.Printf("selfupdate: extract: %v", err)
		return false
	}

	newBin := tmpDir + "/dread"
	if _, err := os.Stat(newBin); err != nil {
		log.Printf("selfupdate: binary not found in archive: %v", err)
		return false
	}

	// Replace: remove old, move new
	os.Remove(exe)
	cmd = exec.Command("cp", newBin, exe)
	if err := cmd.Run(); err != nil {
		log.Printf("selfupdate: replace failed: %v", err)
		return false
	}
	os.Chmod(exe, 0755)

	log.Printf("updated to v%s — restarting", latest)
	return true
}
