package watch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"dread.sh/internal/auth"
	"dread.sh/internal/hub"
	"dread.sh/internal/notify"
	"dread.sh/internal/selfupdate"
	"dread.sh/internal/tui"

	"github.com/coder/websocket"
)

// Run connects to the server via WebSocket and sends desktop notifications
// for incoming events. It reloads the full config on each reconnect so new
// channels, muting, and forwarding settings are picked up automatically.
func Run(serverURL string, filter string, follows []string, slackOverride string, discordOverride string) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	wsURL := strings.Replace(serverURL, "http://", "ws://", 1)
	wsURL = strings.Replace(wsURL, "https://", "wss://", 1)

	alertCounters := &alertState{
		windows: make(map[string][]time.Time),
	}

	for {
		// Auto-update check on each reconnect cycle
		if selfupdate.Check(serverURL, tui.Version) {
			// Binary was replaced — re-exec ourselves
			exe, _ := os.Executable()
			cmd := exec.Command(exe, os.Args[1:]...)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			cmd.Run()
			return nil
		}

		cfg, err := auth.Load()
		if err != nil {
			log.Printf("config error: %v — retrying in 3s", err)
			select {
			case <-time.After(3 * time.Second):
				continue
			case <-ctx.Done():
				return nil
			}
		}

		channels := cfg.Channels
		for _, wsID := range cfg.Follows {
			remote, err := resolveWorkspace(serverURL, wsID)
			if err != nil {
				log.Printf("workspace %s: %v", wsID, err)
				continue
			}
			channels = mergeChannels(channels, remote)
		}

		if len(channels) == 0 {
			log.Printf("no channels configured — retrying in 10s")
			select {
			case <-time.After(10 * time.Second):
				continue
			case <-ctx.Done():
				return nil
			}
		}

		ids := make([]string, len(channels))
		nameByID := make(map[string]string, len(channels))
		for i, ch := range channels {
			ids[i] = ch.ID
			nameByID[ch.ID] = ch.Name
		}

		slackURL := cfg.SlackURL
		if slackOverride != "" {
			slackURL = slackOverride
		}
		discordURL := cfg.DiscordURL
		if discordOverride != "" {
			discordURL = discordOverride
		}

		endpoint := wsURL + "/ws?channels=" + strings.Join(ids, ",")
		fmt.Printf("watching %d channel(s)...\n", len(channels))

		err = listen(ctx, endpoint, nameByID, filter, cfg.Sound, cfg.Muted, slackURL, discordURL, cfg.Alerts, alertCounters)
		if ctx.Err() != nil {
			fmt.Println("\nshutting down")
			return nil
		}
		log.Printf("disconnected: %v — reconnecting in 3s", err)
		select {
		case <-time.After(3 * time.Second):
		case <-ctx.Done():
			fmt.Println("\nshutting down")
			return nil
		}
	}
}

func matchesFilter(filter, source, typ, summary string) bool {
	if filter == "" {
		return true
	}
	lower := strings.ToLower(filter)
	return strings.Contains(strings.ToLower(source), lower) ||
		strings.Contains(strings.ToLower(typ), lower) ||
		strings.Contains(strings.ToLower(summary), lower)
}

func matchesPattern(pattern, source, typ, summary string) bool {
	lower := strings.ToLower(pattern)
	return strings.Contains(strings.ToLower(source), lower) ||
		strings.Contains(strings.ToLower(typ), lower) ||
		strings.Contains(strings.ToLower(summary), lower)
}

// alertState tracks sliding window counters for threshold alerts.
type alertState struct {
	mu      sync.Mutex
	windows map[string][]time.Time // pattern -> list of event timestamps
}

func (a *alertState) record(pattern string, now time.Time, windowMinutes int) int {
	a.mu.Lock()
	defer a.mu.Unlock()

	cutoff := now.Add(-time.Duration(windowMinutes) * time.Minute)
	times := a.windows[pattern]

	// Prune old entries
	valid := times[:0]
	for _, t := range times {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}
	valid = append(valid, now)
	a.windows[pattern] = valid
	return len(valid)
}

