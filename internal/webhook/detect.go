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
	// Lemon Squeezy
	if header.Get("X-Event-Name") != "" {
		return "lemonsqueezy"
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
	// FreshBooks
	if header.Get("X-Freshbooks-Hmac-Sha256") != "" {
		return "freshbooks"
	}
	// Wave
	if header.Get("Wave-Signature") != "" {
		return "wave"
	}
	// Chargebee
	if header.Get("X-Chargebee-Webhook-Api-Version") != "" {
		return "chargebee"
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
	// Webflow
	if header.Get("X-Webflow-Signature") != "" {
		return "webflow"
	}
	// Squarespace
	if header.Get("Squarespace-Signature") != "" {
		return "squarespace"
	}
	// Netlify (deploy notifications)
	if header.Get("X-Netlify-Event") != "" {
		return "netlify"
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

	// --- Social Media ---

	// TikTok
	if header.Get("Tiktok-Signature") != "" {
		return "tiktok"
	}
	// Hootsuite
	if header.Get("X-Hootsuite-Signature") != "" {
		return "hootsuite"
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
	// Asana
	if header.Get("X-Hook-Signature") != "" {
		return "asana"
	}
	// Teamwork
	if header.Get("X-Projects-Signature") != "" {
		return "teamwork"
	}
	// Smartsheet
	if header.Get("Smartsheet-Hmac-Sha256") != "" {
		return "smartsheet"
	}
	// Miro
	if header.Get("X-Miro-Signature") != "" {
		return "miro"
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
	// Ecwid
	if header.Get("X-Ecwid-Webhook-Signature") != "" {
		return "ecwid"
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

	// --- File Storage ---

	// Dropbox
	if header.Get("X-Dropbox-Signature") != "" {
		return "dropbox"
	}
	// Box
	if header.Get("Box-Signature-Primary") != "" {
		return "box"
	}

	// --- CRM / Sales ---

	// Pipedrive
	if header.Get("X-Pipedrive-Signature") != "" {
		return "pipedrive"
	}
	// Zoho
	if header.Get("X-Zoho-Webhook-Signature") != "" {
		return "zoho"
	}
	// Keap / Infusionsoft
	if header.Get("X-Keap-Signature") != "" {
		return "keap"
	}
	// Close CRM
	if header.Get("Close-Sig-Hash") != "" {
		return "close"
	}
	// Copper CRM
	if header.Get("X-Pw-Application") != "" {
		return "copper"
	}

	// --- Customer Support ---

	// Help Scout
	if header.Get("X-Helpscout-Signature") != "" {
		return "helpscout"
	}
	// Crisp
	if header.Get("X-Crisp-Signature") != "" {
		return "crisp"
	}
	// Drift
	if header.Get("X-Drift-Signature") != "" {
		return "drift"
	}

	// --- Email Marketing ---

	// Klaviyo
	if header.Get("Klaviyo-Signature") != "" {
		return "klaviyo"
	}
	// MailerLite
	if header.Get("X-Mailerlite-Signature") != "" {
		return "mailerlite"
	}

	// --- HR / Recruiting ---

	// BambooHR
	if header.Get("X-Bamboohr-Signature") != "" {
		return "bamboohr"
	}

	// --- Forms / Surveys ---

	// Tally
	if header.Get("Tally-Signature") != "" {
		return "tally"
	}
	// SurveyMonkey
	if header.Get("Sm-Signature") != "" {
		return "surveymonkey"
	}

	// --- Scheduling ---

	// Cal.com
	if header.Get("X-Cal-Signature-256") != "" {
		return "calcom"
	}
	// Acuity Scheduling
	if header.Get("X-Acuity-Signature") != "" {
		return "acuity"
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
	case strings.Contains(ua, "postmark"):
		return "postmark"
	case strings.Contains(ua, "mailgun"):
		return "mailgun"
	case strings.Contains(ua, "intercom"):
		return "intercom"
	case strings.Contains(ua, "netlify"):
		return "netlify"
	case strings.Contains(ua, "clickup"):
		return "clickup"
	case strings.Contains(ua, "convertkit"):
		return "convertkit"
	case strings.Contains(ua, "brevo"):
		return "brevo"
	case strings.Contains(ua, "sendinblue"):
		return "brevo"
	case strings.Contains(ua, "freshdesk"):
		return "freshdesk"
	case strings.Contains(ua, "freshworks"):
		return "freshdesk"
	case strings.Contains(ua, "railway"):
		return "railway"
	case strings.Contains(ua, "datadog"):
		return "datadog"
	case strings.Contains(ua, "newrelic"):
		return "newrelic"
	case strings.Contains(ua, "tiktok"):
		return "tiktok"
	case strings.Contains(ua, "facebook"):
		return "meta"
	case strings.Contains(ua, "basecamp"):
		return "basecamp"
	case strings.Contains(ua, "activecampaign"):
		return "activecampaign"
	case strings.Contains(ua, "monday"):
		return "monday"
	case strings.Contains(ua, "chargebee"):
		return "chargebee"
	case strings.Contains(ua, "salesforce"):
		return "salesforce"
	case strings.Contains(ua, "pipedrive"):
		return "pipedrive"
	case strings.Contains(ua, "asana"):
		return "asana"
	case strings.Contains(ua, "webflow"):
		return "webflow"
	case strings.Contains(ua, "squarespace"):
		return "squarespace"
	case strings.Contains(ua, "klaviyo"):
		return "klaviyo"
	case strings.Contains(ua, "freshbooks"):
		return "freshbooks"
	case strings.Contains(ua, "helpscout"):
		return "helpscout"
	case strings.Contains(ua, "zoho"):
		return "zoho"
	case strings.Contains(ua, "mailerlite"):
		return "mailerlite"
	case strings.Contains(ua, "bamboohr"):
		return "bamboohr"
	case strings.Contains(ua, "miro"):
		return "miro"
	case strings.Contains(ua, "drift"):
		return "drift"
	case strings.Contains(ua, "crisp"):
		return "crisp"
	case strings.Contains(ua, "smartsheet"):
		return "smartsheet"
	case strings.Contains(ua, "hootsuite"):
		return "hootsuite"
	case strings.Contains(ua, "dropbox"):
		return "dropbox"
	case strings.Contains(ua, "ecwid"):
		return "ecwid"
	}

	// X-Dread-Source custom header
	if src := header.Get("X-Dread-Source"); src != "" {
		return src
	}

	return "webhook"
}
