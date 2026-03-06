package forward

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"time"

	"dread.sh/internal/event"
)

// Result holds the full response data from a forwarded request.
type Result struct {
	StatusCode int
	Headers    http.Header
	Body       string
	Duration   time.Duration
	Err        error
}

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
	r := f.ForwardFull(ev)
	return r.StatusCode, r.Err
}

// ForwardFull sends an event and returns the full response details.
func (f *Forwarder) ForwardFull(ev *event.Event) Result {
	start := time.Now()

	req, err := http.NewRequest("POST", f.URL, bytes.NewBufferString(ev.RawJSON))
	if err != nil {
		return Result{Err: fmt.Errorf("creating request: %w", err)}
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Dread-Source", ev.Source)
	req.Header.Set("X-Dread-Event-Type", ev.Type)
	req.Header.Set("X-Dread-Event-ID", ev.ID)

	resp, err := f.client.Do(req)
	if err != nil {
		return Result{Err: fmt.Errorf("forwarding: %w", err), Duration: time.Since(start)}
	}
	defer resp.Body.Close()

	// Read up to 64KB of response body
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	return Result{
		StatusCode: resp.StatusCode,
		Headers:    resp.Header,
		Body:       string(body),
		Duration:   time.Since(start),
	}
}
