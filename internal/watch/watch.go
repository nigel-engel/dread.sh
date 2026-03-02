package watch

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
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
func Run(serverURL string, filter string) error {
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

		if len(cfg.Channels) == 0 {
			log.Printf("no channels configured — retrying in 10s")
			select {
			case <-time.After(10 * time.Second):
				continue
			case <-ctx.Done():
				return nil
			}
		}

		ids := make([]string, len(cfg.Channels))
		nameByID := make(map[string]string, len(cfg.Channels))
		for i, ch := range cfg.Channels {
			ids[i] = ch.ID
			nameByID[ch.ID] = ch.Name
		}

		endpoint := wsURL + "/ws?channels=" + strings.Join(ids, ",")
		fmt.Printf("watching %d channel(s)...\n", len(cfg.Channels))

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
