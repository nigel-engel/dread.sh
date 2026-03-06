package webhook

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"dread.sh/internal/event"
)

// GenericProcessor normalizes webhook payloads with source-aware summary extraction.
type GenericProcessor struct{}

func (p *GenericProcessor) Process(source string, header http.Header, body []byte) (*event.Event, error) {
	var raw map[string]any
	json.Unmarshal(body, &raw)

	// HubSpot sends a JSON array — use the first element
	if raw == nil && len(body) > 0 && body[0] == '[' {
		var arr []map[string]any
		if err := json.Unmarshal(body, &arr); err == nil && len(arr) > 0 {
			raw = arr[0]
		}
	}

	ev := &event.Event{
		Source:  source,
		RawJSON: string(body),
	}

	switch source {
	case "stripe":
		ev.Type, ev.Summary = summarizeStripe(raw)
	case "github":
		ev.Type, ev.Summary = summarizeGitHub(header, raw)
	case "shopify":
		ev.Type, ev.Summary = summarizeShopify(header, raw)
	case "slack":
		ev.Type, ev.Summary = summarizeSlack(raw)
	case "discord":
		ev.Type, ev.Summary = summarizeDiscord(raw)
	case "linear":
		ev.Type, ev.Summary = summarizeLinear(raw)
	case "paddle":
		ev.Type, ev.Summary = summarizePaddle(raw)
	case "vercel":
		ev.Type, ev.Summary = summarizeVercel(raw)
	case "sentry":
		ev.Type, ev.Summary = summarizeSentry(header, raw)
	case "pagerduty":
		ev.Type, ev.Summary = summarizePagerDuty(raw)
	case "jira":
		ev.Type, ev.Summary = summarizeJira(raw)
	case "gitlab":
		ev.Type, ev.Summary = summarizeGitLab(header, raw)
	case "paypal":
		ev.Type, ev.Summary = summarizePayPal(raw)
	case "aws":
		ev.Type, ev.Summary = summarizeAWSSNS(raw)
	case "twitch":
		ev.Type, ev.Summary = summarizeTwitch(header, raw)
	case "hubspot":
		ev.Type, ev.Summary = summarizeHubSpot(raw)
	case "typeform":
		ev.Type, ev.Summary = summarizeTypeform(raw)
	case "supabase":
		ev.Type, ev.Summary = summarizeSupabase(raw)
	case "postmark":
		ev.Type, ev.Summary = summarizePostmark(raw)
	case "mailgun":
		ev.Type, ev.Summary = summarizeMailgun(raw)
	case "meta":
		ev.Type, ev.Summary = summarizeMeta(raw)
	case "intercom":
		ev.Type, ev.Summary = summarizeIntercom(raw)
	case "lemonsqueezy":
		ev.Type, ev.Summary = summarizeLemonSqueezy(raw)
	case "netlify":
		ev.Type, ev.Summary = summarizeNetlify(raw)
	case "render":
		ev.Type, ev.Summary = summarizeRender(raw)
	case "newrelic":
		ev.Type, ev.Summary = summarizeNewRelic(raw)
	case "gumroad":
		ev.Type, ev.Summary = summarizeGumroad(raw)
	case "clickup":
		ev.Type, ev.Summary = summarizeClickUp(raw)
	case "railway":
		ev.Type, ev.Summary = summarizeRailway(raw)
	case "brevo":
		ev.Type, ev.Summary = summarizeBrevo(raw)
	case "datadog":
		ev.Type, ev.Summary = summarizeDatadog(raw)
	case "tiktok":
		ev.Type, ev.Summary = summarizeTikTok(raw)
	case "pipedrive":
		ev.Type, ev.Summary = summarizePipedrive(raw)
	case "asana":
		ev.Type, ev.Summary = summarizeAsana(raw)
	case "webflow":
		ev.Type, ev.Summary = summarizeWebflow(raw)
	case "klaviyo":
		ev.Type, ev.Summary = summarizeKlaviyo(raw)
	case "squarespace":
		ev.Type, ev.Summary = summarizeSquarespace(raw)
	case "ecwid":
		ev.Type, ev.Summary = summarizeEcwid(raw)
	case "box":
		ev.Type, ev.Summary = summarizeBox(raw)
	case "helpscout":
		ev.Type, ev.Summary = summarizeHelpScout(raw)
	case "smartsheet":
		ev.Type, ev.Summary = summarizeSmartsheet(raw)
	case "calcom":
		ev.Type, ev.Summary = summarizeCalcom(raw)
	case "monday":
		ev.Type, ev.Summary = summarizeMonday(raw)
	case "chargebee":
		ev.Type, ev.Summary = summarizeChargebee(raw)
	case "activecampaign":
		ev.Type, ev.Summary = summarizeActiveCampaign(raw)
	case "basecamp":
		ev.Type, ev.Summary = summarizeBasecamp(raw)
	case "trigger.dev":
		ev.Type, ev.Summary = summarizeTriggerDev(raw)
	default:
		ev.Type, ev.Summary = summarizeGeneric(source, raw)
	}

	return ev, nil
}

