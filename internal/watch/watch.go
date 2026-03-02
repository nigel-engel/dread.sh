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
// for incoming events. It reconnects automatically on disconnect and shuts
// down cleanly on SIGINT/SIGTERM.
func Run(serverURL string, channels []auth.Channel) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ids := make([]string, len(channels))
	nameByID := make(map[string]string, len(channels))
	for i, ch := range channels {
		ids[i] = ch.ID
		nameByID[ch.ID] = ch.Name
	}

	wsURL := strings.Replace(serverURL, "http://", "ws://", 1)
	wsURL = strings.Replace(wsURL, "https://", "wss://", 1)
	endpoint := wsURL + "/ws?channels=" + strings.Join(ids, ",")

	fmt.Printf("watching %d channel(s)...\n", len(channels))

	for {
		err := listen(ctx, endpoint, nameByID)
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

func listen(ctx context.Context, endpoint string, names map[string]string) error {
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
			title := names[msg.Event.Channel]
			if title == "" {
				title = msg.Event.Channel
			}
			notify.Send(title, msg.Event.Summary)
			log.Printf("[%s] %s", title, msg.Event.Summary)
		}
	}
}
