package webhook

import (
	"net/http"
	"strings"
)

// DetectSource inspects HTTP headers to automatically identify the webhook source.
// Returns the detected source name, or falls back to the X-Dread-Source header, or "webhook".
func DetectSource(header http.Header) string {
	// --- Payment / Finance ---

	// Stripe
	if header.Get("Stripe-Signature") != "" {
		return "stripe"
	}
	// PayPal
	if header.Get("Paypal-Transmission-Sig") != "" {
		return "paypal"
	}
	// Square
	if header.Get("X-Square-Hmacsha256-Signature") != "" {
		return "square"
	}
	// Razorpay
	if header.Get("X-Razorpay-Signature") != "" {
		return "razorpay"
	}
	// Paddle
	if header.Get("Paddle-Signature") != "" {
		return "paddle"
	}
	// Recurly
	if header.Get("Recurly-Signature") != "" {
		return "recurly"
	}
	// Coinbase Commerce
	if header.Get("X-Cc-Webhook-Signature") != "" {
		return "coinbase"
	}
	// Plaid
	if header.Get("Plaid-Verification") != "" {
		return "plaid"
	}
	// Xero
	if header.Get("X-Xero-Signature") != "" {
		return "xero"
	}
	// QuickBooks / Intuit
	if header.Get("Intuit-Signature") != "" {
		return "quickbooks"
	}

	// --- Dev / Code ---

	// GitHub
	if header.Get("X-GitHub-Event") != "" {
		return "github"
	}
	// GitLab
	if header.Get("X-Gitlab-Event") != "" {
		return "gitlab"
	}
	// Bitbucket
	if header.Get("X-Hook-UUID") != "" && strings.Contains(strings.ToLower(header.Get("User-Agent")), "bitbucket") {
		return "bitbucket"
	}
	// CircleCI
	if header.Get("Circleci-Event-Type") != "" {
		return "circleci"
	}
	// Travis CI
	if header.Get("Travis-Repo-Slug") != "" {
		return "travis-ci"
	}
	// Buildkite
	if header.Get("X-Buildkite-Event") != "" {
		return "buildkite"
	}

	// --- Infrastructure ---

	// Vercel
	if header.Get("X-Vercel-Signature") != "" {
		return "vercel"
	}
	// Heroku
	if header.Get("Heroku-Webhook-Hmac-Sha256") != "" {
		return "heroku"
	}
	// AWS SNS
	if header.Get("X-Amz-Sns-Message-Type") != "" {
		return "aws"
	}
	// Cloudflare
	if header.Get("Cf-Webhook-Auth") != "" {
		return "cloudflare"
	}

	// --- Communication ---

	// Slack
	if header.Get("X-Slack-Signature") != "" {
		return "slack"
	}
	// Discord
	if header.Get("X-Signature-Ed25519") != "" {
		return "discord"
	}
	// Twilio
	if header.Get("X-Twilio-Signature") != "" {
		return "twilio"
	}
	// SendGrid
	if header.Get("X-Twilio-Email-Event-Webhook-Signature") != "" {
		return "sendgrid"
	}
	// Mailchimp / Mandrill
	if header.Get("X-Mandrill-Signature") != "" {
		return "mailchimp"
	}
	// Zendesk
	if header.Get("X-Zendesk-Webhook-Signature") != "" {
		return "zendesk"
	}
	// Telegram
	if header.Get("X-Telegram-Bot-Api-Secret-Token") != "" {
		return "telegram"
	}
	// LINE
	if header.Get("X-Line-Signature") != "" {
		return "line"
	}

	// --- Project Management ---

	// Linear
	if header.Get("Linear-Delivery") != "" {
		return "linear"
	}
	// Jira / Atlassian
	if header.Get("X-Atlassian-Webhook-Identifier") != "" {
		return "jira"
	}
	// Notion
	if header.Get("X-Notion-Signature") != "" {
		return "notion"
	}
	// Trello
	if header.Get("X-Trello-Webhook") != "" {
		return "trello"
	}
	// Airtable
	if header.Get("X-Airtable-Content-Mac") != "" {
		return "airtable"
	}

	// --- Monitoring ---

	// Sentry
	if header.Get("Sentry-Hook-Resource") != "" {
		return "sentry"
	}
	// PagerDuty
	if header.Get("X-Pagerduty-Signature") != "" {
		return "pagerduty"
	}
	// Grafana
	if header.Get("X-Grafana-Alerting-Signature") != "" {
		return "grafana"
	}

	// --- CMS / Commerce ---

	// Shopify
	if header.Get("X-Shopify-Topic") != "" {
		return "shopify"
	}
	// WooCommerce
	if header.Get("X-Wc-Webhook-Topic") != "" {
		return "woocommerce"
	}
	// Contentful
	if header.Get("X-Contentful-Topic") != "" {
		return "contentful"
	}
	// Sanity
	if header.Get("Sanity-Webhook-Signature") != "" {
		return "sanity"
	}
	// BigCommerce
	if header.Get("X-Bc-Webhook-Signature") != "" {
		return "bigcommerce"
	}

	// --- Auth / Identity ---

	// Auth0
	if header.Get("Auth0-Signature") != "" {
		return "auth0"
	}
	// WorkOS
	if header.Get("Workos-Signature") != "" {
		return "workos"
	}

	// --- Database / Backend ---

	// Supabase
	if header.Get("X-Supabase-Event-Signature") != "" {
		return "supabase"
	}
	// PlanetScale
	if header.Get("X-Planetscale-Signature") != "" {
		return "planetscale"
	}

	// --- SaaS ---

	// Svix (used by Clerk, Resend, etc.)
	if header.Get("Svix-Id") != "" {
		return "svix"
	}
	// HubSpot
	if header.Get("X-Hubspot-Signature") != "" {
		return "hubspot"
	}
	// Typeform
	if header.Get("Typeform-Signature") != "" {
		return "typeform"
	}
	// Calendly
	if header.Get("Calendly-Webhook-Signature") != "" {
		return "calendly"
	}
	// DocuSign
	if header.Get("X-Docusign-Signature-1") != "" {
		return "docusign"
	}
	// Zoom
	if header.Get("X-Zm-Signature") != "" {
		return "zoom"
	}
	// Figma
	if header.Get("Figma-Signature") != "" {
		return "figma"
	}
	// Knock
	if header.Get("X-Knock-Signature") != "" {
		return "knock"
	}
	// Novu
	if header.Get("X-Novu-Signature") != "" {
		return "novu"
	}
	// LaunchDarkly
	if header.Get("X-Ld-Signature") != "" {
		return "launchdarkly"
	}
	// Customer.io
	if header.Get("X-Cio-Signature") != "" {
		return "customerio"
	}
	// Pusher
	if header.Get("X-Pusher-Key") != "" {
		return "pusher"
	}
	// Ably
	if header.Get("X-Ably-Key") != "" {
		return "ably"
	}
	// Twitch
	if header.Get("Twitch-Eventsub-Message-Signature") != "" {
		return "twitch"
	}

	// --- User-Agent fallback ---
	ua := strings.ToLower(header.Get("User-Agent"))
	switch {
	case strings.Contains(ua, "stripe"):
		return "stripe"
	case strings.Contains(ua, "github"):
		return "github"
	case strings.Contains(ua, "gitlab"):
		return "gitlab"
	case strings.Contains(ua, "bitbucket"):
		return "bitbucket"
	case strings.Contains(ua, "shopify"):
		return "shopify"
	case strings.Contains(ua, "woocommerce"):
		return "woocommerce"
	case strings.Contains(ua, "supabase"):
		return "supabase"
	case strings.Contains(ua, "pagerduty"):
		return "pagerduty"
	case strings.Contains(ua, "pingdom"):
		return "pingdom"
	case strings.Contains(ua, "zapier"):
		return "zapier"
	}

	// X-Dread-Source custom header
	if src := header.Get("X-Dread-Source"); src != "" {
		return src
	}

	return "webhook"
}