func summarizeStripe(raw map[string]any) (string, string) {
	typ := str(raw, "type")
	if typ == "" {
		return "webhook", "stripe event"
	}

	// Try to extract useful details from data.object
	if data, ok := raw["data"].(map[string]any); ok {
		if obj, ok := data["object"].(map[string]any); ok {
			// Amount for payment-related events
			if amount, ok := obj["amount"].(float64); ok {
				currency := strings.ToUpper(str(obj, "currency"))
				if currency == "" {
					currency = "USD"
				}
				return typ, fmt.Sprintf("%s — %s", typ, formatAmount(amount, currency))
			}
			// Status for subscription events
			if status := str(obj, "status"); status != "" {
				return typ, fmt.Sprintf("%s — %s", typ, status)
			}
			// Email for customer events
			if email := str(obj, "email"); email != "" {
				return typ, fmt.Sprintf("%s — %s", typ, email)
			}
		}
	}

	return typ, typ
}

func summarizeGitHub(header http.Header, raw map[string]any) (string, string) {
	ghEvent := header.Get("X-GitHub-Event")
	if ghEvent == "" {
		ghEvent = "event"
	}
	action := str(raw, "action")
	typ := ghEvent
	if action != "" {
		typ = ghEvent + "." + action
	}

	switch ghEvent {
	case "push":
		ref := str(raw, "ref")
		branch := strings.TrimPrefix(ref, "refs/heads/")
		pusher := ""
		if p, ok := raw["pusher"].(map[string]any); ok {
			pusher = str(p, "name")
		}
		msg := ""
		if commits, ok := raw["commits"].([]any); ok && len(commits) > 0 {
			if c, ok := commits[len(commits)-1].(map[string]any); ok {
				msg = str(c, "message")
				if i := strings.Index(msg, "\n"); i > 0 {
					msg = msg[:i]
				}
			}
		}
		parts := []string{"push to " + branch}
		if pusher != "" {
			parts = append(parts, "by "+pusher)
		}
		if msg != "" {
			parts = append(parts, "— "+msg)
		}
		return typ, strings.Join(parts, " ")

	case "pull_request":
		title := ""
		number := 0
		if pr, ok := raw["pull_request"].(map[string]any); ok {
			title = str(pr, "title")
			if n, ok := pr["number"].(float64); ok {
				number = int(n)
			}
		}
		if action != "" && number > 0 {
			return typ, fmt.Sprintf("PR #%d %s — %s", number, action, title)
		}
		return typ, fmt.Sprintf("pull request %s", action)

	case "issues":
		title := ""
		number := 0
		if issue, ok := raw["issue"].(map[string]any); ok {
			title = str(issue, "title")
			if n, ok := issue["number"].(float64); ok {
				number = int(n)
			}
		}
		if action != "" && number > 0 {
			return typ, fmt.Sprintf("issue #%d %s — %s", number, action, title)
		}
		return typ, fmt.Sprintf("issue %s", action)

	case "star":
		sender := ""
		if s, ok := raw["sender"].(map[string]any); ok {
			sender = str(s, "login")
		}
		if action == "created" && sender != "" {
			return typ, fmt.Sprintf("starred by %s", sender)
		}
		return typ, fmt.Sprintf("star %s", action)

	case "release":
		tag := ""
		if rel, ok := raw["release"].(map[string]any); ok {
			tag = str(rel, "tag_name")
		}
		if action != "" && tag != "" {
			return typ, fmt.Sprintf("release %s — %s", action, tag)
		}
		return typ, fmt.Sprintf("release %s", action)
	}

	if action != "" {
		return typ, fmt.Sprintf("%s %s", ghEvent, action)
	}
	return typ, ghEvent
}

func summarizeShopify(header http.Header, raw map[string]any) (string, string) {
	topic := header.Get("X-Shopify-Topic")
	if topic == "" {
		return "webhook", "shopify event"
	}

	// Try to get order number or product title
	if orderNum := str(raw, "order_number"); orderNum != "" {
		total := str(raw, "total_price")
		if total != "" {
			return topic, fmt.Sprintf("%s — order #%s ($%s)", topic, orderNum, total)
		}
		return topic, fmt.Sprintf("%s — order #%s", topic, orderNum)
	}
	if title := str(raw, "title"); title != "" {
		return topic, fmt.Sprintf("%s — %s", topic, title)
	}

	return topic, topic
}

