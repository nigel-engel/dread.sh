package tui

import (
	"time"

	"dread.sh/internal/event"

	"github.com/coder/websocket"
)

type newEventMsg struct {
	Event event.Event
}

type historyMsg struct {
	Events  []event.Event
	HasMore bool
}

type wsConnectedMsg struct {
	conn        *websocket.Conn
	webhookURLs map[string]string // channel -> URL
}

type wsErrorMsg struct {
	Err error
}

type tickMsg time.Time

type forwardResultMsg struct {
	EventID    string
	StatusCode int
	Err        error
}

type clipboardMsg struct {
	Err error
}
