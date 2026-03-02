package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"dread.sh/internal/event"
)

// GitHubProcessor verifies and normalizes GitHub webhook events.
type GitHubProcessor struct {
	Secret string
}

func (p *GitHubProcessor) Process(source string, header http.Header, body []byte) (*event.Event, error) {
	sig := header.Get("X-Hub-Signature-256")
	if sig == "" {
		return nil, fmt.Errorf("missing X-Hub-Signature-256 header")
	}

	if !verifyGitHubSignature(p.Secret, sig, body) {
		return nil, fmt.Errorf("github signature verification failed")
	}

	eventType := header.Get("X-GitHub-Event")
	if eventType == "" {
		eventType = "unknown"
	}

	deliveryID := header.Get("X-GitHub-Delivery")

	summary := fmt.Sprintf("[github] %s", eventType)
	if eventType == "push" {
		summary = summarizeGitHubPush(body, summary)
	}

	return &event.Event{
		ID:      deliveryID,
		Source:  source,
		Type:    eventType,
		Summary: summary,
		RawJSON: string(body),
	}, nil
}

func verifyGitHubSignature(secret, signature string, body []byte) bool {
	sig := strings.TrimPrefix(signature, "sha256=")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(sig), []byte(expected))
}

func summarizeGitHubPush(body []byte, fallback string) string {
	var payload struct {
		Ref  string `json:"ref"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Commits []struct {
			Message string `json:"message"`
		} `json:"commits"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return fallback
	}
	branch := strings.TrimPrefix(payload.Ref, "refs/heads/")
	msg := ""
	if len(payload.Commits) > 0 {
		msg = payload.Commits[len(payload.Commits)-1].Message
		if i := strings.IndexByte(msg, '\n'); i > 0 {
			msg = msg[:i]
		}
	}
	n := len(payload.Commits)
	return fmt.Sprintf("[github] push %s/%s — %d commit(s): %s", payload.Repo.FullName, branch, n, msg)
}