func summarizeSlack(raw map[string]any) (string, string) {
	typ := str(raw, "type")
	if typ == "" {
		return "webhook", "slack event"
	}

	if evt, ok := raw["event"].(map[string]any); ok {
		evtType := str(evt, "type")
		text := str(evt, "text")
		if text != "" && len(text) > 60 {
			text = text[:60] + "..."
		}
		if text != "" {
			return evtType, fmt.Sprintf("%s — %s", evtType, text)
		}
		if evtType != "" {
			return evtType, evtType
		}
	}

	return typ, typ
}

func summarizeDiscord(raw map[string]any) (string, string) {
	typ := str(raw, "type")
	if typ == "" {
		typ = "event"
	}
	// Discord sends interaction type as int
	if t, ok := raw["type"].(float64); ok {
		switch int(t) {
		case 1:
			return "ping", "ping"
		case 2:
			if data, ok := raw["data"].(map[string]any); ok {
				name := str(data, "name")
				return "command", fmt.Sprintf("command — /%s", name)
			}
			return "command", "slash command"
		}
	}
	return typ, fmt.Sprintf("discord %s", typ)
}

func summarizeLinear(raw map[string]any) (string, string) {
	action := str(raw, "action")
	typ := str(raw, "type")
	if typ == "" {
		typ = "event"
	}
	full := typ
	if action != "" {
		full = typ + "." + action
	}

	if data, ok := raw["data"].(map[string]any); ok {
		title := str(data, "title")
		id := str(data, "identifier")
		if id != "" && title != "" {
			return full, fmt.Sprintf("%s %s — %s %s", typ, action, id, title)
		}
		if title != "" {
			return full, fmt.Sprintf("%s %s — %s", typ, action, title)
		}
	}

	return full, full
}

func summarizePaddle(raw map[string]any) (string, string) {
	typ := str(raw, "event_type")
	if typ == "" {
		return "webhook", "paddle event"
	}

	if data, ok := raw["data"].(map[string]any); ok {
		if status := str(data, "status"); status != "" {
			return typ, fmt.Sprintf("%s — %s", typ, status)
		}
	}

	return typ, typ
}

func summarizeVercel(raw map[string]any) (string, string) {
	typ := str(raw, "type")
	if typ == "" {
		return "webhook", "vercel event"
	}
	if payload, ok := raw["payload"].(map[string]any); ok {
		// Vercel nests project name under payload.deployment
		if dep, ok := payload["deployment"].(map[string]any); ok {
			name := str(dep, "name")
			if name == "" {
				name = str(dep, "url")
			}
			if name != "" {
				return typ, fmt.Sprintf("%s — %s", typ, name)
			}
		}
		// Fallback to payload.name for other event types
		if name := str(payload, "name"); name != "" {
			return typ, fmt.Sprintf("%s — %s", typ, name)
		}
	}
	return typ, typ
}

func summarizeSentry(header http.Header, raw map[string]any) (string, string) {
	resource := header.Get("Sentry-Hook-Resource")
	if resource == "" {
		resource = "event"
	}
	action := str(raw, "action")
	typ := resource
	if action != "" {
		typ = resource + "." + action
	}
	if data, ok := raw["data"].(map[string]any); ok {
		if issue, ok := data["issue"].(map[string]any); ok {
			title := str(issue, "title")
			if title != "" {
				return typ, fmt.Sprintf("%s — %s", typ, title)
			}
		}
		if ev, ok := data["event"].(map[string]any); ok {
			title := str(ev, "title")
			if title != "" {
				return typ, fmt.Sprintf("%s — %s", typ, title)
			}
		}
		// metric_alert webhooks use description_title
		if descTitle := str(data, "description_title"); descTitle != "" {
			return typ, fmt.Sprintf("%s — %s", typ, descTitle)
		}
		// error webhooks use data.error.title
		if errObj, ok := data["error"].(map[string]any); ok {
			title := str(errObj, "title")
			if title != "" {
				return typ, fmt.Sprintf("%s — %s", typ, title)
			}
		}
	}
	return typ, typ
}

func summarizePagerDuty(raw map[string]any) (string, string) {
	// V3 format: top-level "event" object
	if ev, ok := raw["event"].(map[string]any); ok {
		evType := str(ev, "event_type")
		if data, ok := ev["data"].(map[string]any); ok {
			title := str(data, "title")
			if title != "" {
				return evType, fmt.Sprintf("%s — %s", evType, title)
			}
		}
		if evType != "" {
			return evType, evType
		}
	}
	// V2 format: "messages" array
	if messages, ok := raw["messages"].([]any); ok && len(messages) > 0 {
		if msg, ok := messages[0].(map[string]any); ok {
			evType := str(msg, "event")
			if incident, ok := msg["incident"].(map[string]any); ok {
				title := str(incident, "title")
				if title != "" {
					return evType, fmt.Sprintf("%s — %s", evType, title)
				}
			}
			if evType != "" {
				return evType, evType
			}
		}
	}
	return "webhook", "pagerduty event"
}

