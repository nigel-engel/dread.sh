package webhook

import (
	"encoding/json"
	"fmt"
	"net/http"

	"dread.sh/internal/event"

	"github.com/stripe/stripe-go/v82/webhook"
)

// StripeProcessor verifies and normalizes Stripe webhook events.
type StripeProcessor struct {
	Secret string
}

func (p *StripeProcessor) Process(source string, header http.Header, body []byte) (*event.Event, error) {
	sig := header.Get("Stripe-Signature")
	if sig == "" {
		return nil, fmt.Errorf("missing Stripe-Signature header")
	}

	stripeEvent, err := webhook.ConstructEvent(body, sig, p.Secret)
	if err != nil {
		return nil, fmt.Errorf("stripe verification failed: %w", err)
	}

	raw, _ := json.Marshal(stripeEvent)

	return &event.Event{
		ID:      stripeEvent.ID,
		Source:  source,
		Type:    string(stripeEvent.Type),
		Summary: fmt.Sprintf("[stripe] %s", stripeEvent.Type),
		RawJSON: string(raw),
	}, nil
}
