package hub

import "dread.sh/internal/event"

// MsgType identifies the type of WebSocket message.
type MsgType string

const (
	MsgTypeEvent      MsgType = "event"
	MsgTypeHistory    MsgType = "history"
	MsgTypeRegistered MsgType = "registered"
)

// Message is the envelope for all WebSocket messages.
type Message struct {
	Type        MsgType           `json:"type"`
	Event       *event.Event      `json:"event,omitempty"`
	Events      []event.Event     `json:"events,omitempty"`
	HasMore     bool              `json:"has_more,omitempty"`
	WebhookURLs map[string]string `json:"webhook_urls,omitempty"` // channel -> webhook URL
}