func summarizeJira(raw map[string]any) (string, string) {
	event := str(raw, "webhookEvent")
	if event == "" {
		return "webhook", "jira event"
	}
	if issue, ok := raw["issue"].(map[string]any); ok {
		key := str(issue, "key")
		if fields, ok := issue["fields"].(map[string]any); ok {
			summary := str(fields, "summary")
			if key != "" && summary != "" {
				return event, fmt.Sprintf("%s — %s %s", event, key, summary)
			}
		}
		if key != "" {
			return event, fmt.Sprintf("%s — %s", event, key)
		}
	}
	return event, event
}

func summarizeGitLab(header http.Header, raw map[string]any) (string, string) {
	glEvent := header.Get("X-Gitlab-Event")
	if glEvent == "" {
		glEvent = str(raw, "object_kind")
	}
	if glEvent == "" {
		return "webhook", "gitlab event"
	}
	switch glEvent {
	case "Push Hook", "push":
		ref := str(raw, "ref")
		branch := strings.TrimPrefix(ref, "refs/heads/")
		user := str(raw, "user_name")
		parts := []string{"push to " + branch}
		if user != "" {
			parts = append(parts, "by "+user)
		}
		return "push", strings.Join(parts, " ")
	case "Merge Request Hook", "merge_request":
		if attrs, ok := raw["object_attributes"].(map[string]any); ok {
			title := str(attrs, "title")
			action := str(attrs, "action")
			if title != "" {
				return "merge_request." + action, fmt.Sprintf("MR %s — %s", action, title)
			}
		}
	case "Issue Hook", "issue":
		if attrs, ok := raw["object_attributes"].(map[string]any); ok {
			title := str(attrs, "title")
			action := str(attrs, "action")
			if title != "" {
				return "issue." + action, fmt.Sprintf("issue %s — %s", action, title)
			}
		}
	case "Pipeline Hook", "pipeline":
		if attrs, ok := raw["object_attributes"].(map[string]any); ok {
			status := str(attrs, "status")
			ref := str(attrs, "ref")
			return "pipeline", fmt.Sprintf("pipeline %s on %s", status, ref)
		}
	}
	return glEvent, glEvent
}

func summarizePayPal(raw map[string]any) (string, string) {
	typ := str(raw, "event_type")
	if typ == "" {
		return "webhook", "paypal event"
	}
	if resource, ok := raw["resource"].(map[string]any); ok {
		if amount, ok := resource["amount"].(map[string]any); ok {
			value := str(amount, "total")
			if value == "" {
				value = str(amount, "value")
			}
			currency := str(amount, "currency_code")
			if currency == "" {
				currency = str(amount, "currency") // v1 fallback
			}
			if value != "" {
				return typ, fmt.Sprintf("%s — %s %s", typ, value, currency)
			}
		}
		if status := str(resource, "status"); status != "" {
			return typ, fmt.Sprintf("%s — %s", typ, status)
		}
		if state := str(resource, "state"); state != "" {
			return typ, fmt.Sprintf("%s — %s", typ, state)
		}
	}
	return typ, typ
}

func summarizeAWSSNS(raw map[string]any) (string, string) {
	typ := str(raw, "Type")
	if typ == "" {
		return "webhook", "aws event"
	}
	switch typ {
	case "SubscriptionConfirmation":
		topic := str(raw, "TopicArn")
		return "subscription_confirmation", fmt.Sprintf("confirm subscription — %s", topic)
	case "Notification":
		subject := str(raw, "Subject")
		if subject != "" {
			return "notification", fmt.Sprintf("notification — %s", subject)
		}
		msg := str(raw, "Message")
		if len(msg) > 80 {
			msg = msg[:80] + "..."
		}
		if msg != "" {
			return "notification", fmt.Sprintf("notification — %s", msg)
		}
		return "notification", "SNS notification"
	}
	return typ, fmt.Sprintf("SNS %s", typ)
}

func summarizeTwitch(header http.Header, raw map[string]any) (string, string) {
	msgType := header.Get("Twitch-Eventsub-Message-Type")
	if sub, ok := raw["subscription"].(map[string]any); ok {
		typ := str(sub, "type")
		if typ != "" {
			if ev, ok := raw["event"].(map[string]any); ok {
				broadcaster := str(ev, "broadcaster_user_name")
				if broadcaster != "" {
					return typ, fmt.Sprintf("%s — %s", typ, broadcaster)
				}
			}
			return typ, typ
		}
	}
	if msgType != "" {
		return msgType, fmt.Sprintf("twitch %s", msgType)
	}
	return "webhook", "twitch event"
}

func summarizeHubSpot(raw map[string]any) (string, string) {
	// HubSpot sends an array of events
	if arr, ok := raw["subscriptionType"].(string); ok && arr != "" {
		objectId := str(raw, "objectId")
		if objectId != "" {
			return arr, fmt.Sprintf("%s — object %s", arr, objectId)
		}
		return arr, arr
	}
	return "webhook", "hubspot event"
}

