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

// formatAmount formats a cents amount to dollars.
func formatAmount(cents float64, currency string) string {
	return fmt.Sprintf("$%.2f %s", cents/100, currency)
}
