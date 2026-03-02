package webhook

import (
	"fmt"
	"io"
	"net/http"
	"time"

	"dread.sh/internal/event"

	"github.com/google/uuid"
)

// Processor verifies a webhook request and normalizes it into an Event.
type Processor interface {
	Process(source string, header http.Header, body []byte) (*event.Event, error)
}

// MakeHandler creates an http.HandlerFunc for incoming webhooks.
// onEvent is called after successful processing with the channel and event.
func MakeHandler(onEvent func(channel string, ev *event.Event)) http.HandlerFunc {
	proc := &GenericProcessor{}

	return func(w http.ResponseWriter, r *http.Request) {
		channel := r.PathValue("channel")
		if channel == "" {
			http.Error(w, "missing channel", http.StatusBadRequest)
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1 MB limit
		if err != nil {
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}

		source := DetectSource(r.Header)

		ev, err := proc.Process(source, r.Header, body)
		if err != nil {
			http.Error(w, fmt.Sprintf("webhook rejected: %v", err), http.StatusBadRequest)
			return
		}

		if ev.ID == "" {
			ev.ID = uuid.New().String()
		}
		if ev.Timestamp.IsZero() {
			ev.Timestamp = time.Now().UTC()
		}
		ev.Channel = channel

		onEvent(channel, ev)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	}
}