func summarizeTypeform(raw map[string]any) (string, string) {
	eventType := str(raw, "event_type")
	if eventType == "" {
		return "webhook", "typeform event"
	}
	if formResp, ok := raw["form_response"].(map[string]any); ok {
		if defn, ok := formResp["definition"].(map[string]any); ok {
			title := str(defn, "title")
			if title != "" {
				return eventType, fmt.Sprintf("%s — %s", eventType, title)
			}
		}
	}
	return eventType, eventType
}

func summarizeSupabase(raw map[string]any) (string, string) {
	typ := strings.ToLower(str(raw, "type")) // INSERT, UPDATE, DELETE
	table := str(raw, "table")
	if typ == "" {
		typ = "webhook"
	}
	if table != "" {
		return typ, fmt.Sprintf("%s — %s", table, typ)
	}
	return typ, fmt.Sprintf("supabase %s", typ)
}

func summarizePostmark(raw map[string]any) (string, string) {
	recordType := str(raw, "RecordType")
	if recordType == "" {
		return "webhook", "postmark event"
	}
	recipient := str(raw, "Recipient")
	if recipient == "" {
		recipient = str(raw, "Email")
	}
	if recipient != "" {
		return recordType, fmt.Sprintf("%s — %s", recordType, recipient)
	}
	return recordType, recordType
}

func summarizeMailgun(raw map[string]any) (string, string) {
	if eventData, ok := raw["event-data"].(map[string]any); ok {
		event := str(eventData, "event")
		recipient := str(eventData, "recipient")
		if event != "" && recipient != "" {
			return event, fmt.Sprintf("%s — %s", event, recipient)
		}
		if event != "" {
			return event, event
		}
	}
	event := str(raw, "event")
	if event != "" {
		recipient := str(raw, "recipient")
		if recipient != "" {
			return event, fmt.Sprintf("%s — %s", event, recipient)
		}
		return event, event
	}
	return "webhook", "mailgun event"
}

func summarizeMeta(raw map[string]any) (string, string) {
	object := str(raw, "object")
	if object == "" {
		object = "meta"
	}
	if entries, ok := raw["entry"].([]any); ok && len(entries) > 0 {
		if entry, ok := entries[0].(map[string]any); ok {
			if changes, ok := entry["changes"].([]any); ok && len(changes) > 0 {
				if change, ok := changes[0].(map[string]any); ok {
					field := str(change, "field")
					if field != "" {
						return object, fmt.Sprintf("%s — %s changed", object, field)
					}
				}
			}
			if _, ok := entry["messaging"].([]any); ok {
				return object, fmt.Sprintf("%s — new message", object)
			}
		}
	}
	return object, fmt.Sprintf("%s webhook", object)
}

func summarizeIntercom(raw map[string]any) (string, string) {
	topic := str(raw, "topic")
	if topic == "" {
		return "webhook", "intercom event"
	}
	if data, ok := raw["data"].(map[string]any); ok {
		if item, ok := data["item"].(map[string]any); ok {
			for _, key := range []string{"title", "name", "subject", "body"} {
				if v := str(item, key); v != "" {
					if len(v) > 60 {
						v = v[:60] + "..."
					}
					return topic, fmt.Sprintf("%s — %s", topic, v)
				}
			}
		}
	}
	return topic, topic
}

func summarizeLemonSqueezy(raw map[string]any) (string, string) {
	eventName := ""
	if meta, ok := raw["meta"].(map[string]any); ok {
		eventName = str(meta, "event_name")
	}
	if eventName == "" {
		return "webhook", "lemonsqueezy event"
	}
	if data, ok := raw["data"].(map[string]any); ok {
		if attrs, ok := data["attributes"].(map[string]any); ok {
			if total := str(attrs, "total_formatted"); total != "" {
				return eventName, fmt.Sprintf("%s — %s", eventName, total)
			}
			if name := str(attrs, "product_name"); name != "" {
				return eventName, fmt.Sprintf("%s — %s", eventName, name)
			}
			if status := str(attrs, "status"); status != "" {
				return eventName, fmt.Sprintf("%s — %s", eventName, status)
			}
		}
	}
	return eventName, eventName
}

func summarizeNetlify(raw map[string]any) (string, string) {
	state := str(raw, "state")
	name := str(raw, "name")
	title := str(raw, "title")
	branch := str(raw, "branch")
	if state == "" && name == "" {
		return "webhook", "netlify event"
	}
	typ := "deploy"
	if state != "" {
		typ = state
	}
	parts := []string{typ}
	if name != "" {
		parts = append(parts, name)
	}
	if branch != "" {
		parts = append(parts, "on "+branch)
	}
	if title != "" && len(title) <= 60 {
		parts = append(parts, "— "+title)
	}
	return typ, strings.Join(parts, " ")
}