func listen(ctx context.Context, endpoint string, names map[string]string, filter string, sound string, muted []string, slackURL string, discordURL string, alerts []auth.AlertRule, alertCounters *alertState) error {
	conn, _, err := websocket.Dial(ctx, endpoint, nil)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.CloseNow()

	// Read registration handshake.
	_, _, err = conn.Read(ctx)
	if err != nil {
		return fmt.Errorf("handshake: %w", err)
	}

	mutedSet := make(map[string]bool, len(muted))
	for _, m := range muted {
		mutedSet[m] = true
	}

	log.Println("connected")

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}

		var msg hub.Message
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}

		if msg.Type == hub.MsgTypeEvent && msg.Event != nil {
			// Skip muted channels
			if mutedSet[msg.Event.Channel] {
				continue
			}
			if !matchesFilter(filter, msg.Event.Source, msg.Event.Type, msg.Event.Summary) {
				continue
			}
			title := names[msg.Event.Channel]
			if title == "" {
				title = msg.Event.Channel
			}
			notify.Send(title, msg.Event.Summary, sound)
			log.Printf("[%s] %s", title, msg.Event.Summary)

			// Forward to Slack/Discord
			if slackURL != "" {
				go forwardToSlack(slackURL, title, msg.Event.Source, msg.Event.Summary)
			}
			if discordURL != "" {
				go forwardToDiscord(discordURL, title, msg.Event.Source, msg.Event.Summary)
			}

			// Check threshold alerts
			for _, rule := range alerts {
				if matchesPattern(rule.Pattern, msg.Event.Source, msg.Event.Type, msg.Event.Summary) {
					count := alertCounters.record(rule.Pattern, time.Now(), rule.WindowMinutes)
					if count == rule.Threshold {
						alertMsg := fmt.Sprintf("Alert: %d events matching %q in %dm", count, rule.Pattern, rule.WindowMinutes)
						notify.Send("dread alert", alertMsg, sound)
						log.Printf("[ALERT] %s", alertMsg)
						if slackURL != "" {
							go forwardToSlack(slackURL, "dread alert", "alert", alertMsg)
						}
						if discordURL != "" {
							go forwardToDiscord(discordURL, "dread alert", "alert", alertMsg)
						}
					}
				}
			}
		}
	}
}

func forwardToSlack(webhookURL, channel, source, summary string) {
	payload := map[string]any{
		"text": fmt.Sprintf("[%s] %s", channel, summary),
		"blocks": []map[string]any{
			{
				"type": "section",
				"text": map[string]any{
					"type": "mrkdwn",
					"text": fmt.Sprintf("*%s* (%s)\n%s", channel, source, summary),
				},
			},
		},
	}
	body, _ := json.Marshal(payload)
	http.Post(webhookURL, "application/json", bytes.NewReader(body))
}

func forwardToDiscord(webhookURL, channel, source, summary string) {
	payload := map[string]any{
		"embeds": []map[string]any{
			{
				"title":       fmt.Sprintf("%s — %s", channel, source),
				"description": summary,
				"color":       0xC37960, // dread brand colour
			},
		},
	}
	body, _ := json.Marshal(payload)
	http.Post(webhookURL, "application/json", bytes.NewReader(body))
}

// resolveWorkspace fetches a workspace's channels from the server.
func resolveWorkspace(serverURL, wsID string) ([]auth.Channel, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(serverURL + "/api/workspaces/" + wsID)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var payload struct {
		Channels []auth.Channel `json:"channels"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	return payload.Channels, nil
}

// mergeChannels merges remote channels into local, deduplicating by ID.
func mergeChannels(local, remote []auth.Channel) []auth.Channel {
	seen := make(map[string]bool, len(local))
	for _, ch := range local {
		seen[ch.ID] = true
	}
	merged := make([]auth.Channel, len(local))
	copy(merged, local)
	for _, ch := range remote {
		if !seen[ch.ID] {
			merged = append(merged, ch)
			seen[ch.ID] = true
		}
	}
	return merged
}
