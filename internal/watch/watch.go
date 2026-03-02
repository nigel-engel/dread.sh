package watch

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"dread.sh/internal/auth"
	"dread.sh/internal/hub"
	"dread.sh/internal/notify"

	"github.com/coder/websocket"
)

// Run connects to the server via WebSocket and sends desktop notifications
// for incoming events. It reloads the config on each reconnect so new
// channels are picked up automatically. Shuts down on SIGINT/SIGTERM.
// If filter is non-empty, only events matching the substring (in source,
// type, or summary) will trigger notifications.
func Run(serverURL string, filter string, follows []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	wsURL := strings.Replace(serverURL, "http://", "ws://", 1)
	wsURL = strings.Replace(wsURL, "https://", "wss://", 1)

	for {
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

		endpoint := wsURL + "/ws?channels=" + strings.Join(ids, ",")
		fmt.Printf("watching %d channel(s)...\n", len(channels))

		err = listen(ctx, endpoint, nameByID, filter)
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

func listen(ctx context.Context, endpoint string, names map[string]string, filter string) error {
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
			if !matchesFilter(filter, msg.Event.Source, msg.Event.Type, msg.Event.Summary) {
				continue
			}
			title := names[msg.Event.Channel]
			if title == "" {
				title = msg.Event.Channel
			}
			notify.Send(title, msg.Event.Summary)
			log.Printf("[%s] %s", title, msg.Event.Summary)
		}
	}
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