func summarizeRender(raw map[string]any) (string, string) {
	typ := str(raw, "type")
	if typ == "" {
		return "webhook", "render event"
	}
	if data, ok := raw["data"].(map[string]any); ok {
		serviceName := str(data, "serviceName")
		if serviceName == "" {
			if svc, ok := data["service"].(map[string]any); ok {
				serviceName = str(svc, "name")
			}
		}
		status := str(data, "status")
		if serviceName != "" && status != "" {
			return typ, fmt.Sprintf("%s — %s %s", typ, serviceName, status)
		}
		if serviceName != "" {
			return typ, fmt.Sprintf("%s — %s", typ, serviceName)
		}
	}
	return typ, typ
}

func summarizeNewRelic(raw map[string]any) (string, string) {
	conditionName := str(raw, "condition_name")
	currentState := str(raw, "current_state")
	severity := str(raw, "severity")
	if conditionName == "" && currentState == "" {
		if targets, ok := raw["targets"].([]any); ok && len(targets) > 0 {
			if t, ok := targets[0].(map[string]any); ok {
				name := str(t, "name")
				if name != "" {
					return "alert", fmt.Sprintf("alert — %s", name)
				}
			}
		}
		return "webhook", "newrelic event"
	}
	typ := "alert"
	if currentState != "" {
		typ = currentState
	}
	parts := []string{}
	if conditionName != "" {
		parts = append(parts, conditionName)
	}
	if severity != "" {
		parts = append(parts, fmt.Sprintf("[%s]", severity))
	}
	if currentState != "" && conditionName != "" {
		parts = append(parts, "— "+currentState)
	}
	if len(parts) > 0 {
		return typ, strings.Join(parts, " ")
	}
	return typ, typ
}

func summarizeGumroad(raw map[string]any) (string, string) {
	resourceName := str(raw, "resource_name")
	productName := str(raw, "product_name")
	if resourceName == "" {
		resourceName = "sale"
	}
	if productName != "" {
		if priceVal, ok := raw["price"].(float64); ok && priceVal > 0 {
			return resourceName, fmt.Sprintf("%s — %s ($%.2f)", resourceName, productName, priceVal/100)
		}
		return resourceName, fmt.Sprintf("%s — %s", resourceName, productName)
	}
	if email := str(raw, "email"); email != "" {
		return resourceName, fmt.Sprintf("%s — %s", resourceName, email)
	}
	return resourceName, fmt.Sprintf("gumroad %s", resourceName)
}

func summarizeClickUp(raw map[string]any) (string, string) {
	event := str(raw, "event")
	if event == "" {
		return "webhook", "clickup event"
	}
	taskID := str(raw, "task_id")
	if taskID != "" {
		return event, fmt.Sprintf("%s — task %s", event, taskID)
	}
	return event, event
}

func summarizeRailway(raw map[string]any) (string, string) {
	typ := str(raw, "type")
	if typ == "" {
		return "webhook", "railway event"
	}
	status := str(raw, "status")
	if project, ok := raw["project"].(map[string]any); ok {
		name := str(project, "name")
		if name != "" && status != "" {
			return typ, fmt.Sprintf("%s — %s %s", typ, name, status)
		}
		if name != "" {
			return typ, fmt.Sprintf("%s — %s", typ, name)
		}
	}
	if status != "" {
		return typ, fmt.Sprintf("%s — %s", typ, status)
	}
	return typ, typ
}

func summarizeBrevo(raw map[string]any) (string, string) {
	event := str(raw, "event")
	if event == "" {
		return "webhook", "brevo event"
	}
	email := str(raw, "email")
	subject := str(raw, "subject")
	if email != "" && subject != "" {
		if len(subject) > 40 {
			subject = subject[:40] + "..."
		}
		return event, fmt.Sprintf("%s — %s (%s)", event, email, subject)
	}
	if email != "" {
		return event, fmt.Sprintf("%s — %s", event, email)
	}
	return event, event
}

func summarizeDatadog(raw map[string]any) (string, string) {
	title := str(raw, "title")
	if title == "" {
		title = str(raw, "alert_title")
	}
	alertType := str(raw, "alert_type")
	if alertType == "" {
		alertType = "alert"
	}
	if title != "" {
		if len(title) > 80 {
			title = title[:80] + "..."
		}
		return alertType, fmt.Sprintf("%s — %s", alertType, title)
	}
	body := str(raw, "body")
	if body == "" {
		body = str(raw, "text")
	}
	if body != "" {
		if len(body) > 80 {
			body = body[:80] + "..."
		}
		return alertType, fmt.Sprintf("%s — %s", alertType, body)
	}
	return alertType, fmt.Sprintf("datadog %s", alertType)
}

func summarizeTikTok(raw map[string]any) (string, string) {
	event := str(raw, "event")
	if event == "" {
		return "webhook", "tiktok event"
	}
	return event, fmt.Sprintf("tiktok %s", event)
}

