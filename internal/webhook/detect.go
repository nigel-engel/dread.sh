package webhook

import (
	"net/http"
	"strings"
)

// DetectSource inspects HTTP headers to automatically identify the webhook source.
// Returns the detected source name, or falls back to the X-Dread-Source header, or "webhook".
func DetectSource(header http.Header) string {
	// Stripe
	if header.Get("Stripe-Signature") != "" {
		return "stripe"
	}

	// GitHub
	if header.Get("X-GitHub-Event") != "" {
		return "github"
	}

	// Shopify
	if header.Get("X-Shopify-Topic") != "" {
		return "shopify"
	}

	// Twilio
	if header.Get("X-Twilio-Signature") != "" {
		return "twilio"
	}

	// SendGrid
	if header.Get("X-Twilio-Email-Event-Webhook-Signature") != "" {
		return "sendgrid"
	}

	// Slack
	if header.Get("X-Slack-Signature") != "" {
		return "slack"
	}

	// Discord
	if header.Get("X-Signature-Ed25519") != "" {
		return "discord"
	}

	// Linear
	if header.Get("Linear-Delivery") != "" {
		return "linear"
	}

	// Svix (used by Clerk, Resend, etc.)
	if header.Get("Svix-Id") != "" {
		return "svix"
	}

	// Paddle
	if header.Get("Paddle-Signature") != "" {
		return "paddle"
	}

	// User-Agent fallback
	ua := strings.ToLower(header.Get("User-Agent"))
	switch {
	case strings.Contains(ua, "stripe"):
		return "stripe"
	case strings.Contains(ua, "github"):
		return "github"
	case strings.Contains(ua, "shopify"):
		return "shopify"
	}

	// X-Dread-Source custom header
	if src := header.Get("X-Dread-Source"); src != "" {
		return src
	}

	return "webhook"
}
