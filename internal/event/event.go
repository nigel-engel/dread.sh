package event

import "time"

// Event represents a normalized webhook event from any source.
type Event struct {
	ID        string    `json:"id"`
	Channel   string    `json:"channel,omitempty"`
	Source    string    `json:"source"`
	Type      string    `json:"type"`
	Summary   string    `json:"summary"`
	RawJSON   string    `json:"raw_json"`
	Timestamp time.Time `json:"timestamp"`
}