func summarizePipedrive(raw map[string]any) (string, string) {
	if meta, ok := raw["meta"].(map[string]any); ok {
		action := str(meta, "action")
		entity := str(meta, "entity")
		if action != "" && entity != "" {
			return action, fmt.Sprintf("%s %s", entity, action)
		}
		if action != "" {
			return action, fmt.Sprintf("pipedrive %s", action)
		}
	}
	if current, ok := raw["current"].(map[string]any); ok {
		if title := str(current, "title"); title != "" {
			return "updated", fmt.Sprintf("updated — %s", title)
		}
		if name := str(current, "name"); name != "" {
			return "updated", fmt.Sprintf("updated — %s", name)
		}
	}
	return "webhook", "pipedrive event"
}

func summarizeAsana(raw map[string]any) (string, string) {
	if events, ok := raw["events"].([]any); ok && len(events) > 0 {
		if ev, ok := events[0].(map[string]any); ok {
			action := str(ev, "action")
			resType := ""
			if res, ok := ev["resource"].(map[string]any); ok {
				resType = str(res, "resource_type")
			}
			if action != "" && resType != "" {
				return action, fmt.Sprintf("%s %s", resType, action)
			}
			if action != "" {
				return action, fmt.Sprintf("asana %s", action)
			}
		}
	}
	return "webhook", "asana event"
}

func summarizeWebflow(raw map[string]any) (string, string) {
	triggerType := str(raw, "triggerType")
	if triggerType == "" {
		return "webhook", "webflow event"
	}
	if payload, ok := raw["payload"].(map[string]any); ok {
		if name := str(payload, "name"); name != "" {
			return triggerType, fmt.Sprintf("%s — %s", triggerType, name)
		}
		if slug := str(payload, "slug"); slug != "" {
			return triggerType, fmt.Sprintf("%s — %s", triggerType, slug)
		}
	}
	return triggerType, triggerType
}

func summarizeKlaviyo(raw map[string]any) (string, string) {
	topic := str(raw, "topic")
	if topic == "" {
		return "webhook", "klaviyo event"
	}
	if data, ok := raw["data"].(map[string]any); ok {
		if email := str(data, "email"); email != "" {
			return topic, fmt.Sprintf("%s — %s", topic, email)
		}
	}
	return topic, topic
}

func summarizeSquarespace(raw map[string]any) (string, string) {
	topic := str(raw, "topic")
	if topic == "" {
		return "webhook", "squarespace event"
	}
	if data, ok := raw["data"].(map[string]any); ok {
		if orderNum := str(data, "orderNumber"); orderNum != "" {
			return topic, fmt.Sprintf("%s — order #%s", topic, orderNum)
		}
	}
	return topic, topic
}

func summarizeEcwid(raw map[string]any) (string, string) {
	eventType := str(raw, "eventType")
	if eventType == "" {
		return "webhook", "ecwid event"
	}
	entityID := str(raw, "entityId")
	if entityID != "" {
		return eventType, fmt.Sprintf("%s — #%s", eventType, entityID)
	}
	return eventType, eventType
}

func summarizeBox(raw map[string]any) (string, string) {
	trigger := str(raw, "trigger")
	if trigger == "" {
		return "webhook", "box event"
	}
	if source, ok := raw["source"].(map[string]any); ok {
		name := str(source, "name")
		srcType := str(source, "type")
		if name != "" {
			return trigger, fmt.Sprintf("%s — %s %s", trigger, srcType, name)
		}
	}
	return trigger, trigger
}

func summarizeHelpScout(raw map[string]any) (string, string) {
	topic := str(raw, "topic")
	if topic == "" {
		return "webhook", "helpscout event"
	}
	if payload, ok := raw["payload"].(map[string]any); ok {
		if subject := str(payload, "subject"); subject != "" {
			if len(subject) > 60 {
				subject = subject[:60] + "..."
			}
			return topic, fmt.Sprintf("%s — %s", topic, subject)
		}
	}
	return topic, topic
}

func summarizeSmartsheet(raw map[string]any) (string, string) {
	if events, ok := raw["events"].([]any); ok && len(events) > 0 {
		if ev, ok := events[0].(map[string]any); ok {
			objType := str(ev, "objectType")
			evtType := str(ev, "eventType")
			if objType != "" && evtType != "" {
				return evtType, fmt.Sprintf("%s %s", objType, evtType)
			}
		}
	}
	scope := str(raw, "scopeObjectId")
	if scope != "" {
		return "change", fmt.Sprintf("sheet %s changed", scope)
	}
	return "webhook", "smartsheet event"
}

