package forward

import (
	"bytes"
	"fmt"
	"net/http"
	"time"

	"dread.sh/internal/event"
)

// Forwarder forwards webhook events to a local URL.
type Forwarder struct {
	URL    string
	client *http.Client
}

// New creates a new Forwarder targeting the given URL.
func New(url string) *Forwarder {
	return &Forwarder{
		URL: url,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// Forward sends an event's raw JSON payload to the forward URL.
// Returns the HTTP status code and any error.
func (f *Forwarder) Forward(ev *event.Event) (int, error) {
	req, err := http.NewRequest("POST", f.URL, bytes.NewBufferString(ev.RawJSON))
	if err != nil {
		return 0, fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Dread-Source", ev.Source)
	req.Header.Set("X-Dread-Event-Type", ev.Type)
	req.Header.Set("X-Dread-Event-ID", ev.ID)

	resp, err := f.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("forwarding: %w", err)
	}
	defer resp.Body.Close()

	return resp.StatusCode, nil
}
