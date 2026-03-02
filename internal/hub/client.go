package hub

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/coder/websocket"
)

// Client wraps a single WebSocket connection.
type Client struct {
	conn     *websocket.Conn
	channels []string
	send     chan []byte
}

func newClient(conn *websocket.Conn, channels []string) *Client {
	return &Client{
		conn:     conn,
		channels: channels,
		send:     make(chan []byte, 64),
	}
}

// writePump sends queued messages to the WebSocket connection.
func (c *Client) writePump(ctx context.Context) {
	for {
		select {
		case msg, ok := <-c.send:
			if !ok {
				return
			}
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := c.conn.Write(ctx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				log.Printf("ws write error: %v", err)
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// Send marshals a message and queues it for sending.
func (c *Client) Send(msg *Message) {
	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("marshal error: %v", err)
		return
	}
	select {
	case c.send <- data:
	default:
		log.Printf("client send buffer full, dropping message")
	}
}
