package hub

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"

	"dread.sh/internal/event"

	"github.com/coder/websocket"
)

// Hub manages WebSocket client connections and broadcasts events per channel.
type Hub struct {
	mu      sync.RWMutex
	clients map[string]map[*Client]struct{} // channel -> clients
}

// New creates a new Hub.
func New() *Hub {
	return &Hub{
		clients: make(map[string]map[*Client]struct{}),
	}
}

// Broadcast sends an event to all clients subscribed to the given channel.
func (h *Hub) Broadcast(channel string, ev *event.Event) {
	msg := &Message{
		Type:  MsgTypeEvent,
		Event: ev,
	}
	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("broadcast marshal error: %v", err)
		return
	}

	h.mu.RLock()
	defer h.mu.RUnlock()

	for c := range h.clients[channel] {
		select {
		case c.send <- data:
		default:
			log.Printf("dropping message for slow client")
		}
	}
}

// HandleWS is the HTTP handler that upgrades connections to WebSocket.
// It reads the "channels" query parameter (comma-separated) and subscribes
// the client to all listed channels.
func (h *Hub) HandleWS(baseURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		channelsParam := r.URL.Query().Get("channels")
		if channelsParam == "" {
			http.Error(w, "missing channels query parameter", http.StatusBadRequest)
			return
		}

		channels := strings.Split(channelsParam, ",")

		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
		})
		if err != nil {
			log.Printf("ws accept error: %v", err)
			return
		}

		client := newClient(conn, channels)

		// Register client for all channels
		h.mu.Lock()
		for _, ch := range channels {
			if h.clients[ch] == nil {
				h.clients[ch] = make(map[*Client]struct{})
			}
			h.clients[ch][client] = struct{}{}
		}
		h.mu.Unlock()

		ctx := r.Context()
		go client.writePump(ctx)

		// Send registration message with webhook URLs for all channels
		webhookURLs := make(map[string]string, len(channels))
		for _, ch := range channels {
			webhookURLs[ch] = baseURL + "/wh/" + ch
		}
		client.Send(&Message{
			Type:        MsgTypeRegistered,
			WebhookURLs: webhookURLs,
		})

		defer func() {
			h.mu.Lock()
			for _, ch := range channels {
				delete(h.clients[ch], client)
				if len(h.clients[ch]) == 0 {
					delete(h.clients, ch)
				}
			}
			h.mu.Unlock()
			close(client.send)
			conn.CloseNow()
		}()

		for {
			_, _, err := conn.Read(ctx)
			if err != nil {
				return
			}
		}
	}
}