func summarizeCalcom(raw map[string]any) (string, string) {
	triggerEvent := str(raw, "triggerEvent")
	if triggerEvent == "" {
		return "webhook", "calcom event"
	}
	if payload, ok := raw["payload"].(map[string]any); ok {
		title := str(payload, "title")
		if title != "" {
			return triggerEvent, fmt.Sprintf("%s — %s", triggerEvent, title)
		}
		if start := str(payload, "startTime"); start != "" {
			return triggerEvent, fmt.Sprintf("%s — %s", triggerEvent, start)
		}
	}
	return triggerEvent, triggerEvent
}

func summarizeMonday(raw map[string]any) (string, string) {
	if ev, ok := raw["event"].(map[string]any); ok {
		typ := str(ev, "type")
		boardID := str(ev, "boardId")
		if typ != "" && boardID != "" {
			return typ, fmt.Sprintf("%s — board %s", typ, boardID)
		}
		if typ != "" {
			return typ, fmt.Sprintf("monday %s", typ)
		}
	}
	return "webhook", "monday event"
}

func summarizeChargebee(raw map[string]any) (string, string) {
	eventType := str(raw, "event_type")
	if eventType == "" {
		return "webhook", "chargebee event"
	}
	if content, ok := raw["content"].(map[string]any); ok {
		if sub, ok := content["subscription"].(map[string]any); ok {
			if status := str(sub, "status"); status != "" {
				return eventType, fmt.Sprintf("%s — %s", eventType, status)
			}
			if planID := str(sub, "plan_id"); planID != "" {
				return eventType, fmt.Sprintf("%s — %s", eventType, planID)
			}
		}
		if customer, ok := content["customer"].(map[string]any); ok {
			if email := str(customer, "email"); email != "" {
				return eventType, fmt.Sprintf("%s — %s", eventType, email)
			}
		}
	}
	return eventType, eventType
}

func summarizeActiveCampaign(raw map[string]any) (string, string) {
	typ := str(raw, "type")
	if typ == "" {
		return "webhook", "activecampaign event"
	}
	if contact, ok := raw["contact"].(map[string]any); ok {
		email := str(contact, "email")
		if email != "" {
			return typ, fmt.Sprintf("%s — %s", typ, email)
		}
	}
	return typ, typ
}

func summarizeBasecamp(raw map[string]any) (string, string) {
	kind := str(raw, "kind")
	if kind == "" {
		return "webhook", "basecamp event"
	}
	if recording, ok := raw["recording"].(map[string]any); ok {
		title := str(recording, "title")
		if title != "" {
			return kind, fmt.Sprintf("%s — %s", kind, title)
		}
	}
	if creator, ok := raw["creator"].(map[string]any); ok {
		name := str(creator, "name")
		if name != "" {
			return kind, fmt.Sprintf("%s by %s", kind, name)
		}
	}
	return kind, kind
}

func summarizeTriggerDev(raw map[string]any) (string, string) {
	eventType := str(raw, "event")
	if eventType == "" {
		eventType = str(raw, "type")
	}
	if eventType == "" {
		return "webhook", "trigger.dev event"
	}
	// Try to get run/task info
	if payload, ok := raw["payload"].(map[string]any); ok {
		if taskID := str(payload, "taskIdentifier"); taskID != "" {
			return eventType, fmt.Sprintf("%s — %s", eventType, taskID)
		}
		if name := str(payload, "name"); name != "" {
			return eventType, fmt.Sprintf("%s — %s", eventType, name)
		}
		if id := str(payload, "id"); id != "" {
			return eventType, fmt.Sprintf("%s — %s", eventType, id)
		}
	}
	return eventType, eventType
}

func summarizeGeneric(source string, raw map[string]any) (string, string) {
	// Try common field names for event type
	for _, key := range []string{"type", "event", "event_type", "action", "topic"} {
		if v := str(raw, key); v != "" {
			return v, fmt.Sprintf("%s — %s", source, v)
		}
	}

	// Try to find a message or description
	for _, key := range []string{"message", "description", "summary", "text", "status"} {
		if v := str(raw, key); v != "" {
			if len(v) > 80 {
				v = v[:80] + "..."
			}
			return "webhook", fmt.Sprintf("%s — %s", source, v)
		}
	}

	return "webhook", fmt.Sprintf("%s event", source)
}

// str safely extracts a string from a map.
func str(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return fmt.Sprintf("%v", v)
	}
	return s
}

// zeroDecimalCurrencies lists Stripe currencies where amounts are already whole units.
var zeroDecimalCurrencies = map[string]bool{
	"BIF": true, "CLP": true, "DJF": true, "GNF": true, "JPY": true,
	"KMF": true, "KRW": true, "MGA": true, "PYG": true, "RWF": true,
	"UGX": true, "VND": true, "VUV": true, "XAF": true, "XOF": true, "XPF": true,
}

// formatAmount formats a Stripe amount for display.
func formatAmount(amount float64, currency string) string {
	if zeroDecimalCurrencies[currency] {
		return fmt.Sprintf("%.0f %s", amount, currency)
	}
	return fmt.Sprintf("%.2f %s", amount/100, currency)
}
