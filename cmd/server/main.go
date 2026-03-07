package main

import (
	"bytes"
	"compress/gzip"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"dread.sh/internal/config"
	"dread.sh/internal/event"
	"dread.sh/internal/hub"
	"dread.sh/internal/store"
	"dread.sh/internal/webhook"
)

var serverStartTime = time.Now()

func main() {
	cfgPath := flag.String("config", "", "path to config file (optional if env vars set)")
	flag.Parse()

	var cfg *config.Config
	if *cfgPath != "" {
		var err error
		cfg, err = config.Load(*cfgPath)
		if err != nil {
			log.Fatalf("config: %v", err)
		}
	} else {
		cfg = config.LoadFromEnv()
	}

	db, err := store.New(cfg.Server.DB)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer db.Close()

	// Run retention purge on startup
	if cfg.Server.RetentionDays > 0 {
		maxAge := time.Duration(cfg.Server.RetentionDays) * 24 * time.Hour
		deleted, err := db.Purge(maxAge)
		if err != nil {
			log.Printf("retention purge error: %v", err)
		} else if deleted > 0 {
			log.Printf("retention purge: deleted %d events older than %d days", deleted, cfg.Server.RetentionDays)
		}
	}

	h := hub.New()

	mux := http.NewServeMux()

	// D_ icon for notifications
	mux.HandleFunc("GET /icon.png", func(w http.ResponseWriter, r *http.Request) {
		const size = 256
		black := color.NRGBA{R: 0, G: 0, B: 0, A: 255}
		white := color.NRGBA{R: 255, G: 255, B: 255, A: 255}
		const (
			left = 28.0; top = 44.0; bottom = 200.0; strokeW = 30.0
			curveX = 82.0; midY = 122.0; outerR = 78.0; innerR = 48.0
			usLeft = 172.0; usRight = 224.0; usTop = 184.0; usBottom = 200.0
		)
		img := image.NewNRGBA(image.Rect(0, 0, size, size))
		for py := 0; py < size; py++ {
			for px := 0; px < size; px++ {
				x, y := float64(px), float64(py)
				inOuter := y >= top && y <= bottom && x >= left &&
					(x <= curveX || (x-curveX)*(x-curveX)+(y-midY)*(y-midY) <= outerR*outerR)
				inInner := y >= top+strokeW && y <= bottom-strokeW && x >= left+strokeW &&
					(x <= curveX || (x-curveX)*(x-curveX)+(y-midY)*(y-midY) <= innerR*innerR)
				inUS := x >= usLeft && x <= usRight && y >= usTop && y <= usBottom
				if (inOuter && !inInner) || inUS {
					img.SetNRGBA(px, py, white)
				} else {
					img.SetNRGBA(px, py, black)
				}
			}
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		png.Encode(w, img)
	})

	// Health endpoint
	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"latest": "0.1.0"})
	})

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		uptime := time.Since(serverStartTime).Truncate(time.Second).String()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"uptime": uptime,
			"events": db.EventCount(),
		})
	})

	// Webhook endpoint — any channel
	mux.HandleFunc("POST /wh/{channel}", webhook.MakeHandler(func(channel string, ev *event.Event) {
		if err := db.Insert(channel, ev); err != nil {
			log.Printf("store insert: %v", err)
		}
		h.Broadcast(channel, ev)
	}))

	// History API — supports multiple channels
	mux.HandleFunc("GET /api/events", func(w http.ResponseWriter, r *http.Request) {
		channelsParam := r.URL.Query().Get("channels")
		if channelsParam == "" {
			http.Error(w, "missing channels parameter", http.StatusBadRequest)
			return
		}
		channels := strings.Split(channelsParam, ",")

		var before time.Time
		if b := r.URL.Query().Get("before"); b != "" {
			before, _ = time.Parse(time.RFC3339Nano, b)
		}
		limit := 50
		if l := r.URL.Query().Get("limit"); l != "" {
			if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 200 {
				limit = n
			}
		}

		events, err := db.List(channels, before, limit)
		if err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			log.Printf("list events: %v", err)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(hub.Message{
			Type:    hub.MsgTypeHistory,
			Events:  events,
			HasMore: len(events) == limit,
		})
	})

	// Single event lookup
	mux.HandleFunc("GET /api/events/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		ev, err := db.GetByID(id)
		if err != nil {
			http.Error(w, "event not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ev)
	})

	// WebSocket — supports multiple channels
	mux.HandleFunc("GET /ws", h.HandleWS(cfg.Server.BaseURL))

	// Install stats
	mux.HandleFunc("GET /api/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(db.GetStats())
	})

	mux.HandleFunc("GET /api/live-stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=30")
		stats := db.LiveStats()
		uptime := time.Since(serverStartTime)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"EventsWeek":  stats.EventsWeek,
			"EventsTotal": stats.EventsTotal,
			"UptimeDays":  int(uptime.Hours() / 24),
			"UptimeHours": int(uptime.Hours()),
		})
	})

	// Workspace API — save workspace
	mux.HandleFunc("PUT /api/workspaces/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		// Validate it's valid JSON with a channels array
		var payload struct {
			Channels json.RawMessage `json:"channels"`
			Sound    string          `json:"sound"`
		}
		if err := json.Unmarshal(body, &payload); err != nil || payload.Channels == nil {
			http.Error(w, "invalid payload: requires {\"channels\":[...]}", http.StatusBadRequest)
			return
		}
		if err := db.SaveWorkspace(id, string(payload.Channels), payload.Sound); err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			log.Printf("save workspace: %v", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// Workspace API — get workspace
	mux.HandleFunc("GET /api/workspaces/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		ws, err := db.GetWorkspace(id)
		if err != nil {
			http.Error(w, "workspace not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		soundJSON := `""`
		if ws.Sound != "" {
			b, _ := json.Marshal(ws.Sound)
			soundJSON = string(b)
		}
		w.Write([]byte(`{"channels":` + ws.Channels + `,"sound":` + soundJSON + `}`))
	})

	// Export API — download events as JSON or CSV
	mux.HandleFunc("GET /api/export", func(w http.ResponseWriter, r *http.Request) {
		channelsParam := r.URL.Query().Get("channels")
		if channelsParam == "" {
			http.Error(w, "missing channels parameter", http.StatusBadRequest)
			return
		}
		channels := strings.Split(channelsParam, ",")
		format := r.URL.Query().Get("format")
		if format == "" {
			format = "json"
		}

		events, err := db.List(channels, time.Time{}, 1000)
		if err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			log.Printf("export events: %v", err)
			return
		}

		switch format {
		case "csv":
			w.Header().Set("Content-Type", "text/csv")
			w.Header().Set("Content-Disposition", "attachment; filename=dread-events.csv")
			cw := csv.NewWriter(w)
			cw.Write([]string{"id", "channel", "source", "type", "summary", "timestamp"})
			for _, e := range events {
				cw.Write([]string{e.ID, e.Channel, e.Source, e.Type, e.Summary, e.Timestamp.UTC().Format(time.RFC3339)})
			}
			cw.Flush()
		default:
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Disposition", "attachment; filename=dread-events.json")
			json.NewEncoder(w).Encode(events)
		}
	})

	// Replay API — re-forward an event to a URL
	mux.HandleFunc("POST /api/replay", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			EventID string `json:"event_id"`
			URL     string `json:"url"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if req.EventID == "" || req.URL == "" {
			http.Error(w, "event_id and url required", http.StatusBadRequest)
			return
		}
		ev, err := db.GetByID(req.EventID)
		if err != nil {
			http.Error(w, "event not found", http.StatusNotFound)
			return
		}
		fwdReq, err := http.NewRequest("POST", req.URL, bytes.NewBufferString(ev.RawJSON))
		if err != nil {
			http.Error(w, "invalid URL", http.StatusBadRequest)
			return
		}
		fwdReq.Header.Set("Content-Type", "application/json")
		fwdReq.Header.Set("X-Dread-Source", ev.Source)
		fwdReq.Header.Set("X-Dread-Event-ID", ev.ID)
		fwdReq.Header.Set("X-Dread-Replay", "true")
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(fwdReq)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
			return
		}
		resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "status": resp.StatusCode})
	})

	// Digest API — summary of recent events
	mux.HandleFunc("GET /api/digest", func(w http.ResponseWriter, r *http.Request) {
		channelsParam := r.URL.Query().Get("channels")
		if channelsParam == "" {
			http.Error(w, "missing channels parameter", http.StatusBadRequest)
			return
		}
		channels := strings.Split(channelsParam, ",")
		hours := 24
		if h := r.URL.Query().Get("hours"); h != "" {
			if n, err := strconv.Atoi(h); err == nil && n > 0 && n <= 720 {
				hours = n
			}
		}
		since := time.Now().UTC().Add(-time.Duration(hours) * time.Hour)
		total, bySrc, top, err := db.DigestStats(channels, since)
		if err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			log.Printf("digest: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"total":     total,
			"by_source": bySrc,
			"top":       top,
			"period":    fmt.Sprintf("last %dh", hours),
		})
	})

	// Status API — last event per channel for a workspace
	mux.HandleFunc("GET /api/status/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		ws, err := db.GetWorkspace(id)
		if err != nil {
			http.Error(w, "workspace not found", http.StatusNotFound)
			return
		}
		var channels []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal([]byte(ws.Channels), &channels); err != nil {
			http.Error(w, "invalid workspace data", http.StatusInternalServerError)
			return
		}
		chIDs := make([]string, len(channels))
		for i, ch := range channels {
			chIDs[i] = ch.ID
		}
		lastEvents, err := db.LastEventPerChannel(chIDs)
		if err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		type channelStatus struct {
			ID        string       `json:"id"`
			Name      string       `json:"name"`
			LastEvent *event.Event `json:"last_event,omitempty"`
		}
		result := make([]channelStatus, len(channels))
		for i, ch := range channels {
			result[i] = channelStatus{ID: ch.ID, Name: ch.Name, LastEvent: lastEvents[ch.ID]}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	})

	// Install script
	mux.HandleFunc("GET /install", func(w http.ResponseWriter, r *http.Request) {
		db.Increment("install_downloads")
		ip := r.Header.Get("Fly-Client-IP")
		if ip == "" {
			ip = r.Header.Get("X-Forwarded-For")
			if i := strings.Index(ip, ","); i != -1 {
				ip = ip[:i]
			}
		}
		if ip == "" {
			ip, _, _ = strings.Cut(r.RemoteAddr, ":")
		}
		log.Printf("install: ip=%s ua=%s", strings.TrimSpace(ip), r.UserAgent())
		db.TrackUniqueInstall(strings.TrimSpace(ip))
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(installScript))
	})

	// Install completed — phone-home from install script
	mux.HandleFunc("POST /api/installed", func(w http.ResponseWriter, r *http.Request) {
		db.Increment("installs")
		ip := r.Header.Get("Fly-Client-IP")
		if ip == "" {
			ip = r.Header.Get("X-Forwarded-For")
			if i := strings.Index(ip, ","); i != -1 {
				ip = ip[:i]
			}
		}
		if ip == "" {
			ip, _, _ = strings.Cut(r.RemoteAddr, ":")
		}
		log.Printf("installed: ip=%s ua=%s", strings.TrimSpace(ip), r.UserAgent())
		w.WriteHeader(http.StatusNoContent)
	})

	// Build pages with shared nav component
	builtLanding := buildPage(landingPage)
	builtDocs := buildPage(docsPage)
	builtChangelog := buildPage(changelogPage)
	builtDashboard := buildPage(dashboardPage)
	builtHowTo := buildPage(howToPage)
	builtStatusTemplate := buildPage(statusPage)

	// Dashboard page
	mux.HandleFunc("GET /dashboard", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(builtDashboard))
	})

	// Changelog page
	mux.HandleFunc("GET /changelog", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(builtChangelog))
	})

	// Documentation page
	mux.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(builtDocs))
	})

	// How To page
	mux.HandleFunc("GET /howto", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(builtHowTo))
	})

	// Status page (public, per workspace)
	mux.HandleFunc("GET /status/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		page := strings.Replace(builtStatusTemplate, "{{WORKSPACE_ID}}", id, -1)
		w.Write([]byte(page))
	})

	// Download page with live install counter
	builtDownload := buildPage(downloadPage)
	mux.HandleFunc("GET /download", func(w http.ResponseWriter, r *http.Request) {
		stats := db.GetStats()
		page := strings.Replace(builtDownload, "{{UNIQUE_COUNT}}", fmt.Sprintf("%d", stats["unique_installs"]), 1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(page))
	})

	// robots.txt — allow all AI crawlers
	mux.HandleFunc("GET /robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write([]byte(`# dread.sh — all crawlers welcome
User-agent: *
Allow: /

# AI search crawlers
User-agent: GPTBot
Allow: /

User-agent: ClaudeBot
Allow: /

User-agent: Claude-SearchBot
Allow: /

User-agent: Claude-User
Allow: /

User-agent: OAI-SearchBot
Allow: /

User-agent: ChatGPT-User
Allow: /

User-agent: PerplexityBot
Allow: /

User-agent: Perplexity-User
Allow: /

User-agent: Google-Extended
Allow: /

User-agent: Applebot-Extended
Allow: /

User-agent: DuckAssistBot
Allow: /

User-agent: Amazonbot
Allow: /

User-agent: cohere-ai
Allow: /

User-agent: Meta-ExternalAgent
Allow: /

Sitemap: https://dread.sh/sitemap.xml
`))
	})

	// sitemap.xml
	mux.HandleFunc("GET /sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url>
    <loc>https://dread.sh/</loc>
    <lastmod>2026-03-07</lastmod>
    <changefreq>weekly</changefreq>
    <priority>1.0</priority>
  </url>
  <url>
    <loc>https://dread.sh/docs</loc>
    <lastmod>2026-03-07</lastmod>
    <changefreq>weekly</changefreq>
    <priority>0.9</priority>
  </url>
  <url>
    <loc>https://dread.sh/download</loc>
    <lastmod>2026-03-07</lastmod>
    <changefreq>monthly</changefreq>
    <priority>0.8</priority>
  </url>
  <url>
    <loc>https://dread.sh/changelog</loc>
    <lastmod>2026-03-07</lastmod>
    <changefreq>weekly</changefreq>
    <priority>0.7</priority>
  </url>
  <url>
    <loc>https://dread.sh/howto</loc>
    <lastmod>2026-03-07</lastmod>
    <changefreq>monthly</changefreq>
    <priority>0.6</priority>
  </url>
  <url>
    <loc>https://dread.sh/dashboard</loc>
    <lastmod>2026-03-07</lastmod>
    <changefreq>daily</changefreq>
    <priority>0.5</priority>
  </url>
  <url>
    <loc>https://dread.sh/blog</loc>
    <lastmod>2026-03-07</lastmod>
    <changefreq>weekly</changefreq>
    <priority>0.8</priority>
  </url>
  <url>
    <loc>https://dread.sh/blog/webhook-vs-polling</loc>
    <lastmod>2026-03-07</lastmod>
    <changefreq>monthly</changefreq>
    <priority>0.7</priority>
  </url>
  <url>
    <loc>https://dread.sh/blog/test-webhooks-locally</loc>
    <lastmod>2026-03-07</lastmod>
    <changefreq>monthly</changefreq>
    <priority>0.7</priority>
  </url>
  <url>
    <loc>https://dread.sh/blog/stripe-webhook-setup</loc>
    <lastmod>2026-03-07</lastmod>
    <changefreq>monthly</changefreq>
    <priority>0.7</priority>
  </url>
</urlset>
`))
	})

	// llms.txt — structured overview for LLMs
	mux.HandleFunc("GET /llms.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write([]byte(`# dread.sh

> dread is a webhook relay and notification tool. It captures webhooks from any source (Stripe, GitHub, Sentry, etc.) and delivers real-time desktop notifications, a live terminal UI, and forwards to Slack or Discord. Install with one command, share your setup with your team.

## Docs

- [Documentation](https://dread.sh/docs): Complete reference for all CLI commands, configuration, and webhook setup
- [How To Guide](https://dread.sh/howto): Step-by-step setup guides for common integrations
- [Changelog](https://dread.sh/changelog): Version history and release notes

## Getting Started

- [Install](https://dread.sh/install): Shell install script — run curl -sSL dread.sh/install | sh
- [Download](https://dread.sh/download): Download page with install instructions
- [Dashboard](https://dread.sh/dashboard): Live web dashboard for viewing webhook events in real time

## Key Features

- Real-time desktop notifications for incoming webhooks (macOS and Linux)
- Terminal UI with per-source sparklines, event timeline, and payload viewer
- Slack and Discord forwarding with rich formatting
- Background watch mode with auto-updates
- Team workspace sharing with follows
- Threshold alerts for event volume spikes
- Webhook status pages for uptime monitoring
- Bookmark and diff view for comparing payloads

## How It Works

1. Create a channel: dread init
2. Point your webhook URL to https://dread.sh/wh/YOUR_CHANNEL_ID?source=stripe
3. Run dread to see events in the terminal, or dread watch for background notifications
4. Forward to Slack/Discord with dread config --slack WEBHOOK_URL

## Blog

- [Webhook vs Polling](https://dread.sh/blog/webhook-vs-polling): When to use webhooks instead of polling, with real-world examples and performance comparisons
- [Test Webhooks Locally](https://dread.sh/blog/test-webhooks-locally): How to test and debug webhooks in local development without tunneling tools
- [Stripe Webhook Setup](https://dread.sh/blog/stripe-webhook-setup): Step-by-step guide to setting up Stripe webhooks with desktop notifications

## Optional

- [Landing Page](https://dread.sh/): Product overview and install instructions
- [Full LLM Content](https://dread.sh/llms-full.txt): Extended version of this file with complete documentation
`))
	})

	// llms-full.txt — complete content for LLM ingestion
	mux.HandleFunc("GET /llms-full.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write([]byte(`# dread.sh — Complete Documentation

> dread is a webhook relay and notification tool for developers and teams. It captures HTTP webhooks from any source and delivers real-time desktop notifications, a live terminal UI, and forwards to Slack or Discord.

## Installation

Install on macOS or Linux with one command:

    curl -sSL dread.sh/install | sh

This downloads the latest binary for your OS and architecture (supports darwin/amd64, darwin/arm64, linux/amd64, linux/arm64).

The CLI auto-updates itself when new versions are available.

## Quick Start

1. Run dread init to create a channel and get your webhook URL
2. Copy the webhook URL (e.g. https://dread.sh/wh/ch_xxx?source=stripe)
3. Paste it into your service's webhook settings (Stripe, GitHub, Sentry, etc.)
4. Run dread to open the terminal UI, or dread watch for background desktop notifications

## CLI Commands

- dread — Open the interactive terminal UI
- dread init — Create a new channel and get your webhook URL
- dread watch — Background mode with desktop notifications
- dread config — View and edit configuration
- dread config --slack WEBHOOK_URL — Set Slack forwarding
- dread config --discord WEBHOOK_URL — Set Discord forwarding
- dread config --sound SOUND — Set notification sound
- dread config --mute CHANNEL — Mute a channel
- dread config --follow WORKSPACE_ID — Follow another workspace
- dread version — Show version

## Terminal UI Features

The interactive TUI has 4 tabs:

### 1. Events Tab
- Live feed of incoming webhooks
- Color-coded by status (green=success, red=failure, yellow=info)
- Search/filter with /
- Bookmark events with b
- Full payload viewer with syntax highlighting

### 2. Sources Tab
- Per-source event sparklines
- Source activity over time

### 3. Diff Tab
- Compare two bookmarked payloads side by side
- See exactly what changed between webhooks

### 4. Channels Tab
- Channel activity timeline (24 slots, 5-minute buckets)
- Event counts and last activity time

### Keyboard Shortcuts
- Tab/Shift+Tab — Switch between tabs
- j/k or Up/Down — Navigate events
- Enter — View event payload
- / — Search/filter events
- b — Bookmark current event
- d — Toggle diff view
- q or Ctrl+C — Quit
- ? — Show command palette

## Configuration

Config is stored at ~/.config/dread/config.json with these fields:

- channels — Array of {id, name} for your webhook channels
- slack_url — Slack incoming webhook URL for forwarding
- discord_url — Discord webhook URL for forwarding
- sound — Notification sound name (macOS: Basso, Blow, Bottle, Frog, Funk, Glass, Hero, Morse, Ping, Pop, Purr, Sosumi, Submarine, Tink)
- muted — Array of channel IDs to mute
- follows — Array of workspace IDs to follow
- alerts — Array of {pattern, threshold, window_minutes} for threshold alerts

## Webhook URL Format

    https://dread.sh/wh/CHANNEL_ID?source=SOURCE_NAME

The source parameter is optional but recommended — it labels events in the UI and notifications.

## Forwarding

### Slack
Events are forwarded as rich Slack messages with channel name, source, and summary.
Set with: dread config --slack https://hooks.slack.com/services/xxx

### Discord
Events are forwarded as Discord embeds with the dread brand color.
Set with: dread config --discord https://discord.com/api/webhooks/xxx

## Threshold Alerts

Configure alerts that fire when event volume exceeds a threshold:

    "alerts": [
      {"pattern": "error", "threshold": 10, "window_minutes": 5}
    ]

This sends a notification when 10+ events matching "error" arrive within 5 minutes.

## Team Workspaces

Share your channel setup with teammates:
1. One person runs dread init and configures channels
2. They share their workspace ID
3. Teammates run dread config --follow WORKSPACE_ID
4. Everyone sees the same channels and events

## Status Pages

Each workspace gets a public status page at:

    https://dread.sh/status/WORKSPACE_ID

Shows channel health and recent event activity.

## Web Dashboard

View events in a browser at https://dread.sh/dashboard

Features: real-time event feed, payload viewer with copy button, per-source color coding, search and filtering.

## Auto-Updates

The dread binary checks for updates on each run and automatically downloads and installs new versions from GitHub Releases.

## Supported Platforms

- macOS (Intel and Apple Silicon)
- Linux (x86_64 and ARM64)
`))
	})

	// security.txt
	mux.HandleFunc("GET /.well-known/security.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write([]byte(`Contact: mailto:security@dread.sh
Expires: 2027-03-07T00:00:00Z
Preferred-Languages: en
Canonical: https://dread.sh/.well-known/security.txt
`))
	})

	// Blog pages
	builtBlogIndex := buildPage(blogIndexPage)
	builtBlogWebhookVsPolling := buildPage(blogWebhookVsPolling)
	builtBlogTestWebhooksLocally := buildPage(blogTestWebhooksLocally)
	builtBlogStripeWebhooks := buildPage(blogStripeWebhooks)

	mux.HandleFunc("GET /blog", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(builtBlogIndex))
	})
	mux.HandleFunc("GET /blog/webhook-vs-polling", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(builtBlogWebhookVsPolling))
	})
	mux.HandleFunc("GET /blog/test-webhooks-locally", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(builtBlogTestWebhooksLocally))
	})
	mux.HandleFunc("GET /blog/stripe-webhook-setup", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(builtBlogStripeWebhooks))
	})

	// Landing page
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(builtLanding))
	})

	server := &http.Server{
		Addr:    cfg.Server.Addr,
		Handler: gzipMiddleware(mux),
	}

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("shutting down...")
		server.Close()
	}()

	log.Printf("dread server listening on %s", cfg.Server.Addr)
	log.Printf("webhook base URL: %s", cfg.Server.BaseURL)
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("server: %v", err)
	}
}

const navCSS = `
  nav {
    position: sticky; top: 0; z-index: 100;
    border-bottom: 1px solid var(--border);
    background: var(--nav-bg);
    backdrop-filter: blur(16px);
    -webkit-backdrop-filter: blur(16px);
  }
  .nav-inner {
    max-width: 1080px; margin: 0 auto;
    padding: 0 24px; height: 56px;
    display: flex; align-items: center; justify-content: space-between;
  }
  .nav-brand {
    font-family: "Press Start 2P", monospace;
    font-size: 1.15rem; color: var(--accent);
    text-decoration: none; letter-spacing: 0.05em;
  }
  .nav-links { display: flex; gap: 24px; align-items: center; }
  .nav-links a {
    font-size: 0.85rem; color: var(--text-muted);
    text-decoration: none; transition: color 0.15s;
  }
  .nav-links a:hover { color: var(--text); }
  .nav-btn {
    background: none; border: none; cursor: pointer;
    color: var(--text-muted); display: flex; align-items: center;
    justify-content: center; padding: 6px; border-radius: 6px;
    transition: color 0.15s, background 0.15s;
  }
  .nav-btn:hover { color: var(--text); background: var(--surface); }
  .nav-btn svg { width: 18px; height: 18px; }
  .docs-menu-btn {
    display: none; background: none; border: 1px solid var(--border);
    border-radius: 6px; padding: 6px 8px; cursor: pointer;
    color: var(--text-muted); align-items: center; justify-content: center;
    margin-right: 12px;
  }
  .docs-menu-btn svg { width: 18px; height: 18px; }
  .docs-menu-btn:hover { color: var(--text); background: var(--surface); }
`

const navHTML = `<nav>
  <div class="nav-inner">
    <button class="docs-menu-btn" id="menu-btn" aria-label="Toggle menu"><i data-lucide="menu"></i></button>
    <a href="/" class="nav-brand">DREAD</a>
    <div class="nav-links">
      <a href="/docs">Documentation</a>
      <a href="/blog">Blog</a>
      <a href="/changelog">Changelog</a>
      <a href="/howto">How To</a>
      <a href="/dashboard">Dashboard</a>
      <button class="nav-btn" onclick="toggleTheme()" aria-label="Toggle theme"><i data-lucide="moon" id="theme-icon"></i></button>
      <iframe src="https://ghbtns.com/github-btn.html?user=nigel-engel&repo=dread.sh&type=star&count=true" frameborder="0" scrolling="0" width="150" height="20" title="GitHub" style="vertical-align:middle;"></iframe>
    </div>
  </div>
</nav>`

func buildPage(template string) string {
	s := strings.Replace(template, "/*! NAV_CSS */", navCSS, 1)
	s = strings.Replace(s, "/*! BLOG_CSS */", blogCSS, 1)
	s = strings.Replace(s, "<!-- NAV_HTML -->", navHTML, 1)
	return s
}

var gzipPool = sync.Pool{
	New: func() any { w, _ := gzip.NewWriterLevel(nil, gzip.DefaultCompression); return w },
}

type gzipResponseWriter struct {
	http.ResponseWriter
	gz *gzip.Writer
}

func (w *gzipResponseWriter) Write(b []byte) (int, error) { return w.gz.Write(b) }

func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") ||
			r.Header.Get("Upgrade") == "websocket" {
			next.ServeHTTP(w, r)
			return
		}
		gz := gzipPool.Get().(*gzip.Writer)
		defer gzipPool.Put(gz)
		gz.Reset(w)
		defer gz.Close()
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Vary", "Accept-Encoding")
		w.Header().Del("Content-Length")
		next.ServeHTTP(&gzipResponseWriter{ResponseWriter: w, gz: gz}, r)
	})
}

const landingPage = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="description" content="dread is a webhook relay that sends desktop notifications and a live terminal feed from Stripe, GitHub, Sentry, and any webhook source. Install with one command, forward to Slack and Discord.">
<link rel="canonical" href="https://dread.sh/">
<meta property="og:type" content="website">
<meta property="og:site_name" content="dread.sh">
<meta property="og:title" content="Webhook Notifications for Your Terminal and Desktop - dread.sh">
<meta property="og:description" content="Get desktop notifications and a live terminal feed from Stripe, GitHub, Sentry, and any webhook source. Forward to Slack and Discord. Install with one command.">
<meta property="og:url" content="https://dread.sh/">
<meta property="og:image" content="https://dread.sh/og.png">
<meta property="og:image:width" content="1200">
<meta property="og:image:height" content="630">
<meta name="twitter:card" content="summary_large_image">
<meta name="twitter:title" content="Webhook Notifications for Your Terminal and Desktop - dread.sh">
<meta name="twitter:description" content="Get desktop notifications and a live terminal feed from any webhook source. Forward to Slack and Discord.">
<meta name="twitter:image" content="https://dread.sh/og.png">
<script type="application/ld+json">
{
  "@context": "https://schema.org",
  "@type": "SoftwareApplication",
  "name": "dread",
  "description": "Webhook relay and notification tool. Captures webhooks from Stripe, GitHub, Sentry, and any source, then delivers desktop notifications, a live terminal UI, and forwards to Slack or Discord.",
  "applicationCategory": "DeveloperApplication",
  "operatingSystem": ["macOS", "Linux"],
  "url": "https://dread.sh",
  "downloadUrl": "https://dread.sh/download",
  "installUrl": "https://dread.sh/install",
  "releaseNotes": "https://dread.sh/changelog",
  "offers": {
    "@type": "Offer",
    "price": "0",
    "priceCurrency": "USD"
  },
  "author": {
    "@type": "Organization",
    "name": "dread.sh",
    "url": "https://dread.sh"
  }
}
</script>
<script type="application/ld+json">
{
  "@context": "https://schema.org",
  "@type": "FAQPage",
  "mainEntity": [
    {
      "@type": "Question",
      "name": "What is dread.sh?",
      "acceptedAnswer": {
        "@type": "Answer",
        "text": "dread is a webhook relay and notification tool for developers. It captures webhooks from any source (Stripe, GitHub, Sentry, etc.) and delivers real-time desktop notifications, a live terminal UI with sparklines and payload viewer, and forwards to Slack or Discord."
      }
    },
    {
      "@type": "Question",
      "name": "How do I install dread?",
      "acceptedAnswer": {
        "@type": "Answer",
        "text": "Run curl -sSL dread.sh/install | sh to install on macOS or Linux. Supports Intel, Apple Silicon, and ARM64."
      }
    },
    {
      "@type": "Question",
      "name": "How do I set up a webhook with dread?",
      "acceptedAnswer": {
        "@type": "Answer",
        "text": "Run dread init to create a channel and get a webhook URL. Paste that URL into your service's webhook settings (e.g. Stripe, GitHub). Run dread to see events in the terminal, or dread watch for background desktop notifications."
      }
    },
    {
      "@type": "Question",
      "name": "Can dread forward webhooks to Slack or Discord?",
      "acceptedAnswer": {
        "@type": "Answer",
        "text": "Yes. Run dread config --slack WEBHOOK_URL or dread config --discord WEBHOOK_URL to forward all webhook events as rich messages to your Slack or Discord channels."
      }
    }
  ]
}
</script>
<script type="application/ld+json">
{
  "@context": "https://schema.org",
  "@type": "WebSite",
  "name": "dread.sh",
  "url": "https://dread.sh",
  "description": "Webhook relay and notification tool for developers",
  "publisher": {
    "@type": "Organization",
    "name": "dread.sh",
    "url": "https://dread.sh",
    "logo": {
      "@type": "ImageObject",
      "url": "https://dread.sh/icon.png"
    }
  }
}
</script>
<script>if(localStorage.getItem('theme')==='light')document.documentElement.classList.add('light')</script>
<link rel="icon" href="data:image/svg+xml,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 100 100'><circle cx='50' cy='50' r='40' fill='%23c37960'/></svg>">
<link rel="preconnect" href="https://fonts.googleapis.com" crossorigin>
<link rel="preconnect" href="https://cdn.jsdelivr.net" crossorigin>
<link rel="preconnect" href="https://unpkg.com" crossorigin>
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Press+Start+2P&display=swap">
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/geist@1.3.1/dist/fonts/geist-sans/style.min.css">
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/geist@1.3.1/dist/fonts/geist-mono/style.min.css">
<meta name="theme-color" content="#c37960">
<title>Webhook Notifications for Your Terminal and Desktop - dread.sh</title>
<script async src="https://www.googletagmanager.com/gtag/js?id=G-22TEKCP3M8"></script>
<script>
  window.dataLayer = window.dataLayer || [];
  function gtag(){dataLayer.push(arguments);}
  gtag('js', new Date());
  gtag('config', 'G-22TEKCP3M8');
</script>
<script src="https://unpkg.com/lucide@0.469.0/dist/umd/lucide.min.js"></script>
<style>
  :root {
    --bg: oklch(10% 0.003 256);
    --surface: oklch(16% 0.003 256);
    --surface-hover: oklch(20% 0.003 256);
    --border: oklch(23% 0.003 256);
    --border-subtle: oklch(18% 0.003 256);
    --text: oklch(98.5% 0.003 256);
    --text-secondary: oklch(70.5% 0.003 256);
    --text-muted: oklch(55.2% 0.003 256);
    --text-dim: oklch(40% 0.003 256);
    --accent: oklch(65% 0.1 40);
    --accent-dim: oklch(47% 0.09 36);
    --accent-glow: oklch(55% 0.1 38 / 0.15);
    --accent-glow-strong: oklch(55% 0.1 38 / 0.3);
    --orange: oklch(75% 0.18 55);
    --orange-dim: oklch(52% 0.16 55);
    --blue: oklch(70.7% 0.165 254.62);
    --violet: oklch(70.2% 0.183 293.54);
    --amber: oklch(82.8% 0.189 84.43);
    --rose: oklch(71.2% 0.194 13.43);
    --cyan: oklch(78.9% 0.154 211.53);
    --nav-bg: oklch(10% 0.003 256 / 0.85);
    --notif-bg: oklch(95% 0.003 256);
    --notif-text: oklch(20% 0.003 256);
    --notif-sub: oklch(40% 0.003 256);
    --notif-shadow: oklch(0% 0 0 / 0.4);
  }

  :root.light {
    --bg: oklch(98% 0.003 256);
    --surface: oklch(97% 0.003 256);
    --surface-hover: oklch(94% 0.003 256);
    --border: oklch(85% 0.003 256);
    --border-subtle: oklch(90% 0.003 256);
    --text: oklch(15% 0.003 256);
    --text-secondary: oklch(35% 0.003 256);
    --text-muted: oklch(50% 0.003 256);
    --text-dim: oklch(65% 0.003 256);
    --accent: oklch(50% 0.12 40);
    --accent-dim: oklch(40% 0.1 36);
    --accent-glow: oklch(50% 0.12 40 / 0.1);
    --accent-glow-strong: oklch(50% 0.12 40 / 0.2);
    --orange: oklch(55% 0.18 55);
    --orange-dim: oklch(45% 0.16 55);
    --blue: oklch(50% 0.165 254.62);
    --violet: oklch(50% 0.183 293.54);
    --amber: oklch(55% 0.189 84.43);
    --rose: oklch(55% 0.194 13.43);
    --cyan: oklch(50% 0.154 211.53);
    --nav-bg: oklch(98% 0.003 256 / 0.85);
    --notif-bg: oklch(100% 0 0);
    --notif-text: oklch(15% 0.003 256);
    --notif-sub: oklch(45% 0.003 256);
    --notif-shadow: oklch(0% 0 0 / 0.12);
  }

  * { margin: 0; padding: 0; box-sizing: border-box; }

  html, body { overscroll-behavior: none; }

  html { font-size: 18px; }

  body {
    font-family: "Geist", -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
    background: var(--bg);
    color: var(--text-secondary);
    line-height: 1.6;
    -webkit-font-smoothing: antialiased;
  }

  code, pre, kbd {
    font-family: "Geist Mono", ui-monospace, "Cascadia Code", "Source Code Pro", Menlo, Consolas, monospace;
  }

  /*! NAV_CSS */

  /* ---- HERO ---- */
  .hero {
    max-width: 1080px; margin: 0 auto;
    padding: 100px 24px 0;
    text-align: center;
    position: relative;
  }
  .release-bar {
    background: var(--accent);
    color: #fff;
    text-align: center;
    padding: 10px 24px;
    font-size: 0.85rem;
    font-weight: 600;
    letter-spacing: 0.01em;
    position: relative; z-index: 20;
    max-width: 1200px;
    margin: 16px auto 0;
    border-radius: 10px;
  }
  .release-bar a {
    color: #fff;
    text-decoration: underline;
    text-underline-offset: 3px;
  }
  .release-bar a:hover { opacity: 0.85; }
  .release-bar code {
    background: rgba(255,255,255,0.2);
    padding: 2px 6px;
    border-radius: 4px;
    font-size: 0.8rem;
  }
  .badge {
    display: inline-flex; align-items: center; gap: 8px;
    font-size: 0.8rem; color: var(--accent);
    border: 1px solid var(--accent-glow-strong);
    background: var(--accent-glow);
    padding: 4px 14px; border-radius: 20px;
    margin-bottom: 24px;
    position: relative; z-index: 1;
  }
  .badge-dot {
    width: 6px; height: 6px;
    background: var(--accent); border-radius: 50%;
    animation: pulse 2s ease-in-out infinite;
  }
  @keyframes pulse {
    0%, 100% { opacity: 1; }
    50% { opacity: 0.4; }
  }
  h1 {
    font-size: clamp(3rem, 7vw, 4.5rem);
    color: var(--text);
    font-weight: 700;
    letter-spacing: -0.03em;
    line-height: 1.08;
    margin-bottom: 24px;
    position: relative; z-index: 1;
  }
  h1 span { color: var(--text); }
  .hero-sub {
    font-size: 1.25rem;
    color: var(--text-muted);
    max-width: 720px; margin: 0 auto 44px;
    line-height: 1.7;
    position: relative; z-index: 1;
  }
  .hero-actions {
    display: flex; gap: 12px;
    justify-content: center;
    margin-bottom: 48px;
    position: relative; z-index: 1;
  }
  .hero-install {
    display: inline-flex; align-items: center; gap: 10px;
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 14px 24px;
    font-size: 0.9rem; color: var(--text);
    font-family: "Geist Mono", ui-monospace, monospace;
    cursor: pointer; transition: border-color 0.15s;
    user-select: none;
  }
  .hero-install:hover { border-color: var(--text-muted); }
  .hero-install .prompt { color: var(--text-dim); }
  .hero-install .pipe { color: var(--text-dim); }

  /* ---- LIVE STATS ---- */
  .live-stats {
    display: flex; justify-content: center; gap: 16px;
    margin-bottom: 40px;
    position: relative; z-index: 1;
  }
  .live-stat-card {
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: 12px;
    padding: 24px 32px;
    text-align: center;
    min-width: 180px;
    transition: border-color 0.15s;
  }
  .live-stat-card:hover { border-color: var(--accent-glow-strong); }
  .live-stat-value {
    font-size: 2.2rem;
    font-weight: 700;
    font-family: "Geist Mono", ui-monospace, monospace;
    color: var(--text);
    letter-spacing: -0.02em;
    line-height: 1;
  }
  .live-stat-value .accent { color: var(--accent); }
  .live-stat-label {
    font-size: 0.75rem;
    color: var(--text-dim);
    margin-top: 10px;
    text-transform: uppercase;
    letter-spacing: 0.06em;
  }
  .live-stat-detail {
    font-size: 0.7rem;
    color: var(--text-muted);
    margin-top: 4px;
    font-family: "Geist Mono", ui-monospace, monospace;
  }
  .live-stat-dot { display: inline-block; width: 6px; height: 6px; border-radius: 50%; background: var(--green); margin-right: 4px; animation: pulse-dot 2s ease-in-out infinite; }
  @keyframes pulse-dot { 0%, 100% { opacity: 1; } 50% { opacity: 0.3; } }
  @media (max-width: 640px) {
    .live-stats { flex-direction: column; gap: 12px; align-items: center; }
    .live-stat-card { min-width: 0; width: 100%; max-width: 280px; }
  }

  /* ---- SECTION ---- */
  .section {
    border-top: 1px solid var(--border-subtle);
    border-bottom: 1px solid var(--border-subtle);
  }
  .section-tint-1 { background: oklch(12% 0.003 256); }
  .section-tint-2 { background: oklch(13% 0.004 240); }
  .section-tint-3 { background: oklch(11.5% 0.003 270); }
  :root.light .section-tint-1 { background: oklch(96% 0.003 256); }
  :root.light .section-tint-2 { background: oklch(95% 0.004 240); }
  :root.light .section-tint-3 { background: oklch(96.5% 0.003 270); }
  .section-inner {
    max-width: 1080px; margin: 0 auto;
    padding: 96px 24px;
    border-left: 1px solid var(--border-subtle);
    border-right: 1px solid var(--border-subtle);
  }
  .section-label {
    font-size: 0.75rem; text-transform: uppercase;
    letter-spacing: 0.1em; color: var(--accent);
    margin-bottom: 14px;
  }
  .section-title {
    font-size: 2rem; color: var(--text);
    font-weight: 600; letter-spacing: -0.02em;
    margin-bottom: 16px; margin-top: 0;
  }
  .section-desc {
    color: var(--text-muted); font-size: 1rem;
    max-width: 620px; line-height: 1.7;
    margin-bottom: 52px;
  }

  /* ---- STEPS (quick start) ---- */
  .steps {
    display: grid; gap: 1px;
    background: var(--border-subtle);
    border: 1px solid var(--border);
    border-radius: 12px;
    overflow: hidden;
  }
  .step-row {
    display: grid;
    grid-template-columns: 200px 1fr;
    background: var(--bg);
  }
  .step-num {
    padding: 24px;
    border-right: 1px solid var(--border-subtle);
    display: flex; align-items: flex-start; gap: 12px;
  }
  .step-n {
    width: 26px; height: 26px;
    border-radius: 7px;
    display: flex; align-items: center; justify-content: center;
    font-size: 0.7rem; font-weight: 600;
    flex-shrink: 0;
    border: none;
  }
  .step-n-1 { background: var(--accent-glow-strong); color: var(--accent); }
  .step-n-2 { background: oklch(70.7% 0.165 254.62 / 0.2); color: var(--blue); }
  .step-n-3 { background: oklch(70.2% 0.183 293.54 / 0.2); color: var(--violet); }
  .step-label {
    font-size: 0.9rem; color: var(--text-secondary);
    padding-top: 3px;
  }
  .step-content {
    padding: 24px;
  }
  .step-content pre {
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 16px 20px;
    overflow-x: auto;
    font-size: 0.85rem;
  }
  .step-content code { color: var(--text); }

  @media (max-width: 640px) {
    .step-row { grid-template-columns: 1fr; }
    .step-num { border-right: none; border-bottom: 1px solid var(--border-subtle); padding: 16px 20px; }
    .step-content { padding: 20px; }
  }

  /* ---- WORKSPACE FLOW (2-col hero card) ---- */
  .flow-grid {
    display: grid; grid-template-columns: 1fr 1fr;
    gap: 28px;
  }
  @media (max-width: 720px) {
    .flow-grid { grid-template-columns: 1fr; }
  }
  .flow-card {
    background: transparent;
    border: 1px solid var(--border);
    border-radius: 12px;
    padding: 28px;
    position: relative;
    transition: border-color 0.2s;
  }
  .flow-card:hover { border-color: var(--text-dim); }
  .flow-card-icon {
    width: 40px; height: 40px;
    border-radius: 8px;
    display: flex; align-items: center; justify-content: center;
    margin-bottom: 18px;
  }
  .flow-card-icon svg { width: 20px; height: 20px; }
  .flow-card h3 {
    font-size: 1.05rem; color: var(--text);
    font-weight: 600; margin-bottom: 10px;
  }
  .flow-card p {
    font-size: 0.9rem; color: var(--text-muted);
    line-height: 1.6; margin-bottom: 18px;
  }
  .flow-card pre {
    background: var(--bg);
    border: 1px solid var(--border-subtle);
    border-radius: 8px;
    padding: 14px 16px;
    overflow-x: auto;
    font-size: 0.8rem;
  }
  .flow-card code { color: var(--text-secondary); }
  .flow-card-full { grid-column: 1 / -1; }
  .flow-card-full .flow-inner {
    display: grid; grid-template-columns: 1fr 1fr; gap: 24px;
    align-items: center;
  }
  @media (max-width: 720px) {
    .flow-card-full .flow-inner { grid-template-columns: 1fr; }
  }

  /* ---- FEATURE GRID ---- */
  .feat-grid {
    display: grid; grid-template-columns: repeat(3, 1fr);
    gap: 1px;
    background: var(--border-subtle);
    border: 1px solid var(--border);
    border-radius: 12px;
    overflow: hidden;
  }
  @media (max-width: 720px) {
    .feat-grid { grid-template-columns: 1fr 1fr; }
  }
  @media (max-width: 480px) {
    .feat-grid { grid-template-columns: 1fr; }
  }
  .feat {
    background: var(--bg);
    padding: 32px;
    transition: background 0.15s;
  }
  .feat:hover { background: var(--surface); }
  .feat-icon {
    width: 36px; height: 36px;
    border-radius: 8px;
    display: flex; align-items: center; justify-content: center;
    margin-bottom: 16px;
  }
  .feat-icon svg { width: 18px; height: 18px; }
  .feat h3 {
    font-size: 0.95rem; color: var(--text);
    font-weight: 500; margin-bottom: 8px;
  }
  .feat p {
    font-size: 0.85rem; color: var(--text-muted);
    line-height: 1.6;
  }
  .ic-green { background: oklch(65% 0.1 40 / 0.12); color: var(--accent); }
  .ic-blue { background: oklch(70.7% 0.165 254.62 / 0.12); color: var(--blue); }
  .ic-orange { background: oklch(75% 0.18 55 / 0.12); color: var(--orange); }
  .ic-violet { background: oklch(70.2% 0.183 293.54 / 0.12); color: var(--violet); }
  .ic-amber { background: oklch(82.8% 0.189 84.43 / 0.12); color: var(--amber); }
  .ic-rose { background: oklch(71.2% 0.194 13.43 / 0.12); color: var(--rose); }
  .ic-cyan { background: oklch(78.9% 0.154 211.53 / 0.12); color: var(--cyan); }

  /* ---- COMMANDS ---- */
  .cmd-grid {
    display: grid; grid-template-columns: 1fr 1fr;
    gap: 28px;
  }
  @media (max-width: 640px) {
    .cmd-grid { grid-template-columns: 1fr; }
  }
  .cmd-group {
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: 12px;
    overflow: hidden;
  }
  .cmd-group-header {
    padding: 14px 20px;
    font-size: 0.75rem; text-transform: uppercase;
    letter-spacing: 0.08em;
    color: var(--text-muted);
    border-bottom: 1px solid var(--border);
  }
  .cmd-group pre {
    padding: 16px 20px;
    font-size: 0.8rem;
    overflow-x: auto;
    background: transparent;
  }
  .cmd-group code { color: var(--text-secondary); }

  /* ---- CODE COLORS ---- */
  .c { color: var(--text-dim); }
  .o { color: var(--text-muted); }
  .h { color: var(--accent); }
  .kw { color: var(--orange); }
  .ws { color: var(--violet); }

  /* ---- WHY GRID ---- */
  .why-grid {
    display: grid; grid-template-columns: repeat(3, 1fr);
    gap: 28px;
  }
  @media (max-width: 720px) {
    .why-grid { grid-template-columns: 1fr; }
  }
  .why-card {
    background: transparent;
    border: 1px solid var(--border);
    border-radius: 12px;
    padding: 32px;
    transition: border-color 0.2s;
  }
  .why-card:hover { border-color: var(--text-dim); }
  .why-icon {
    width: 40px; height: 40px;
    border-radius: 8px;
    display: flex; align-items: center; justify-content: center;
    margin-bottom: 18px;
    background: var(--accent-glow); color: var(--accent);
  }
  .why-icon svg { width: 20px; height: 20px; }
  .why-card h3 {
    font-size: 1.05rem; color: var(--text);
    font-weight: 600; margin-bottom: 10px;
  }
  .why-card p {
    font-size: 0.9rem; color: var(--text-muted);
    line-height: 1.6;
  }

  /* ---- USE CASES ---- */
  .use-grid {
    display: flex; flex-direction: column; gap: 1px;
    background: var(--border); border: 1px solid var(--border);
    border-radius: 12px; overflow: hidden;
  }
  .use-card {
    display: grid; grid-template-columns: 40px 180px 1fr;
    align-items: center; gap: 24px;
    background: var(--bg); padding: 22px 28px;
    transition: background 0.15s;
  }
  .use-card:hover { background: var(--surface); }
  .use-icon {
    width: 40px; height: 40px;
    border-radius: 8px;
    display: flex; align-items: center; justify-content: center;
  }
  .use-icon svg { width: 20px; height: 20px; }
  .use-card h3 {
    font-size: 1rem; color: var(--text);
    font-weight: 600; margin: 0;
  }
  .use-card p {
    font-size: 0.85rem; color: var(--text-muted);
    line-height: 1.5;
  }
  @media (max-width: 720px) {
    .use-card { grid-template-columns: 40px 1fr; }
    .use-card p { grid-column: 1 / -1; }
  }
  .use-card code {
    font-size: 0.75rem; background: var(--surface);
    padding: 2px 6px; border-radius: 4px; color: var(--accent);
  }

  /* ---- LOGO MARQUEE ---- */
  .marquee-section {
    border-top: 1px solid var(--border-subtle);
    border-bottom: 1px solid var(--border-subtle);
  }
  .marquee-section-inner {
    max-width: 1080px; margin: 0 auto;
    padding: 48px 24px 24px;
    border-left: 1px solid var(--border-subtle);
    border-right: 1px solid var(--border-subtle);
    text-align: center;
  }
  .marquee-label {
    font-size: 0.75rem; text-transform: uppercase;
    letter-spacing: 0.1em; color: var(--accent);
    margin-bottom: 8px;
  }
  .marquee-title {
    font-size: 1.5rem; color: var(--text);
    font-weight: 600; letter-spacing: -0.02em;
    margin: 0 0 32px;
  }
  .marquee-wrap {
    --gap: 2.5rem;
    --duration: 35s;
    display: flex;
    overflow: hidden;
    gap: var(--gap);
    mask-image: linear-gradient(to right, transparent 0%, black 8%, black 92%, transparent 100%);
    -webkit-mask-image: linear-gradient(to right, transparent 0%, black 8%, black 92%, transparent 100%);
  }
  .marquee-wrap + .marquee-wrap { margin-top: 16px; }
  .marquee__track {
    display: flex;
    flex-shrink: 0;
    justify-content: space-around;
    gap: var(--gap);
    min-width: 100%;
    animation: marquee-scroll var(--duration) linear infinite;
  }
  .marquee-wrap--reverse .marquee__track {
    animation-direction: reverse;
  }
  @keyframes marquee-scroll {
    from { transform: translateX(0); }
    to { transform: translateX(calc(-100% - var(--gap))); }
  }
  .marquee-wrap:hover .marquee__track { animation-play-state: paused; }
  .marquee__item {
    display: inline-flex; align-items: center; gap: 8px;
    color: var(--text-dim); font-size: 0.8rem;
    white-space: nowrap; padding: 6px 0;
  }
  .marquee__item svg { flex-shrink: 0; opacity: 0.5; transition: opacity 0.2s; }
  .marquee__item:hover svg { opacity: 1; }
  .marquee__item:hover { color: var(--text-secondary); }
  @media (prefers-reduced-motion: reduce) {
    .marquee__track { animation-play-state: paused; }
  }

  /* ---- FOOTER ---- */
  footer {
    border-top: 1px solid var(--border-subtle);
    padding: 40px 0;
    margin-top: 0;
  }
  .footer-inner {
    max-width: 1080px; margin: 0 auto; padding: 0 24px;
    display: flex; justify-content: space-between;
    align-items: center;
  }
  .footer-brand { font-family: "Press Start 2P", monospace; font-size: 0.7rem; color: var(--text-dim); }
  .footer-links { display: flex; gap: 20px; }
  .footer-links a {
    font-size: 0.8rem; color: var(--text-dim);
    text-decoration: none; transition: color 0.15s;
  }
  .footer-links a:hover { color: var(--text-secondary); }
  .footer-status { display: flex; align-items: center; gap: 6px; font-size: 0.75rem; color: oklch(72% 0.19 145); }
  .footer-status-dot { width: 8px; height: 8px; border-radius: 50%; background: oklch(72% 0.19 145); display: inline-block; }

  /* ---- COPY BUTTON ---- */
  .copy-wrap { position: relative; }
  .copy-btn {
    position: absolute; top: 8px; right: 8px;
    background: var(--border);
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 5px 6px;
    cursor: pointer;
    display: flex; align-items: center; justify-content: center;
    color: var(--text-muted);
    opacity: 0; transition: opacity 0.15s, color 0.15s, background 0.15s;
  }
  .copy-wrap:hover .copy-btn { opacity: 1; }
  .copy-btn:hover { color: var(--text); background: var(--surface-hover); }
  .copy-btn svg { width: 14px; height: 14px; pointer-events: none; }
  .copy-btn.copied { color: var(--accent); }

  .hero-install { position: relative; }
  .hero-install .copy-btn {
    position: static;
    opacity: 0.6;
    background: transparent;
    border: none;
    padding: 2px;
    margin-left: 4px;
  }
  .hero-install:hover .copy-btn { opacity: 1; }
  .hero-install .copy-btn:hover { color: var(--text); background: transparent; }
  .hero-install .copy-btn.copied { color: var(--accent); opacity: 1; }

  /* ---- HERO RIGHT (terminal mockup) ---- */
  .hero-right { position: relative; margin: 0 -24px; }
  .terminal {
    background: var(--surface);
    border: 1px solid var(--border);
    border-bottom: none;
    border-radius: 12px 12px 0 0;
    overflow: hidden;
    font-family: "Geist Mono", ui-monospace, monospace;
    font-size: 0.75rem;
    line-height: 1.7;
  }
  .terminal-bar {
    display: flex; align-items: center; gap: 10px;
    padding: 10px 16px;
    border-bottom: 1px solid var(--border);
    color: var(--text-muted);
    font-size: 0.7rem;
  }
  .terminal-dots { display: flex; gap: 6px; }
  .terminal-dots span {
    width: 10px; height: 10px; border-radius: 50%;
  }
  .terminal-dots .dot-red { background: #FF5F57; }
  .terminal-dots .dot-yellow { background: #FEBC2E; }
  .terminal-dots .dot-green { background: #28C840; }
  .terminal-body { padding: 0; }
  .terminal-row {
    display: grid;
    grid-template-columns: 100px 90px 1fr;
    gap: 0;
    color: var(--text-secondary);
    border-bottom: 1px solid var(--border-subtle);
    text-align: left;
  }
  .terminal-row:last-child { border-bottom: none; }
  .terminal-row span {
    padding: 6px 12px;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .terminal-row .time { color: var(--text-dim); text-align: left; }
  .terminal-row .source-stripe { color: #635BFF; }
  .terminal-row .source-github { color: #238636; }
  .terminal-row .source-sentry { color: #F57C00; }
  .terminal-row .source-linear { color: #5E6AD2; }
  .terminal-row .source-shopify { color: #96BF48; }
  .terminal-row .source-supabase { color: #3ECF8E; }
  .terminal-row .source-vercel { color: var(--text-secondary); }
  .terminal-row .source-aws { color: #FF9900; }
  .terminal-footer {
    padding: 8px 12px;
    border-top: 1px solid var(--border-subtle);
    color: var(--text-dim);
    font-size: 0.65rem;
    text-align: left;
  }
</style>
</head>
<body>

<!-- NAV -->
<!-- NAV_HTML -->

<!-- RELEASE BAR -->
<div class="release-bar">
  🚀 <code>v0.3.0</code> is out — bookmarks, diff view, command palette, swimlane timeline, and more. <a href="/changelog">Read the changelog →</a>
</div>

<!-- HERO -->
<div class="hero">
  <div class="badge"><span class="badge-dot"></span> developer tool for teams</div>
  <h1>Webhook notifications<br>to your terminal and desktop</h1>
  <p class="hero-sub">Get desktop notifications and a live terminal feed from Stripe, GitHub, Sentry, and anything else that sends webhooks. Share your setup with the whole team in one command.</p>
  <div class="hero-actions">
    <div class="hero-install" onclick="copyText('curl -sSL dread.sh/install | sh', this)"><span class="prompt">$</span> curl -sSL dread.sh/install <span class="pipe">|</span> sh<button class="copy-btn" type="button"><i data-lucide="copy"></i></button></div>
  </div>
  <div class="hero-right">
    <div class="terminal">
      <div class="terminal-bar">
        <div class="terminal-dots"><span class="dot-red"></span><span class="dot-yellow"></span><span class="dot-green"></span></div>
        <span id="terminal-title">dread.sh - 0 events</span>
      </div>
      <div class="terminal-body" id="terminal-body"></div>
      <div class="terminal-footer">q quit &nbsp; ↑↓ navigate &nbsp; enter detail</div>
    </div>
  </div>
</div>

<!-- INTEGRATIONS MARQUEE -->
<div class="marquee-section">
<div class="marquee-section-inner">
  <div class="marquee-label">Integrations</div>
  <h2 class="marquee-title">Connects to any webhook</h2>
  <div class="marquee-wrap">
    <div class="marquee__track">
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M13.976 9.15c-2.172-.806-3.356-1.426-3.356-2.409 0-.831.683-1.305 1.901-1.305 2.227 0 4.515.858 6.09 1.631l.89-5.494C18.252.975 15.697 0 12.165 0 9.667 0 7.589.654 6.104 1.872 4.56 3.147 3.757 4.992 3.757 7.218c0 4.039 2.467 5.76 6.476 7.219 2.585.92 3.445 1.574 3.445 2.583 0 .98-.84 1.545-2.354 1.545-1.875 0-4.965-.921-6.99-2.109l-.9 5.555C5.175 22.99 8.385 24 11.714 24c2.641 0 4.843-.624 6.328-1.813 1.664-1.305 2.525-3.236 2.525-5.732 0-4.128-2.524-5.851-6.594-7.305h.003z"/></svg>Stripe</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M12 .297c-6.63 0-12 5.373-12 12 0 5.303 3.438 9.8 8.205 11.385.6.113.82-.258.82-.577 0-.285-.01-1.04-.015-2.04-3.338.724-4.042-1.61-4.042-1.61C4.422 18.07 3.633 17.7 3.633 17.7c-1.087-.744.084-.729.084-.729 1.205.084 1.838 1.236 1.838 1.236 1.07 1.835 2.809 1.305 3.495.998.108-.776.417-1.305.76-1.605-2.665-.3-5.466-1.332-5.466-5.93 0-1.31.465-2.38 1.235-3.22-.135-.303-.54-1.523.105-3.176 0 0 1.005-.322 3.3 1.23.96-.267 1.98-.399 3-.405 1.02.006 2.04.138 3 .405 2.28-1.552 3.285-1.23 3.285-1.23.645 1.653.24 2.873.12 3.176.765.84 1.23 1.91 1.23 3.22 0 4.61-2.805 5.625-5.475 5.92.42.36.81 1.096.81 2.22 0 1.606-.015 2.896-.015 3.286 0 .315.21.69.825.57C20.565 22.092 24 17.592 24 12.297c0-6.627-5.373-12-12-12"/></svg>GitHub</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M13.91 2.505c-.873-1.448-2.972-1.448-3.844 0L6.904 7.92a15.478 15.478 0 0 1 8.53 12.811h-2.221A13.301 13.301 0 0 0 5.784 9.814l-2.926 5.06a7.65 7.65 0 0 1 4.435 5.848H2.194a.365.365 0 0 1-.298-.534l1.413-2.402a5.16 5.16 0 0 0-1.614-.913L.296 19.275a2.182 2.182 0 0 0 .812 2.999 2.24 2.24 0 0 0 1.086.288h6.983a9.322 9.322 0 0 0-3.845-8.318l1.11-1.922a11.47 11.47 0 0 1 4.95 10.24h5.915a17.242 17.242 0 0 0-7.885-15.28l2.244-3.845a.37.37 0 0 1 .504-.13c.255.14 9.75 16.708 9.928 16.9a.365.365 0 0 1-.327.543h-2.287c.029.612.029 1.223 0 1.831h2.297a2.206 2.206 0 0 0 1.922-3.31z"/></svg>Sentry</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M5.042 15.165a2.528 2.528 0 0 1-2.52 2.523A2.528 2.528 0 0 1 0 15.165a2.527 2.527 0 0 1 2.522-2.52h2.52v2.52zM6.313 15.165a2.527 2.527 0 0 1 2.521-2.52 2.527 2.527 0 0 1 2.521 2.52v6.313A2.528 2.528 0 0 1 8.834 24a2.528 2.528 0 0 1-2.521-2.522v-6.313zM8.834 5.042a2.528 2.528 0 0 1-2.521-2.52A2.528 2.528 0 0 1 8.834 0a2.528 2.528 0 0 1 2.521 2.522v2.52H8.834zM8.834 6.313a2.528 2.528 0 0 1 2.521 2.521 2.528 2.528 0 0 1-2.521 2.521H2.522A2.528 2.528 0 0 1 0 8.834a2.528 2.528 0 0 1 2.522-2.521h6.312zM18.956 8.834a2.528 2.528 0 0 1 2.522-2.521A2.528 2.528 0 0 1 24 8.834a2.528 2.528 0 0 1-2.522 2.521h-2.522V8.834zM17.688 8.834a2.528 2.528 0 0 1-2.523 2.521 2.527 2.527 0 0 1-2.52-2.521V2.522A2.527 2.527 0 0 1 15.165 0a2.528 2.528 0 0 1 2.523 2.522v6.312zM15.165 18.956a2.528 2.528 0 0 1 2.523 2.522A2.528 2.528 0 0 1 15.165 24a2.527 2.527 0 0 1-2.52-2.522v-2.522h2.52zM15.165 17.688a2.527 2.527 0 0 1-2.52-2.523 2.526 2.526 0 0 1 2.52-2.52h6.313A2.527 2.527 0 0 1 24 15.165a2.528 2.528 0 0 1-2.522 2.523h-6.313z"/></svg>Slack</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M20.317 4.3698a19.7913 19.7913 0 00-4.8851-1.5152.0741.0741 0 00-.0785.0371c-.211.3753-.4447.8648-.6083 1.2495-1.8447-.2762-3.68-.2762-5.4868 0-.1636-.3933-.4058-.8742-.6177-1.2495a.077.077 0 00-.0785-.037 19.7363 19.7363 0 00-4.8852 1.515.0699.0699 0 00-.0321.0277C.5334 9.0458-.319 13.5799.0992 18.0578a.0824.0824 0 00.0312.0561c2.0528 1.5076 4.0413 2.4228 5.9929 3.0294a.0777.0777 0 00.0842-.0276c.4616-.6304.8731-1.2952 1.226-1.9942a.076.076 0 00-.0416-.1057c-.6528-.2476-1.2743-.5495-1.8722-.8923a.077.077 0 01-.0076-.1277c.1258-.0943.2517-.1923.3718-.2914a.0743.0743 0 01.0776-.0105c3.9278 1.7933 8.18 1.7933 12.0614 0a.0739.0739 0 01.0785.0095c.1202.099.246.1981.3728.2924a.077.077 0 01-.0066.1276 12.2986 12.2986 0 01-1.873.8914.0766.0766 0 00-.0407.1067c.3604.698.7719 1.3628 1.225 1.9932a.076.076 0 00.0842.0286c1.961-.6067 3.9495-1.5219 6.0023-3.0294a.077.077 0 00.0313-.0552c.5004-5.177-.8382-9.6739-3.5485-13.6604a.061.061 0 00-.0312-.0286zM8.02 15.3312c-1.1825 0-2.1569-1.0857-2.1569-2.419 0-1.3332.9555-2.4189 2.157-2.4189 1.2108 0 2.1757 1.0952 2.1568 2.419 0 1.3332-.9555 2.4189-2.1569 2.4189zm7.9748 0c-1.1825 0-2.1569-1.0857-2.1569-2.419 0-1.3332.9554-2.4189 2.1569-2.4189 1.2108 0 2.1757 1.0952 2.1568 2.419 0 1.3332-.946 2.4189-2.1568 2.4189Z"/></svg>Discord</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="m12 1.608 12 20.784H0Z"/></svg>Vercel</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M15.337 23.979l7.216-1.561s-2.604-17.613-2.625-17.73c-.018-.116-.114-.192-.211-.192s-1.929-.136-1.929-.136-1.275-1.274-1.439-1.411c-.045-.037-.075-.057-.121-.074l-.914 21.104h.023zM11.71 11.305s-.81-.424-1.774-.424c-1.447 0-1.504.906-1.504 1.141 0 1.232 3.24 1.715 3.24 4.629 0 2.295-1.44 3.76-3.406 3.76-2.354 0-3.54-1.465-3.54-1.465l.646-2.086s1.245 1.066 2.28 1.066c.675 0 .975-.545.975-.932 0-1.619-2.654-1.694-2.654-4.359-.034-2.237 1.571-4.416 4.827-4.416 1.257 0 1.875.361 1.875.361l-.945 2.715-.02.01z"/></svg>Shopify</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M12 0C5.381-.008.008 5.352 0 11.971V12c0 6.64 5.359 12 12 12 6.64 0 12-5.36 12-12 0-6.641-5.36-12-12-12zm0 20.801c-4.846.015-8.786-3.904-8.801-8.75V12c-.014-4.846 3.904-8.786 8.75-8.801H12c4.847-.014 8.786 3.904 8.801 8.75V12c.015 4.847-3.904 8.786-8.75 8.801H12zm5.44-11.76c0 1.359-1.12 2.479-2.481 2.479-1.366-.007-2.472-1.113-2.479-2.479 0-1.361 1.12-2.481 2.479-2.481 1.361 0 2.481 1.12 2.481 2.481zm0 5.919c0 1.36-1.12 2.48-2.481 2.48-1.367-.008-2.473-1.114-2.479-2.48 0-1.359 1.12-2.479 2.479-2.479 1.361-.001 2.481 1.12 2.481 2.479zm-5.919 0c0 1.36-1.12 2.48-2.479 2.48-1.368-.007-2.475-1.113-2.481-2.48 0-1.359 1.12-2.479 2.481-2.479 1.358-.001 2.479 1.12 2.479 2.479zm0-5.919c0 1.359-1.12 2.479-2.479 2.479-1.367-.007-2.475-1.112-2.481-2.479 0-1.361 1.12-2.481 2.481-2.481 1.358 0 2.479 1.12 2.479 2.481z"/></svg>Twilio</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M2.886 4.18A11.982 11.982 0 0 1 11.99 0C18.624 0 24 5.376 24 12.009c0 3.64-1.62 6.903-4.18 9.105L2.887 4.18ZM1.817 5.626l16.556 16.556c-.524.33-1.075.62-1.65.866L.951 7.277c.247-.575.537-1.126.866-1.65ZM.322 9.163l14.515 14.515c-.71.172-1.443.282-2.195.322L0 11.358a12 12 0 0 1 .322-2.195Zm-.17 4.862 9.823 9.824a12.02 12.02 0 0 1-9.824-9.824Z"/></svg>Linear</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M16.965 1.18C15.085.164 13.769 0 10.683 0H3.73v14.55h6.926c2.743 0 4.8-.164 6.61-1.37 1.975-1.303 3.004-3.484 3.004-6.007 0-2.716-1.262-4.896-3.305-5.994zm-5.5 10.326h-4.21V3.113l3.977-.027c3.62-.028 5.43 1.234 5.43 4.128 0 3.113-2.248 4.292-5.197 4.292zM3.73 17.61h3.525V24H3.73Z"/></svg>PagerDuty</span>
    </div>
    <div class="marquee__track" aria-hidden="true">
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M13.976 9.15c-2.172-.806-3.356-1.426-3.356-2.409 0-.831.683-1.305 1.901-1.305 2.227 0 4.515.858 6.09 1.631l.89-5.494C18.252.975 15.697 0 12.165 0 9.667 0 7.589.654 6.104 1.872 4.56 3.147 3.757 4.992 3.757 7.218c0 4.039 2.467 5.76 6.476 7.219 2.585.92 3.445 1.574 3.445 2.583 0 .98-.84 1.545-2.354 1.545-1.875 0-4.965-.921-6.99-2.109l-.9 5.555C5.175 22.99 8.385 24 11.714 24c2.641 0 4.843-.624 6.328-1.813 1.664-1.305 2.525-3.236 2.525-5.732 0-4.128-2.524-5.851-6.594-7.305h.003z"/></svg>Stripe</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M12 .297c-6.63 0-12 5.373-12 12 0 5.303 3.438 9.8 8.205 11.385.6.113.82-.258.82-.577 0-.285-.01-1.04-.015-2.04-3.338.724-4.042-1.61-4.042-1.61C4.422 18.07 3.633 17.7 3.633 17.7c-1.087-.744.084-.729.084-.729 1.205.084 1.838 1.236 1.838 1.236 1.07 1.835 2.809 1.305 3.495.998.108-.776.417-1.305.76-1.605-2.665-.3-5.466-1.332-5.466-5.93 0-1.31.465-2.38 1.235-3.22-.135-.303-.54-1.523.105-3.176 0 0 1.005-.322 3.3 1.23.96-.267 1.98-.399 3-.405 1.02.006 2.04.138 3 .405 2.28-1.552 3.285-1.23 3.285-1.23.645 1.653.24 2.873.12 3.176.765.84 1.23 1.91 1.23 3.22 0 4.61-2.805 5.625-5.475 5.92.42.36.81 1.096.81 2.22 0 1.606-.015 2.896-.015 3.286 0 .315.21.69.825.57C20.565 22.092 24 17.592 24 12.297c0-6.627-5.373-12-12-12"/></svg>GitHub</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M13.91 2.505c-.873-1.448-2.972-1.448-3.844 0L6.904 7.92a15.478 15.478 0 0 1 8.53 12.811h-2.221A13.301 13.301 0 0 0 5.784 9.814l-2.926 5.06a7.65 7.65 0 0 1 4.435 5.848H2.194a.365.365 0 0 1-.298-.534l1.413-2.402a5.16 5.16 0 0 0-1.614-.913L.296 19.275a2.182 2.182 0 0 0 .812 2.999 2.24 2.24 0 0 0 1.086.288h6.983a9.322 9.322 0 0 0-3.845-8.318l1.11-1.922a11.47 11.47 0 0 1 4.95 10.24h5.915a17.242 17.242 0 0 0-7.885-15.28l2.244-3.845a.37.37 0 0 1 .504-.13c.255.14 9.75 16.708 9.928 16.9a.365.365 0 0 1-.327.543h-2.287c.029.612.029 1.223 0 1.831h2.297a2.206 2.206 0 0 0 1.922-3.31z"/></svg>Sentry</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M5.042 15.165a2.528 2.528 0 0 1-2.52 2.523A2.528 2.528 0 0 1 0 15.165a2.527 2.527 0 0 1 2.522-2.52h2.52v2.52zM6.313 15.165a2.527 2.527 0 0 1 2.521-2.52 2.527 2.527 0 0 1 2.521 2.52v6.313A2.528 2.528 0 0 1 8.834 24a2.528 2.528 0 0 1-2.521-2.522v-6.313zM8.834 5.042a2.528 2.528 0 0 1-2.521-2.52A2.528 2.528 0 0 1 8.834 0a2.528 2.528 0 0 1 2.521 2.522v2.52H8.834zM8.834 6.313a2.528 2.528 0 0 1 2.521 2.521 2.528 2.528 0 0 1-2.521 2.521H2.522A2.528 2.528 0 0 1 0 8.834a2.528 2.528 0 0 1 2.522-2.521h6.312zM18.956 8.834a2.528 2.528 0 0 1 2.522-2.521A2.528 2.528 0 0 1 24 8.834a2.528 2.528 0 0 1-2.522 2.521h-2.522V8.834zM17.688 8.834a2.528 2.528 0 0 1-2.523 2.521 2.527 2.527 0 0 1-2.52-2.521V2.522A2.527 2.527 0 0 1 15.165 0a2.528 2.528 0 0 1 2.523 2.522v6.312zM15.165 18.956a2.528 2.528 0 0 1 2.523 2.522A2.528 2.528 0 0 1 15.165 24a2.527 2.527 0 0 1-2.52-2.522v-2.522h2.52zM15.165 17.688a2.527 2.527 0 0 1-2.52-2.523 2.526 2.526 0 0 1 2.52-2.52h6.313A2.527 2.527 0 0 1 24 15.165a2.528 2.528 0 0 1-2.522 2.523h-6.313z"/></svg>Slack</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M20.317 4.3698a19.7913 19.7913 0 00-4.8851-1.5152.0741.0741 0 00-.0785.0371c-.211.3753-.4447.8648-.6083 1.2495-1.8447-.2762-3.68-.2762-5.4868 0-.1636-.3933-.4058-.8742-.6177-1.2495a.077.077 0 00-.0785-.037 19.7363 19.7363 0 00-4.8852 1.515.0699.0699 0 00-.0321.0277C.5334 9.0458-.319 13.5799.0992 18.0578a.0824.0824 0 00.0312.0561c2.0528 1.5076 4.0413 2.4228 5.9929 3.0294a.0777.0777 0 00.0842-.0276c.4616-.6304.8731-1.2952 1.226-1.9942a.076.076 0 00-.0416-.1057c-.6528-.2476-1.2743-.5495-1.8722-.8923a.077.077 0 01-.0076-.1277c.1258-.0943.2517-.1923.3718-.2914a.0743.0743 0 01.0776-.0105c3.9278 1.7933 8.18 1.7933 12.0614 0a.0739.0739 0 01.0785.0095c.1202.099.246.1981.3728.2924a.077.077 0 01-.0066.1276 12.2986 12.2986 0 01-1.873.8914.0766.0766 0 00-.0407.1067c.3604.698.7719 1.3628 1.225 1.9932a.076.076 0 00.0842.0286c1.961-.6067 3.9495-1.5219 6.0023-3.0294a.077.077 0 00.0313-.0552c.5004-5.177-.8382-9.6739-3.5485-13.6604a.061.061 0 00-.0312-.0286zM8.02 15.3312c-1.1825 0-2.1569-1.0857-2.1569-2.419 0-1.3332.9555-2.4189 2.157-2.4189 1.2108 0 2.1757 1.0952 2.1568 2.419 0 1.3332-.9555 2.4189-2.1569 2.4189zm7.9748 0c-1.1825 0-2.1569-1.0857-2.1569-2.419 0-1.3332.9554-2.4189 2.1569-2.4189 1.2108 0 2.1757 1.0952 2.1568 2.419 0 1.3332-.946 2.4189-2.1568 2.4189Z"/></svg>Discord</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="m12 1.608 12 20.784H0Z"/></svg>Vercel</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M15.337 23.979l7.216-1.561s-2.604-17.613-2.625-17.73c-.018-.116-.114-.192-.211-.192s-1.929-.136-1.929-.136-1.275-1.274-1.439-1.411c-.045-.037-.075-.057-.121-.074l-.914 21.104h.023zM11.71 11.305s-.81-.424-1.774-.424c-1.447 0-1.504.906-1.504 1.141 0 1.232 3.24 1.715 3.24 4.629 0 2.295-1.44 3.76-3.406 3.76-2.354 0-3.54-1.465-3.54-1.465l.646-2.086s1.245 1.066 2.28 1.066c.675 0 .975-.545.975-.932 0-1.619-2.654-1.694-2.654-4.359-.034-2.237 1.571-4.416 4.827-4.416 1.257 0 1.875.361 1.875.361l-.945 2.715-.02.01z"/></svg>Shopify</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M12 0C5.381-.008.008 5.352 0 11.971V12c0 6.64 5.359 12 12 12 6.64 0 12-5.36 12-12 0-6.641-5.36-12-12-12zm0 20.801c-4.846.015-8.786-3.904-8.801-8.75V12c-.014-4.846 3.904-8.786 8.75-8.801H12c4.847-.014 8.786 3.904 8.801 8.75V12c.015 4.847-3.904 8.786-8.75 8.801H12zm5.44-11.76c0 1.359-1.12 2.479-2.481 2.479-1.366-.007-2.472-1.113-2.479-2.479 0-1.361 1.12-2.481 2.479-2.481 1.361 0 2.481 1.12 2.481 2.481zm0 5.919c0 1.36-1.12 2.48-2.481 2.48-1.367-.008-2.473-1.114-2.479-2.48 0-1.359 1.12-2.479 2.479-2.479 1.361-.001 2.481 1.12 2.481 2.479zm-5.919 0c0 1.36-1.12 2.48-2.479 2.48-1.368-.007-2.475-1.113-2.481-2.48 0-1.359 1.12-2.479 2.481-2.479 1.358-.001 2.479 1.12 2.479 2.479zm0-5.919c0 1.359-1.12 2.479-2.479 2.479-1.367-.007-2.475-1.112-2.481-2.479 0-1.361 1.12-2.481 2.481-2.481 1.358 0 2.479 1.12 2.479 2.481z"/></svg>Twilio</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M2.886 4.18A11.982 11.982 0 0 1 11.99 0C18.624 0 24 5.376 24 12.009c0 3.64-1.62 6.903-4.18 9.105L2.887 4.18ZM1.817 5.626l16.556 16.556c-.524.33-1.075.62-1.65.866L.951 7.277c.247-.575.537-1.126.866-1.65ZM.322 9.163l14.515 14.515c-.71.172-1.443.282-2.195.322L0 11.358a12 12 0 0 1 .322-2.195Zm-.17 4.862 9.823 9.824a12.02 12.02 0 0 1-9.824-9.824Z"/></svg>Linear</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M16.965 1.18C15.085.164 13.769 0 10.683 0H3.73v14.55h6.926c2.743 0 4.8-.164 6.61-1.37 1.975-1.303 3.004-3.484 3.004-6.007 0-2.716-1.262-4.896-3.305-5.994zm-5.5 10.326h-4.21V3.113l3.977-.027c3.62-.028 5.43 1.234 5.43 4.128 0 3.113-2.248 4.292-5.197 4.292zM3.73 17.61h3.525V24H3.73Z"/></svg>PagerDuty</span>
    </div>
  </div>
  <div class="marquee-wrap marquee-wrap--reverse">
    <div class="marquee__track">
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="m23.6004 9.5927-.0337-.0862L20.3.9814a.851.851 0 0 0-.3362-.405.8748.8748 0 0 0-.9997.0539.8748.8748 0 0 0-.29.4399l-2.2055 6.748H7.5375l-2.2057-6.748a.8573.8573 0 0 0-.29-.4412.8748.8748 0 0 0-.9997-.0537.8585.8585 0 0 0-.3362.4049L.4332 9.5015l-.0325.0862a6.0657 6.0657 0 0 0 2.0119 7.0105l.0113.0087.03.0213 4.976 3.7264 2.462 1.8633 1.4995 1.1321a1.0085 1.0085 0 0 0 1.2197 0l1.4995-1.1321 2.4619-1.8633 5.006-3.7489.0125-.01a6.0682 6.0682 0 0 0 2.0094-7.003z"/></svg>GitLab</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M11.571 11.513H0a5.218 5.218 0 0 0 5.232 5.215h2.13v2.057A5.215 5.215 0 0 0 12.575 24V12.518a1.005 1.005 0 0 0-1.005-1.005zm5.723-5.756H5.736a5.215 5.215 0 0 0 5.215 5.214h2.129v2.058a5.218 5.218 0 0 0 5.215 5.214V6.758a1.001 1.001 0 0 0-1.001-1.001zM23.013 0H11.455a5.215 5.215 0 0 0 5.215 5.215h2.129v2.057A5.215 5.215 0 0 0 24 12.483V1.005A1.001 1.001 0 0 0 23.013 0Z"/></svg>Jira</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M21 0H3C1.343 0 0 1.343 0 3v18c0 1.658 1.343 3 3 3h18c1.658 0 3-1.342 3-3V3c0-1.657-1.342-3-3-3zm-5.801 4.399c0-.44.36-.8.802-.8.44 0 .8.36.8.8v10.688c0 .442-.36.801-.8.801-.443 0-.802-.359-.802-.801V4.399zM11.2 3.994c0-.44.357-.799.8-.799s.8.359.8.799v11.602c0 .44-.357.8-.8.8s-.8-.36-.8-.8V3.994zm-4 .405c0-.44.359-.8.799-.8.443 0 .802.36.802.8v10.688c0 .442-.36.801-.802.801-.44 0-.799-.359-.799-.801V4.399zM3.199 6c0-.442.36-.8.802-.8.44 0 .799.358.799.8v7.195c0 .441-.359.8-.799.8-.443 0-.802-.36-.802-.8V6zM20.52 18.202c-.123.105-3.086 2.593-8.52 2.593-5.433 0-8.397-2.486-8.521-2.593-.335-.288-.375-.792-.086-1.128.285-.334.79-.375 1.125-.09.047.041 2.693 2.211 7.481 2.211 4.848 0 7.456-2.186 7.479-2.207.334-.289.839-.25 1.128.086.289.336.25.84-.086 1.128zm.281-5.007c0 .441-.36.8-.801.8-.441 0-.801-.36-.801-.8V6c0-.442.361-.8.801-.8.441 0 .801.357.801.8v7.195z"/></svg>Intercom</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M18.164 7.93V5.084a2.198 2.198 0 001.267-1.978v-.067A2.2 2.2 0 0017.238.845h-.067a2.2 2.2 0 00-2.193 2.193v.067a2.196 2.196 0 001.252 1.973l.013.006v2.852a6.22 6.22 0 00-2.969 1.31l.012-.01-7.828-6.095A2.497 2.497 0 104.3 4.656l-.012.006 7.697 5.991a6.176 6.176 0 00-1.038 3.446c0 1.343.425 2.588 1.147 3.607l-.013-.02-2.342 2.343a1.968 1.968 0 00-.58-.095h-.002a2.033 2.033 0 102.033 2.033 1.978 1.978 0 00-.1-.595l.005.014 2.317-2.317a6.247 6.247 0 104.782-11.134l-.036-.005zm-.964 9.378a3.206 3.206 0 113.215-3.207v.002a3.206 3.206 0 01-3.207 3.207z"/></svg>HubSpot</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M13.976 9.15c-2.172-.806-3.356-1.426-3.356-2.409 0-.831.683-1.305 1.901-1.305 2.227 0 4.515.858 6.09 1.631l.89-5.494C18.252.975 15.697 0 12.165 0 9.667 0 7.589.654 6.104 1.872 4.56 3.147 3.757 4.992 3.757 7.218c0 4.039 2.467 5.76 6.476 7.219 2.585.92 3.445 1.574 3.445 2.583 0 .98-.84 1.545-2.354 1.545-1.875 0-4.965-.921-6.99-2.109l-.9 5.555C5.175 22.99 8.385 24 11.714 24c2.641 0 4.843-.624 6.328-1.813 1.664-1.305 2.525-3.236 2.525-5.732 0-4.128-2.524-5.851-6.594-7.305h.003z"/></svg>Stripe</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M12 .297c-6.63 0-12 5.373-12 12 0 5.303 3.438 9.8 8.205 11.385.6.113.82-.258.82-.577 0-.285-.01-1.04-.015-2.04-3.338.724-4.042-1.61-4.042-1.61C4.422 18.07 3.633 17.7 3.633 17.7c-1.087-.744.084-.729.084-.729 1.205.084 1.838 1.236 1.838 1.236 1.07 1.835 2.809 1.305 3.495.998.108-.776.417-1.305.76-1.605-2.665-.3-5.466-1.332-5.466-5.93 0-1.31.465-2.38 1.235-3.22-.135-.303-.54-1.523.105-3.176 0 0 1.005-.322 3.3 1.23.96-.267 1.98-.399 3-.405 1.02.006 2.04.138 3 .405 2.28-1.552 3.285-1.23 3.285-1.23.645 1.653.24 2.873.12 3.176.765.84 1.23 1.91 1.23 3.22 0 4.61-2.805 5.625-5.475 5.92.42.36.81 1.096.81 2.22 0 1.606-.015 2.896-.015 3.286 0 .315.21.69.825.57C20.565 22.092 24 17.592 24 12.297c0-6.627-5.373-12-12-12"/></svg>GitHub</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M12 0C5.381-.008.008 5.352 0 11.971V12c0 6.64 5.359 12 12 12 6.64 0 12-5.36 12-12 0-6.641-5.36-12-12-12zm0 20.801c-4.846.015-8.786-3.904-8.801-8.75V12c-.014-4.846 3.904-8.786 8.75-8.801H12c4.847-.014 8.786 3.904 8.801 8.75V12c.015 4.847-3.904 8.786-8.75 8.801H12zm5.44-11.76c0 1.359-1.12 2.479-2.481 2.479-1.366-.007-2.472-1.113-2.479-2.479 0-1.361 1.12-2.481 2.479-2.481 1.361 0 2.481 1.12 2.481 2.481zm0 5.919c0 1.36-1.12 2.48-2.481 2.48-1.367-.008-2.473-1.114-2.479-2.48 0-1.359 1.12-2.479 2.479-2.479 1.361-.001 2.481 1.12 2.481 2.479zm-5.919 0c0 1.36-1.12 2.48-2.479 2.48-1.368-.007-2.475-1.113-2.481-2.48 0-1.359 1.12-2.479 2.481-2.479 1.358-.001 2.479 1.12 2.479 2.479zm0-5.919c0 1.359-1.12 2.479-2.479 2.479-1.367-.007-2.475-1.112-2.481-2.479 0-1.361 1.12-2.481 2.481-2.481 1.358 0 2.479 1.12 2.479 2.481z"/></svg>Twilio</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M2.886 4.18A11.982 11.982 0 0 1 11.99 0C18.624 0 24 5.376 24 12.009c0 3.64-1.62 6.903-4.18 9.105L2.887 4.18ZM1.817 5.626l16.556 16.556c-.524.33-1.075.62-1.65.866L.951 7.277c.247-.575.537-1.126.866-1.65ZM.322 9.163l14.515 14.515c-.71.172-1.443.282-2.195.322L0 11.358a12 12 0 0 1 .322-2.195Zm-.17 4.862 9.823 9.824a12.02 12.02 0 0 1-9.824-9.824Z"/></svg>Linear</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M5.042 15.165a2.528 2.528 0 0 1-2.52 2.523A2.528 2.528 0 0 1 0 15.165a2.527 2.527 0 0 1 2.522-2.52h2.52v2.52zM6.313 15.165a2.527 2.527 0 0 1 2.521-2.52 2.527 2.527 0 0 1 2.521 2.52v6.313A2.528 2.528 0 0 1 8.834 24a2.528 2.528 0 0 1-2.521-2.522v-6.313zM8.834 5.042a2.528 2.528 0 0 1-2.521-2.52A2.528 2.528 0 0 1 8.834 0a2.528 2.528 0 0 1 2.521 2.522v2.52H8.834zM8.834 6.313a2.528 2.528 0 0 1 2.521 2.521 2.528 2.528 0 0 1-2.521 2.521H2.522A2.528 2.528 0 0 1 0 8.834a2.528 2.528 0 0 1 2.522-2.521h6.312zM18.956 8.834a2.528 2.528 0 0 1 2.522-2.521A2.528 2.528 0 0 1 24 8.834a2.528 2.528 0 0 1-2.522 2.521h-2.522V8.834zM17.688 8.834a2.528 2.528 0 0 1-2.523 2.521 2.527 2.527 0 0 1-2.52-2.521V2.522A2.527 2.527 0 0 1 15.165 0a2.528 2.528 0 0 1 2.523 2.522v6.312zM15.165 18.956a2.528 2.528 0 0 1 2.523 2.522A2.528 2.528 0 0 1 15.165 24a2.527 2.527 0 0 1-2.52-2.522v-2.522h2.52zM15.165 17.688a2.527 2.527 0 0 1-2.52-2.523 2.526 2.526 0 0 1 2.52-2.52h6.313A2.527 2.527 0 0 1 24 15.165a2.528 2.528 0 0 1-2.522 2.523h-6.313z"/></svg>Slack</span>
    </div>
    <div class="marquee__track" aria-hidden="true">
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="m23.6004 9.5927-.0337-.0862L20.3.9814a.851.851 0 0 0-.3362-.405.8748.8748 0 0 0-.9997.0539.8748.8748 0 0 0-.29.4399l-2.2055 6.748H7.5375l-2.2057-6.748a.8573.8573 0 0 0-.29-.4412.8748.8748 0 0 0-.9997-.0537.8585.8585 0 0 0-.3362.4049L.4332 9.5015l-.0325.0862a6.0657 6.0657 0 0 0 2.0119 7.0105l.0113.0087.03.0213 4.976 3.7264 2.462 1.8633 1.4995 1.1321a1.0085 1.0085 0 0 0 1.2197 0l1.4995-1.1321 2.4619-1.8633 5.006-3.7489.0125-.01a6.0682 6.0682 0 0 0 2.0094-7.003z"/></svg>GitLab</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M11.571 11.513H0a5.218 5.218 0 0 0 5.232 5.215h2.13v2.057A5.215 5.215 0 0 0 12.575 24V12.518a1.005 1.005 0 0 0-1.005-1.005zm5.723-5.756H5.736a5.215 5.215 0 0 0 5.215 5.214h2.129v2.058a5.218 5.218 0 0 0 5.215 5.214V6.758a1.001 1.001 0 0 0-1.001-1.001zM23.013 0H11.455a5.215 5.215 0 0 0 5.215 5.215h2.129v2.057A5.215 5.215 0 0 0 24 12.483V1.005A1.001 1.001 0 0 0 23.013 0Z"/></svg>Jira</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M21 0H3C1.343 0 0 1.343 0 3v18c0 1.658 1.343 3 3 3h18c1.658 0 3-1.342 3-3V3c0-1.657-1.342-3-3-3zm-5.801 4.399c0-.44.36-.8.802-.8.44 0 .8.36.8.8v10.688c0 .442-.36.801-.8.801-.443 0-.802-.359-.802-.801V4.399zM11.2 3.994c0-.44.357-.799.8-.799s.8.359.8.799v11.602c0 .44-.357.8-.8.8s-.8-.36-.8-.8V3.994zm-4 .405c0-.44.359-.8.799-.8.443 0 .802.36.802.8v10.688c0 .442-.36.801-.802.801-.44 0-.799-.359-.799-.801V4.399zM3.199 6c0-.442.36-.8.802-.8.44 0 .799.358.799.8v7.195c0 .441-.359.8-.799.8-.443 0-.802-.36-.802-.8V6zM20.52 18.202c-.123.105-3.086 2.593-8.52 2.593-5.433 0-8.397-2.486-8.521-2.593-.335-.288-.375-.792-.086-1.128.285-.334.79-.375 1.125-.09.047.041 2.693 2.211 7.481 2.211 4.848 0 7.456-2.186 7.479-2.207.334-.289.839-.25 1.128.086.289.336.25.84-.086 1.128zm.281-5.007c0 .441-.36.8-.801.8-.441 0-.801-.36-.801-.8V6c0-.442.361-.8.801-.8.441 0 .801.357.801.8v7.195z"/></svg>Intercom</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M18.164 7.93V5.084a2.198 2.198 0 001.267-1.978v-.067A2.2 2.2 0 0017.238.845h-.067a2.2 2.2 0 00-2.193 2.193v.067a2.196 2.196 0 001.252 1.973l.013.006v2.852a6.22 6.22 0 00-2.969 1.31l.012-.01-7.828-6.095A2.497 2.497 0 104.3 4.656l-.012.006 7.697 5.991a6.176 6.176 0 00-1.038 3.446c0 1.343.425 2.588 1.147 3.607l-.013-.02-2.342 2.343a1.968 1.968 0 00-.58-.095h-.002a2.033 2.033 0 102.033 2.033 1.978 1.978 0 00-.1-.595l.005.014 2.317-2.317a6.247 6.247 0 104.782-11.134l-.036-.005zm-.964 9.378a3.206 3.206 0 113.215-3.207v.002a3.206 3.206 0 01-3.207 3.207z"/></svg>HubSpot</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M13.976 9.15c-2.172-.806-3.356-1.426-3.356-2.409 0-.831.683-1.305 1.901-1.305 2.227 0 4.515.858 6.09 1.631l.89-5.494C18.252.975 15.697 0 12.165 0 9.667 0 7.589.654 6.104 1.872 4.56 3.147 3.757 4.992 3.757 7.218c0 4.039 2.467 5.76 6.476 7.219 2.585.92 3.445 1.574 3.445 2.583 0 .98-.84 1.545-2.354 1.545-1.875 0-4.965-.921-6.99-2.109l-.9 5.555C5.175 22.99 8.385 24 11.714 24c2.641 0 4.843-.624 6.328-1.813 1.664-1.305 2.525-3.236 2.525-5.732 0-4.128-2.524-5.851-6.594-7.305h.003z"/></svg>Stripe</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M12 .297c-6.63 0-12 5.373-12 12 0 5.303 3.438 9.8 8.205 11.385.6.113.82-.258.82-.577 0-.285-.01-1.04-.015-2.04-3.338.724-4.042-1.61-4.042-1.61C4.422 18.07 3.633 17.7 3.633 17.7c-1.087-.744.084-.729.084-.729 1.205.084 1.838 1.236 1.838 1.236 1.07 1.835 2.809 1.305 3.495.998.108-.776.417-1.305.76-1.605-2.665-.3-5.466-1.332-5.466-5.93 0-1.31.465-2.38 1.235-3.22-.135-.303-.54-1.523.105-3.176 0 0 1.005-.322 3.3 1.23.96-.267 1.98-.399 3-.405 1.02.006 2.04.138 3 .405 2.28-1.552 3.285-1.23 3.285-1.23.645 1.653.24 2.873.12 3.176.765.84 1.23 1.91 1.23 3.22 0 4.61-2.805 5.625-5.475 5.92.42.36.81 1.096.81 2.22 0 1.606-.015 2.896-.015 3.286 0 .315.21.69.825.57C20.565 22.092 24 17.592 24 12.297c0-6.627-5.373-12-12-12"/></svg>GitHub</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M12 0C5.381-.008.008 5.352 0 11.971V12c0 6.64 5.359 12 12 12 6.64 0 12-5.36 12-12 0-6.641-5.36-12-12-12zm0 20.801c-4.846.015-8.786-3.904-8.801-8.75V12c-.014-4.846 3.904-8.786 8.75-8.801H12c4.847-.014 8.786 3.904 8.801 8.75V12c.015 4.847-3.904 8.786-8.75 8.801H12zm5.44-11.76c0 1.359-1.12 2.479-2.481 2.479-1.366-.007-2.472-1.113-2.479-2.479 0-1.361 1.12-2.481 2.479-2.481 1.361 0 2.481 1.12 2.481 2.481zm0 5.919c0 1.36-1.12 2.48-2.481 2.48-1.367-.008-2.473-1.114-2.479-2.48 0-1.359 1.12-2.479 2.479-2.479 1.361-.001 2.481 1.12 2.481 2.479zm-5.919 0c0 1.36-1.12 2.48-2.479 2.48-1.368-.007-2.475-1.113-2.481-2.48 0-1.359 1.12-2.479 2.481-2.479 1.358-.001 2.479 1.12 2.479 2.479zm0-5.919c0 1.359-1.12 2.479-2.479 2.479-1.367-.007-2.475-1.112-2.481-2.479 0-1.361 1.12-2.481 2.481-2.481 1.358 0 2.479 1.12 2.479 2.481z"/></svg>Twilio</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M2.886 4.18A11.982 11.982 0 0 1 11.99 0C18.624 0 24 5.376 24 12.009c0 3.64-1.62 6.903-4.18 9.105L2.887 4.18ZM1.817 5.626l16.556 16.556c-.524.33-1.075.62-1.65.866L.951 7.277c.247-.575.537-1.126.866-1.65ZM.322 9.163l14.515 14.515c-.71.172-1.443.282-2.195.322L0 11.358a12 12 0 0 1 .322-2.195Zm-.17 4.862 9.823 9.824a12.02 12.02 0 0 1-9.824-9.824Z"/></svg>Linear</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M5.042 15.165a2.528 2.528 0 0 1-2.52 2.523A2.528 2.528 0 0 1 0 15.165a2.527 2.527 0 0 1 2.522-2.52h2.52v2.52zM6.313 15.165a2.527 2.527 0 0 1 2.521-2.52 2.527 2.527 0 0 1 2.521 2.52v6.313A2.528 2.528 0 0 1 8.834 24a2.528 2.528 0 0 1-2.521-2.522v-6.313zM8.834 5.042a2.528 2.528 0 0 1-2.521-2.52A2.528 2.528 0 0 1 8.834 0a2.528 2.528 0 0 1 2.521 2.522v2.52H8.834zM8.834 6.313a2.528 2.528 0 0 1 2.521 2.521 2.528 2.528 0 0 1-2.521 2.521H2.522A2.528 2.528 0 0 1 0 8.834a2.528 2.528 0 0 1 2.522-2.521h6.312zM18.956 8.834a2.528 2.528 0 0 1 2.522-2.521A2.528 2.528 0 0 1 24 8.834a2.528 2.528 0 0 1-2.522 2.521h-2.522V8.834zM17.688 8.834a2.528 2.528 0 0 1-2.523 2.521 2.527 2.527 0 0 1-2.52-2.521V2.522A2.527 2.527 0 0 1 15.165 0a2.528 2.528 0 0 1 2.523 2.522v6.312zM15.165 18.956a2.528 2.528 0 0 1 2.523 2.522A2.528 2.528 0 0 1 15.165 24a2.527 2.527 0 0 1-2.52-2.522v-2.522h2.52zM15.165 17.688a2.527 2.527 0 0 1-2.52-2.523 2.526 2.526 0 0 1 2.52-2.52h6.313A2.527 2.527 0 0 1 24 15.165a2.528 2.528 0 0 1-2.522 2.523h-6.313z"/></svg>Slack</span>
    </div>
  </div>
  <div class="marquee-wrap">
    <div class="marquee__track">
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M12.007 1.2C5.997 1.2 1.131 6.066 1.131 12.076c0 6.009 4.866 10.876 10.876 10.876 6.01 0 10.876-4.867 10.876-10.876C22.883 6.066 18.017 1.2 12.007 1.2zm0 2.473a2.246 2.246 0 1 1 0 4.493 2.246 2.246 0 0 1 0-4.493zm3.478 13.54H8.529a.823.823 0 0 1-.823-.822V15.08a.823.823 0 0 1 .823-.823h.822v-3.29h-.822a.823.823 0 0 1-.823-.822v-1.315a.823.823 0 0 1 .823-.822h4.11a.823.823 0 0 1 .824.822v5.427h.822a.823.823 0 0 1 .822.823v1.311a.823.823 0 0 1-.822.822z"/></svg>Salesforce</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M7.024 6.965c0-1.399 1.136-2.534 2.535-2.534 1.398 0 2.534 1.135 2.534 2.534 0 1.4-1.136 2.535-2.534 2.535-1.4 0-2.535-1.135-2.535-2.535Zm10.457 4.487c-1.4 0-2.535 1.136-2.535 2.535s1.136 2.535 2.535 2.535 2.534-1.136 2.534-2.535-1.135-2.535-2.534-2.535ZM4.486 11.452c-1.4 0-2.534 1.136-2.534 2.535s1.135 2.535 2.534 2.535c1.4 0 2.535-1.136 2.535-2.535s-1.136-2.535-2.535-2.535Z"/></svg>Datadog</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M11.986 0C5.366 0 0 5.39 0 12.038c0 3.273 1.29 6.236 3.39 8.414L1.97 24l3.675-1.373a11.876 11.876 0 0 0 6.341 1.83c6.62 0 11.986-5.39 11.986-12.038C23.972 5.79 18.606 0 11.986 0Zm6.263 7.38-3.478 5.124a1.397 1.397 0 0 1-2.003.38L10.5 11.23a.56.56 0 0 0-.66.006l-3.18 2.41a.442.442 0 0 1-.64-.583l3.534-5.05a1.397 1.397 0 0 1 1.988-.376l2.289 1.668a.56.56 0 0 0 .66-.006l3.118-2.363a.442.442 0 0 1 .64.443z"/></svg>Mailchimp</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M15.563 4.523a6.847 6.847 0 0 0-1.156.093l-.093.013c-1.238.2-2.395.683-3.4 1.424a8.893 8.893 0 0 0-2.744 3.22 10.482 10.482 0 0 0-.903 2.937c-.186 1.076-.232 2.14-.377 3.213-.093.689-.239 1.387-.547 2.028a3.098 3.098 0 0 1-.754 1.047c-.306.268-.673.45-1.073.523a3.6 3.6 0 0 1-.818.046c-.17-.007-.334-.032-.498-.052a.128.128 0 0 0-.127.065c-.028.046-.008.089.028.127.046.046.099.084.153.12.5.32 1.04.567 1.613.734a6.39 6.39 0 0 0 3.546.093 5.289 5.289 0 0 0 2.576-1.529 6.3 6.3 0 0 0 1.424-2.779 12.74 12.74 0 0 0 .4-2.484c.06-.589.093-1.18.186-1.768.08-.508.199-1.008.395-1.489.18-.444.417-.862.714-1.238.14-.177.293-.347.46-.498.347-.312.754-.538 1.198-.683.1-.033.127-.08.093-.173a3.74 3.74 0 0 0-.734-1.304 4.293 4.293 0 0 0-1.563-1.203V4.523zM12.01 0C5.38 0 0 5.382 0 12.01 0 18.636 5.382 24 12.01 24c6.627 0 12.01-5.382 12.01-12.01C24.02 5.364 18.636 0 12.01 0z"/></svg>Zendesk</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M17.3877 10.5603C17.3877 10.1938 17.6766 9.9048 18.0432 9.9048C20.3107 9.9048 22.1457 11.7399 22.1457 14.0074C22.1457 16.2749 20.3107 18.1099 18.0432 18.1099H15.3397V15.8424H18.0432C19.0553 15.8424 19.8782 15.0195 19.8782 14.0074C19.8782 12.9952 19.0553 12.1724 18.0432 12.1724C17.6766 12.1724 17.3877 11.8834 17.3877 11.5169V10.5603ZM6.6123 13.4397C6.6123 13.8063 6.3234 14.0952 5.9568 14.0952C3.6893 14.0952 1.8543 12.2602 1.8543 9.9927C1.8543 7.7252 3.6893 5.8901 5.9568 5.8901H8.6603V8.1576H5.9568C4.9447 8.1576 4.1218 8.9805 4.1218 9.9927C4.1218 11.0048 4.9447 11.8277 5.9568 11.8277C6.3234 11.8277 6.6123 12.1166 6.6123 12.4831V13.4397ZM8.6602 18.1099C7.404 18.1099 6.3853 17.0912 6.3853 15.835V14.0952C6.3853 12.839 7.404 11.8203 8.6602 11.8203V14.0952H10.9277V15.835C10.9277 17.0912 9.9089 18.1099 8.6527 18.1099H8.6602ZM15.3397 5.8901C16.596 5.8901 17.6147 6.9088 17.6147 8.165V9.9048C17.6147 11.161 16.596 12.1797 15.3397 12.1797V9.9048H13.0723V8.165C13.0723 6.9088 14.0911 5.8901 15.3473 5.8901H15.3397Z"/></svg>Netlify</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M21.362 9.354H12V14.22h5.39c-.49 2.543-2.761 4.417-5.39 4.417-3.004 0-5.44-2.436-5.44-5.44 0-3.005 2.436-5.442 5.44-5.442 1.332 0 2.552.48 3.496 1.276l3.592-3.592C17.226 3.675 14.762 2.72 12 2.72 6.863 2.72 2.72 6.863 2.72 12c0 5.136 4.143 9.28 9.28 9.28 5.136 0 9.28-4.144 9.28-9.28 0-.553-.048-1.1-.138-1.634l.22-.012z"/></svg>Supabase</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M19.655 14.262c0 1.41-.672 2.143-2.013 2.143h-2.398v-4.283h2.398c1.341 0 2.013.732 2.013 2.14zm-2.12-6.478c0-1.29-.615-1.934-1.846-1.934H13.39v3.87h2.299c1.231 0 1.846-.646 1.846-1.935zM24 12c0 6.627-5.373 12-12 12S0 18.627 0 12 5.373 0 12 0s12 5.373 12 12zm-5.478 2.262c0-1.232-.464-2.197-1.382-2.878.75-.679 1.133-1.574 1.133-2.691 0-2.357-1.47-3.509-4.437-3.509H10.34v12.632h3.617c2.98 0 4.564-1.18 4.564-3.554z"/></svg>Calendly</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M12.502 0c-6.6 0-9.958 4.388-9.958 9.497 0 4.401 2.644 7.95 6.574 7.95 1.736 0 3.096-.95 3.58-2.2.168-.397.265-.84.265-1.314 0-.577-.136-1.197-.462-1.906-.577-1.246-.785-1.892-.785-2.638 0-2.13 1.614-4.143 4.614-4.143 2.507 0 4.36 1.752 4.36 4.323 0 2.815-1.504 5.953-4.037 5.953-.975 0-1.66-.672-1.66-1.653 0-.96.643-1.946 1.332-2.97.676-1.012 1.397-2.07 1.397-3.17 0-1.08-.786-1.942-2.098-1.942-1.618 0-2.89 1.433-2.89 3.38 0 .843.201 1.568.498 2.191l-2.05 8.69c-.42 1.736-.157 4.224-.06 4.915.036.276.353.337.494.12.21-.315 2.738-3.893 3.328-6.048.204-.744 1.04-3.786 1.04-3.786.551 1.02 2.098 1.872 3.764 1.872C21.117 17.121 24 13.342 24 8.716 24 4.297 20.564 0 12.502 0z"/></svg>Typeform</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M15.852 8.981h-4.588V0h4.588c2.476 0 4.49 2.014 4.49 4.49s-2.014 4.491-4.49 4.491zM12.735 7.51h3.117a3.007 3.007 0 0 0 3.019-3.019 3.007 3.007 0 0 0-3.019-3.019h-3.117v6.038zm-1.47 1.471H8.148c-2.477 0-4.49-2.014-4.49-4.49S5.67 0 8.148 0h3.117v8.981zm-1.471-7.51H8.148A3.007 3.007 0 0 0 5.13 4.491a3.007 3.007 0 0 0 3.019 3.019h1.647V1.471zm1.471 17.058h3.117c2.476 0 4.49-2.015 4.49-4.491s-2.014-4.49-4.49-4.49h-4.588v8.981h1.471zm0-7.51h3.117a3.007 3.007 0 0 1 3.019 3.019 3.007 3.007 0 0 1-3.019 3.02h-3.117v-6.039zM8.148 24c-2.477 0-4.49-2.015-4.49-4.491v-4.49h4.49a4.491 4.491 0 0 1 0 8.981zM5.13 16.49v2.52A3.007 3.007 0 0 0 8.148 22.028a3.007 3.007 0 0 0 3.019-3.019 3.007 3.007 0 0 0-3.019-3.019H5.13zm6.334-1.471H8.148a4.491 4.491 0 0 1 0-8.981h4.588v8.981h-1.272zm-1.2-7.51H8.148A3.007 3.007 0 0 0 5.13 10.528a3.007 3.007 0 0 0 3.019 3.019h2.116V7.51z"/></svg>Figma</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M14.975 0C11.326 0 8.958 1.936 8.207 5.799c1.352-1.936 2.929-2.663 4.74-2.187.969.252 1.665.977 2.434 1.784C16.667 6.73 18.167 8.28 21.333 8.28c3.649 0 6.017-1.936 6.768-5.799-1.352 1.936-2.929 2.663-4.74 2.187-.969-.252-1.665-.977-2.434-1.784C19.641 1.55 18.141 0 14.975 0zM8.207 8.28C4.558 8.28 2.19 10.216 1.44 14.079c1.352-1.936 2.929-2.663 4.74-2.187.969.252 1.665.977 2.434 1.784C9.9 15.01 11.4 16.56 14.566 16.56c3.649 0 6.017-1.936 6.768-5.799-1.352 1.936-2.929 2.663-4.74 2.187-.969-.252-1.665-.977-2.434-1.784C12.874 9.83 11.374 8.28 8.207 8.28z" transform="scale(.8) translate(1.5 4)"/></svg>SendGrid</span>
    </div>
    <div class="marquee__track" aria-hidden="true">
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M12.007 1.2C5.997 1.2 1.131 6.066 1.131 12.076c0 6.009 4.866 10.876 10.876 10.876 6.01 0 10.876-4.867 10.876-10.876C22.883 6.066 18.017 1.2 12.007 1.2zm0 2.473a2.246 2.246 0 1 1 0 4.493 2.246 2.246 0 0 1 0-4.493zm3.478 13.54H8.529a.823.823 0 0 1-.823-.822V15.08a.823.823 0 0 1 .823-.823h.822v-3.29h-.822a.823.823 0 0 1-.823-.822v-1.315a.823.823 0 0 1 .823-.822h4.11a.823.823 0 0 1 .824.822v5.427h.822a.823.823 0 0 1 .822.823v1.311a.823.823 0 0 1-.822.822z"/></svg>Salesforce</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M7.024 6.965c0-1.399 1.136-2.534 2.535-2.534 1.398 0 2.534 1.135 2.534 2.534 0 1.4-1.136 2.535-2.534 2.535-1.4 0-2.535-1.135-2.535-2.535Zm10.457 4.487c-1.4 0-2.535 1.136-2.535 2.535s1.136 2.535 2.535 2.535 2.534-1.136 2.534-2.535-1.135-2.535-2.534-2.535ZM4.486 11.452c-1.4 0-2.534 1.136-2.534 2.535s1.135 2.535 2.534 2.535c1.4 0 2.535-1.136 2.535-2.535s-1.136-2.535-2.535-2.535Z"/></svg>Datadog</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M11.986 0C5.366 0 0 5.39 0 12.038c0 3.273 1.29 6.236 3.39 8.414L1.97 24l3.675-1.373a11.876 11.876 0 0 0 6.341 1.83c6.62 0 11.986-5.39 11.986-12.038C23.972 5.79 18.606 0 11.986 0Zm6.263 7.38-3.478 5.124a1.397 1.397 0 0 1-2.003.38L10.5 11.23a.56.56 0 0 0-.66.006l-3.18 2.41a.442.442 0 0 1-.64-.583l3.534-5.05a1.397 1.397 0 0 1 1.988-.376l2.289 1.668a.56.56 0 0 0 .66-.006l3.118-2.363a.442.442 0 0 1 .64.443z"/></svg>Mailchimp</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M15.563 4.523a6.847 6.847 0 0 0-1.156.093l-.093.013c-1.238.2-2.395.683-3.4 1.424a8.893 8.893 0 0 0-2.744 3.22 10.482 10.482 0 0 0-.903 2.937c-.186 1.076-.232 2.14-.377 3.213-.093.689-.239 1.387-.547 2.028a3.098 3.098 0 0 1-.754 1.047c-.306.268-.673.45-1.073.523a3.6 3.6 0 0 1-.818.046c-.17-.007-.334-.032-.498-.052a.128.128 0 0 0-.127.065c-.028.046-.008.089.028.127.046.046.099.084.153.12.5.32 1.04.567 1.613.734a6.39 6.39 0 0 0 3.546.093 5.289 5.289 0 0 0 2.576-1.529 6.3 6.3 0 0 0 1.424-2.779 12.74 12.74 0 0 0 .4-2.484c.06-.589.093-1.18.186-1.768.08-.508.199-1.008.395-1.489.18-.444.417-.862.714-1.238.14-.177.293-.347.46-.498.347-.312.754-.538 1.198-.683.1-.033.127-.08.093-.173a3.74 3.74 0 0 0-.734-1.304 4.293 4.293 0 0 0-1.563-1.203V4.523zM12.01 0C5.38 0 0 5.382 0 12.01 0 18.636 5.382 24 12.01 24c6.627 0 12.01-5.382 12.01-12.01C24.02 5.364 18.636 0 12.01 0z"/></svg>Zendesk</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M17.3877 10.5603C17.3877 10.1938 17.6766 9.9048 18.0432 9.9048C20.3107 9.9048 22.1457 11.7399 22.1457 14.0074C22.1457 16.2749 20.3107 18.1099 18.0432 18.1099H15.3397V15.8424H18.0432C19.0553 15.8424 19.8782 15.0195 19.8782 14.0074C19.8782 12.9952 19.0553 12.1724 18.0432 12.1724C17.6766 12.1724 17.3877 11.8834 17.3877 11.5169V10.5603ZM6.6123 13.4397C6.6123 13.8063 6.3234 14.0952 5.9568 14.0952C3.6893 14.0952 1.8543 12.2602 1.8543 9.9927C1.8543 7.7252 3.6893 5.8901 5.9568 5.8901H8.6603V8.1576H5.9568C4.9447 8.1576 4.1218 8.9805 4.1218 9.9927C4.1218 11.0048 4.9447 11.8277 5.9568 11.8277C6.3234 11.8277 6.6123 12.1166 6.6123 12.4831V13.4397ZM8.6602 18.1099C7.404 18.1099 6.3853 17.0912 6.3853 15.835V14.0952C6.3853 12.839 7.404 11.8203 8.6602 11.8203V14.0952H10.9277V15.835C10.9277 17.0912 9.9089 18.1099 8.6527 18.1099H8.6602ZM15.3397 5.8901C16.596 5.8901 17.6147 6.9088 17.6147 8.165V9.9048C17.6147 11.161 16.596 12.1797 15.3397 12.1797V9.9048H13.0723V8.165C13.0723 6.9088 14.0911 5.8901 15.3473 5.8901H15.3397Z"/></svg>Netlify</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M21.362 9.354H12V14.22h5.39c-.49 2.543-2.761 4.417-5.39 4.417-3.004 0-5.44-2.436-5.44-5.44 0-3.005 2.436-5.442 5.44-5.442 1.332 0 2.552.48 3.496 1.276l3.592-3.592C17.226 3.675 14.762 2.72 12 2.72 6.863 2.72 2.72 6.863 2.72 12c0 5.136 4.143 9.28 9.28 9.28 5.136 0 9.28-4.144 9.28-9.28 0-.553-.048-1.1-.138-1.634l.22-.012z"/></svg>Supabase</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M19.655 14.262c0 1.41-.672 2.143-2.013 2.143h-2.398v-4.283h2.398c1.341 0 2.013.732 2.013 2.14zm-2.12-6.478c0-1.29-.615-1.934-1.846-1.934H13.39v3.87h2.299c1.231 0 1.846-.646 1.846-1.935zM24 12c0 6.627-5.373 12-12 12S0 18.627 0 12 5.373 0 12 0s12 5.373 12 12zm-5.478 2.262c0-1.232-.464-2.197-1.382-2.878.75-.679 1.133-1.574 1.133-2.691 0-2.357-1.47-3.509-4.437-3.509H10.34v12.632h3.617c2.98 0 4.564-1.18 4.564-3.554z"/></svg>Calendly</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M12.502 0c-6.6 0-9.958 4.388-9.958 9.497 0 4.401 2.644 7.95 6.574 7.95 1.736 0 3.096-.95 3.58-2.2.168-.397.265-.84.265-1.314 0-.577-.136-1.197-.462-1.906-.577-1.246-.785-1.892-.785-2.638 0-2.13 1.614-4.143 4.614-4.143 2.507 0 4.36 1.752 4.36 4.323 0 2.815-1.504 5.953-4.037 5.953-.975 0-1.66-.672-1.66-1.653 0-.96.643-1.946 1.332-2.97.676-1.012 1.397-2.07 1.397-3.17 0-1.08-.786-1.942-2.098-1.942-1.618 0-2.89 1.433-2.89 3.38 0 .843.201 1.568.498 2.191l-2.05 8.69c-.42 1.736-.157 4.224-.06 4.915.036.276.353.337.494.12.21-.315 2.738-3.893 3.328-6.048.204-.744 1.04-3.786 1.04-3.786.551 1.02 2.098 1.872 3.764 1.872C21.117 17.121 24 13.342 24 8.716 24 4.297 20.564 0 12.502 0z"/></svg>Typeform</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M15.852 8.981h-4.588V0h4.588c2.476 0 4.49 2.014 4.49 4.49s-2.014 4.491-4.49 4.491zM12.735 7.51h3.117a3.007 3.007 0 0 0 3.019-3.019 3.007 3.007 0 0 0-3.019-3.019h-3.117v6.038zm-1.47 1.471H8.148c-2.477 0-4.49-2.014-4.49-4.49S5.67 0 8.148 0h3.117v8.981zm-1.471-7.51H8.148A3.007 3.007 0 0 0 5.13 4.491a3.007 3.007 0 0 0 3.019 3.019h1.647V1.471zm1.471 17.058h3.117c2.476 0 4.49-2.015 4.49-4.491s-2.014-4.49-4.49-4.49h-4.588v8.981h1.471zm0-7.51h3.117a3.007 3.007 0 0 1 3.019 3.019 3.007 3.007 0 0 1-3.019 3.02h-3.117v-6.039zM8.148 24c-2.477 0-4.49-2.015-4.49-4.491v-4.49h4.49a4.491 4.491 0 0 1 0 8.981zM5.13 16.49v2.52A3.007 3.007 0 0 0 8.148 22.028a3.007 3.007 0 0 0 3.019-3.019 3.007 3.007 0 0 0-3.019-3.019H5.13zm6.334-1.471H8.148a4.491 4.491 0 0 1 0-8.981h4.588v8.981h-1.272zm-1.2-7.51H8.148A3.007 3.007 0 0 0 5.13 10.528a3.007 3.007 0 0 0 3.019 3.019h2.116V7.51z"/></svg>Figma</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M14.975 0C11.326 0 8.958 1.936 8.207 5.799c1.352-1.936 2.929-2.663 4.74-2.187.969.252 1.665.977 2.434 1.784C16.667 6.73 18.167 8.28 21.333 8.28c3.649 0 6.017-1.936 6.768-5.799-1.352 1.936-2.929 2.663-4.74 2.187-.969-.252-1.665-.977-2.434-1.784C19.641 1.55 18.141 0 14.975 0zM8.207 8.28C4.558 8.28 2.19 10.216 1.44 14.079c1.352-1.936 2.929-2.663 4.74-2.187.969.252 1.665.977 2.434 1.784C9.9 15.01 11.4 16.56 14.566 16.56c3.649 0 6.017-1.936 6.768-5.799-1.352 1.936-2.929 2.663-4.74 2.187-.969-.252-1.665-.977-2.434-1.784C12.874 9.83 11.374 8.28 8.207 8.28z" transform="scale(.8) translate(1.5 4)"/></svg>SendGrid</span>
    </div>
  </div>
  <div class="marquee-wrap marquee-wrap--reverse">
    <div class="marquee__track">
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M16.5088 16.8447C16.5088 18.6924 14.8847 19.5765 12.7847 19.5765C10.9576 19.5765 9.35059 18.7835 8.48471 17.4894L6.38824 18.6953C7.69412 20.6541 9.97412 21.8824 12.7624 21.8824C16.3982 21.8824 19.0988 19.9412 19.0988 16.78C19.0988 10.3953 10.3718 11.5565 10.3718 7.81647C10.3718 6.36 11.6329 5.47765 13.2353 5.47765C14.5635 5.47765 15.7306 6.11294 16.4876 7.09412L18.5182 5.78824C17.3571 4.00941 15.4159 2.91765 13.2353 2.91765C10.0518 2.91765 7.78118 4.92706 7.78118 7.84235C7.78118 14.1612 16.5088 12.8329 16.5088 16.8447ZM3.19765 12.3953C3.19765 17.4671 7.32706 21.6 12.4024 21.6C13.7976 21.6 15.1259 21.2824 16.3318 20.72L15.4494 18.7565C14.5188 19.1859 13.4906 19.4118 12.4024 19.4118C8.53412 19.4118 5.38824 16.2659 5.38824 12.3953C5.38824 8.52706 8.53412 5.38118 12.4024 5.38118C13.4906 5.38118 14.5188 5.60471 15.4494 6.03647L16.3318 4.07294C15.1259 3.51059 13.7976 3.19059 12.4024 3.19059C7.32706 3.19059 3.19765 7.32 3.19765 12.3953ZM0 12.3953C0 19.2353 5.56235 24.7976 12.4024 24.7976C15.0847 24.7976 17.5694 23.9153 19.5953 22.4047L18.4118 20.64C16.7435 21.8682 14.6612 22.6082 12.4024 22.6082C6.77647 22.6082 2.18824 18.0212 2.18824 12.3953C2.18824 6.77176 6.77647 2.18353 12.4024 2.18353C14.6612 2.18353 16.7435 2.92235 18.4118 4.15059L19.5953 2.38824C17.5694 0.877647 15.0847 0 12.4024 0C5.56235 0 0 5.55765 0 12.3953Z" transform="scale(.83) translate(2.4 0)"/></svg>Cloudflare</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M4.459 4.208c.746.606 1.026.56 2.428.466l13.215-.793c.28 0 .047-.28-.046-.326L17.86 1.968c-.42-.326-.98-.7-2.055-.607L2.84 2.298c-.466.046-.56.28-.374.466zm.793 3.08v13.904c0 .747.373 1.027 1.214.98l14.523-.84c.84-.046.933-.56.933-1.167V6.354c0-.606-.233-.933-.746-.886l-15.177.887c-.56.046-.747.326-.747.933zm14.337.745c.093.42 0 .84-.42.886l-.7.14v10.264c-.608.327-1.168.514-1.635.514-.747 0-.933-.234-1.494-.933l-4.577-7.186v6.952l1.448.327s0 .84-1.168.84l-3.22.186c-.094-.186 0-.653.327-.746l.84-.233V8.755L7.96 8.662c-.094-.42.14-1.026.793-1.073l3.453-.233 4.764 7.279v-6.44l-1.215-.14c-.093-.514.28-.886.747-.933zM1.936 1.035l13.872-.933c1.7-.14 2.1.046 2.8.56l3.873 2.706c.467.327.607.42.607.793v17.19c0 1.073-.394 1.7-1.775 1.793l-15.457.934c-1.026.046-1.54-.094-2.1-.747L1.31 20.2c-.56-.747-.793-1.306-.793-1.96V2.762c0-.84.394-1.587 1.42-1.727z"/></svg>Notion</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M12 0C5.373 0 0 5.373 0 12s5.373 12 12 12 12-5.373 12-12S18.627 0 12 0zm0 2.206a9.794 9.794 0 1 1 0 19.588 9.794 9.794 0 0 1 0-19.588zm-.884 3.665L7.56 12l3.556 6.129h3.808L11.368 12l3.556-6.129z"/></svg>CircleCI</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M22.547 7.539l-1.164-.6a10.843 10.843 0 00-.479-1.154l.555-1.189a.39.39 0 00-.074-.449l-1.528-1.534a.39.39 0 00-.447-.076l-1.186.558a10.85 10.85 0 00-1.149-.48l-.596-1.166a.39.39 0 00-.348-.213H13.96a.39.39 0 00-.348.215l-.6 1.164a10.843 10.843 0 00-1.154.479l-1.189-.555a.389.389 0 00-.449.074L8.686 4.14a.39.39 0 00-.076.447l.558 1.186c-.174.372-.335.758-.48 1.149L7.522 7.52a.39.39 0 00-.213.348v2.171a.39.39 0 00.215.348l1.164.6c.14.393.3.779.479 1.154l-.555 1.189a.389.389 0 00.074.449l1.534 1.528a.39.39 0 00.447.076l1.186-.558c.372.174.758.335 1.149.48l.596 1.166a.39.39 0 00.348.213h2.171a.39.39 0 00.348-.215l.6-1.164c.393-.14.779-.3 1.154-.479l1.189.555a.389.389 0 00.449-.074l1.528-1.534a.39.39 0 00.076-.447l-.558-1.186c.174-.372.335-.758.48-1.149l1.166-.596a.39.39 0 00.213-.348v-2.17a.39.39 0 00-.215-.348zm-7.547 4.46c-1.66 0-3.004-1.344-3.004-3.004 0-1.66 1.344-3.004 3.004-3.004 1.66 0 3.004 1.344 3.004 3.004 0 1.66-1.344 3.004-3.004 3.004zM7.004 17.539l-.808-.416a7.516 7.516 0 01-.332-.8l.385-.825a.27.27 0 00-.051-.311L5.133 14.12a.27.27 0 00-.31-.053l-.824.387a7.522 7.522 0 01-.797-.333l-.414-.81A.27.27 0 002.546 13.1H1.04a.27.27 0 00-.242.149l-.416.808a7.516 7.516 0 01-.8.332L.243 14c0-.011-.028-.02-.051.032l-.019.02L.135 14.1a.27.27 0 00-.053.31l.387.824c-.121.258-.234.526-.333.797l-.81.414A.27.27 0 00-.887 16.7v1.506a.27.27 0 00.149.242l.808.416c.097.273.206.542.332.8l-.385.825a.27.27 0 00.051.311l1.065 1.067a.27.27 0 00.31.053l.824-.387c.258.121.526.234.797.333l.414.81c.05.098.149.16.259.148h1.506a.27.27 0 00.242-.149l.416-.808c.273-.097.542-.206.8-.332l.825.385a.27.27 0 00.311-.051l1.067-1.065a.27.27 0 00.053-.31l-.387-.824c.121-.258.234-.526.333-.797l.81-.414a.27.27 0 00.148-.259v-1.506a.27.27 0 00-.149-.242zm-3.753 3.457a2.084 2.084 0 110-4.168 2.084 2.084 0 010 4.168z"/></svg>Grafana</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M7.076 21.337H2.47a.641.641 0 0 1-.633-.74L4.944.901C5.026.382 5.474 0 5.998 0h7.46c2.57 0 4.578.543 5.69 1.81 1.01 1.15 1.304 2.42 1.012 4.287-.023.143-.047.288-.077.437-.983 5.05-4.349 6.797-8.647 6.797h-2.19c-.524 0-.968.382-1.05.9l-1.12 7.106zm14.146-14.42a3.35 3.35 0 0 0-.607-.541c1.394 3.062-.144 6.598-5.254 6.598h-2.19c-.524 0-.968.382-1.05.9l-1.12 7.106H7.076a.641.641 0 0 1-.633-.74l.166-1.053 1.009-6.393a1.076 1.076 0 0 1 1.05-.9h2.19c4.298 0 7.664-1.748 8.647-6.797.03-.15.054-.294.077-.437a4.835 4.835 0 0 0 .64-1.743z"/></svg>PayPal</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M11.52 2.137A1.379 1.379 0 0 0 10.144.76H1.38A1.376 1.376 0 0 0 0 2.137v19.726A1.376 1.376 0 0 0 1.38 23.24h8.764a1.379 1.379 0 0 0 1.376-1.377V2.137Zm1.334 4.836h9.769A1.376 1.376 0 0 1 24 8.35v13.513a1.376 1.376 0 0 1-1.377 1.377h-9.77a1.376 1.376 0 0 1-1.376-1.377V8.35a1.376 1.376 0 0 1 1.377-1.377ZM12.854.76h9.769A1.376 1.376 0 0 1 24 2.137v2.6a1.376 1.376 0 0 1-1.377 1.377h-9.77a1.376 1.376 0 0 1-1.376-1.377v-2.6A1.376 1.376 0 0 1 12.854.76Z"/></svg>Airtable</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M18.78 14.58c-1.26.6-2.16.12-2.82-.78l-5.4-7.74c-.06-.06-.12-.18-.18-.18-.06 0-.12.06-.12.18v8.88c0 1.08-.84 2.28-2.22 2.76-1.68.6-3.36.06-3.72-1.2-.36-1.26.72-2.7 2.4-3.3.9-.3 1.8-.36 2.52-.12V4.56c0-.42-.18-.66-.54-.72-.12 0-.24-.06-.36-.06h-.06C4.26 3.72 0 8.16 0 13.56 0 19.32 4.68 24 10.44 24c5.28 0 9.6-3.84 10.32-8.88-.36.24-1.26.72-1.98.42z"/></svg>Asana</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M21.98 7.448L19.62 0H4.347L2.02 7.448c-1.352 4.312.03 9.206 3.815 12.015L12.007 24l6.157-4.552c3.755-2.78 5.182-7.688 3.815-12z"/></svg>Auth0</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M23.156 7.394c0-1.028-.357-1.903-1.064-2.61-.707-.714-1.572-1.064-2.6-1.064-.55 0-1.064.136-1.543.4-.48.271-.857.636-1.136 1.1-.278.464-.414.964-.414 1.5v.107c-.107-.043-.279-.064-.507-.064h-.572c-.229 0-.4.021-.507.064v-.107c0-.536-.143-1.036-.421-1.5-.279-.464-.657-.829-1.136-1.1a2.899 2.899 0 0 0-1.5-.4c-1.036 0-1.907.35-2.614 1.064-.707.707-1.064 1.582-1.064 2.61 0 .736.2 1.407.593 2.029.4.614.907 1.064 1.536 1.35l5.921 2.536a.847.847 0 0 0 .357.071c.136 0 .25-.021.357-.071L23.156 10.773c.636-.286 1.143-.736 1.536-1.35.393-.622.593-1.293.593-2.029zm-9.42 4.714L7.635 9.594c-.414-.193-.75-.5-1.007-.907-.264-.414-.393-.857-.393-1.336 0-.707.25-1.307.75-1.807S7.892 4.8 8.599 4.8c.478 0 .914.121 1.3.357.386.243.693.572.921.993.229.414.343.871.343 1.357v.679c0 .314.136.507.4.571l.643.172a.69.69 0 0 0 .186.021.69.69 0 0 0 .186-.021l.636-.172c.271-.064.407-.257.407-.571v-.679c0-.486.114-.943.343-1.357.229-.421.536-.75.921-.993.386-.236.829-.357 1.307-.357.707 0 1.3.243 1.8.743s.75 1.1.75 1.807c0 .479-.129.922-.393 1.336a2.03 2.03 0 0 1-1.007.907l-6.1 2.514zM23.877 12.4l-1.264-.55a.33.33 0 0 0-.143-.028.39.39 0 0 0-.307.157.39.39 0 0 0-.064.343l.014.086c.014.05.021.1.021.143 0 .714-.407 1.336-1.221 1.871-.821.529-1.836.857-3.05 1l-3.95 1.636a.847.847 0 0 1-.357.071c-.136 0-.25-.021-.357-.071L9.249 15.422c-1.214-.143-2.229-.471-3.05-1-.814-.535-1.221-1.157-1.221-1.871 0-.043.007-.093.021-.143l.014-.086a.39.39 0 0 0-.064-.343.39.39 0 0 0-.307-.157.33.33 0 0 0-.143.028l-1.264.55c-.557.243-1.007.621-1.343 1.143-.343.521-.507 1.1-.507 1.728 0 1.121.557 2.064 1.671 2.821 1.057.714 2.464 1.207 4.2 1.457l4.307 1.786a.847.847 0 0 0 .357.071c.136 0 .25-.021.357-.071l4.307-1.786c1.736-.25 3.143-.743 4.2-1.457 1.114-.757 1.671-1.7 1.671-2.821 0-.629-.164-1.207-.507-1.728a3.325 3.325 0 0 0-1.343-1.143z" transform="scale(.87) translate(1.6 1)"/></svg>Webflow</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M.778 1.213a.768.768 0 0 0-.768.892l3.263 19.81c.084.5.515.868 1.022.873H19.95a.772.772 0 0 0 .77-.646l3.27-20.03a.768.768 0 0 0-.768-.891H.778zM14.52 15.53H9.522L8.17 8.466h7.561l-1.211 7.064z"/></svg>Bitbucket</span>
    </div>
    <div class="marquee__track" aria-hidden="true">
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M16.5088 16.8447C16.5088 18.6924 14.8847 19.5765 12.7847 19.5765C10.9576 19.5765 9.35059 18.7835 8.48471 17.4894L6.38824 18.6953C7.69412 20.6541 9.97412 21.8824 12.7624 21.8824C16.3982 21.8824 19.0988 19.9412 19.0988 16.78C19.0988 10.3953 10.3718 11.5565 10.3718 7.81647C10.3718 6.36 11.6329 5.47765 13.2353 5.47765C14.5635 5.47765 15.7306 6.11294 16.4876 7.09412L18.5182 5.78824C17.3571 4.00941 15.4159 2.91765 13.2353 2.91765C10.0518 2.91765 7.78118 4.92706 7.78118 7.84235C7.78118 14.1612 16.5088 12.8329 16.5088 16.8447ZM3.19765 12.3953C3.19765 17.4671 7.32706 21.6 12.4024 21.6C13.7976 21.6 15.1259 21.2824 16.3318 20.72L15.4494 18.7565C14.5188 19.1859 13.4906 19.4118 12.4024 19.4118C8.53412 19.4118 5.38824 16.2659 5.38824 12.3953C5.38824 8.52706 8.53412 5.38118 12.4024 5.38118C13.4906 5.38118 14.5188 5.60471 15.4494 6.03647L16.3318 4.07294C15.1259 3.51059 13.7976 3.19059 12.4024 3.19059C7.32706 3.19059 3.19765 7.32 3.19765 12.3953ZM0 12.3953C0 19.2353 5.56235 24.7976 12.4024 24.7976C15.0847 24.7976 17.5694 23.9153 19.5953 22.4047L18.4118 20.64C16.7435 21.8682 14.6612 22.6082 12.4024 22.6082C6.77647 22.6082 2.18824 18.0212 2.18824 12.3953C2.18824 6.77176 6.77647 2.18353 12.4024 2.18353C14.6612 2.18353 16.7435 2.92235 18.4118 4.15059L19.5953 2.38824C17.5694 0.877647 15.0847 0 12.4024 0C5.56235 0 0 5.55765 0 12.3953Z" transform="scale(.83) translate(2.4 0)"/></svg>Cloudflare</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M4.459 4.208c.746.606 1.026.56 2.428.466l13.215-.793c.28 0 .047-.28-.046-.326L17.86 1.968c-.42-.326-.98-.7-2.055-.607L2.84 2.298c-.466.046-.56.28-.374.466zm.793 3.08v13.904c0 .747.373 1.027 1.214.98l14.523-.84c.84-.046.933-.56.933-1.167V6.354c0-.606-.233-.933-.746-.886l-15.177.887c-.56.046-.747.326-.747.933zm14.337.745c.093.42 0 .84-.42.886l-.7.14v10.264c-.608.327-1.168.514-1.635.514-.747 0-.933-.234-1.494-.933l-4.577-7.186v6.952l1.448.327s0 .84-1.168.84l-3.22.186c-.094-.186 0-.653.327-.746l.84-.233V8.755L7.96 8.662c-.094-.42.14-1.026.793-1.073l3.453-.233 4.764 7.279v-6.44l-1.215-.14c-.093-.514.28-.886.747-.933zM1.936 1.035l13.872-.933c1.7-.14 2.1.046 2.8.56l3.873 2.706c.467.327.607.42.607.793v17.19c0 1.073-.394 1.7-1.775 1.793l-15.457.934c-1.026.046-1.54-.094-2.1-.747L1.31 20.2c-.56-.747-.793-1.306-.793-1.96V2.762c0-.84.394-1.587 1.42-1.727z"/></svg>Notion</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M12 0C5.373 0 0 5.373 0 12s5.373 12 12 12 12-5.373 12-12S18.627 0 12 0zm0 2.206a9.794 9.794 0 1 1 0 19.588 9.794 9.794 0 0 1 0-19.588zm-.884 3.665L7.56 12l3.556 6.129h3.808L11.368 12l3.556-6.129z"/></svg>CircleCI</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M22.547 7.539l-1.164-.6a10.843 10.843 0 00-.479-1.154l.555-1.189a.39.39 0 00-.074-.449l-1.528-1.534a.39.39 0 00-.447-.076l-1.186.558a10.85 10.85 0 00-1.149-.48l-.596-1.166a.39.39 0 00-.348-.213H13.96a.39.39 0 00-.348.215l-.6 1.164a10.843 10.843 0 00-1.154.479l-1.189-.555a.389.389 0 00-.449.074L8.686 4.14a.39.39 0 00-.076.447l.558 1.186c-.174.372-.335.758-.48 1.149L7.522 7.52a.39.39 0 00-.213.348v2.171a.39.39 0 00.215.348l1.164.6c.14.393.3.779.479 1.154l-.555 1.189a.389.389 0 00.074.449l1.534 1.528a.39.39 0 00.447.076l1.186-.558c.372.174.758.335 1.149.48l.596 1.166a.39.39 0 00.348.213h2.171a.39.39 0 00.348-.215l.6-1.164c.393-.14.779-.3 1.154-.479l1.189.555a.389.389 0 00.449-.074l1.528-1.534a.39.39 0 00.076-.447l-.558-1.186c.174-.372.335-.758.48-1.149l1.166-.596a.39.39 0 00.213-.348v-2.17a.39.39 0 00-.215-.348zm-7.547 4.46c-1.66 0-3.004-1.344-3.004-3.004 0-1.66 1.344-3.004 3.004-3.004 1.66 0 3.004 1.344 3.004 3.004 0 1.66-1.344 3.004-3.004 3.004zM7.004 17.539l-.808-.416a7.516 7.516 0 01-.332-.8l.385-.825a.27.27 0 00-.051-.311L5.133 14.12a.27.27 0 00-.31-.053l-.824.387a7.522 7.522 0 01-.797-.333l-.414-.81A.27.27 0 002.546 13.1H1.04a.27.27 0 00-.242.149l-.416.808a7.516 7.516 0 01-.8.332L.243 14c0-.011-.028-.02-.051.032l-.019.02L.135 14.1a.27.27 0 00-.053.31l.387.824c-.121.258-.234.526-.333.797l-.81.414A.27.27 0 00-.887 16.7v1.506a.27.27 0 00.149.242l.808.416c.097.273.206.542.332.8l-.385.825a.27.27 0 00.051.311l1.065 1.067a.27.27 0 00.31.053l.824-.387c.258.121.526.234.797.333l.414.81c.05.098.149.16.259.148h1.506a.27.27 0 00.242-.149l.416-.808c.273-.097.542-.206.8-.332l.825.385a.27.27 0 00.311-.051l1.067-1.065a.27.27 0 00.053-.31l-.387-.824c.121-.258.234-.526.333-.797l.81-.414a.27.27 0 00.148-.259v-1.506a.27.27 0 00-.149-.242zm-3.753 3.457a2.084 2.084 0 110-4.168 2.084 2.084 0 010 4.168z"/></svg>Grafana</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M7.076 21.337H2.47a.641.641 0 0 1-.633-.74L4.944.901C5.026.382 5.474 0 5.998 0h7.46c2.57 0 4.578.543 5.69 1.81 1.01 1.15 1.304 2.42 1.012 4.287-.023.143-.047.288-.077.437-.983 5.05-4.349 6.797-8.647 6.797h-2.19c-.524 0-.968.382-1.05.9l-1.12 7.106zm14.146-14.42a3.35 3.35 0 0 0-.607-.541c1.394 3.062-.144 6.598-5.254 6.598h-2.19c-.524 0-.968.382-1.05.9l-1.12 7.106H7.076a.641.641 0 0 1-.633-.74l.166-1.053 1.009-6.393a1.076 1.076 0 0 1 1.05-.9h2.19c4.298 0 7.664-1.748 8.647-6.797.03-.15.054-.294.077-.437a4.835 4.835 0 0 0 .64-1.743z"/></svg>PayPal</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M11.52 2.137A1.379 1.379 0 0 0 10.144.76H1.38A1.376 1.376 0 0 0 0 2.137v19.726A1.376 1.376 0 0 0 1.38 23.24h8.764a1.379 1.379 0 0 0 1.376-1.377V2.137Zm1.334 4.836h9.769A1.376 1.376 0 0 1 24 8.35v13.513a1.376 1.376 0 0 1-1.377 1.377h-9.77a1.376 1.376 0 0 1-1.376-1.377V8.35a1.376 1.376 0 0 1 1.377-1.377ZM12.854.76h9.769A1.376 1.376 0 0 1 24 2.137v2.6a1.376 1.376 0 0 1-1.377 1.377h-9.77a1.376 1.376 0 0 1-1.376-1.377v-2.6A1.376 1.376 0 0 1 12.854.76Z"/></svg>Airtable</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M18.78 14.58c-1.26.6-2.16.12-2.82-.78l-5.4-7.74c-.06-.06-.12-.18-.18-.18-.06 0-.12.06-.12.18v8.88c0 1.08-.84 2.28-2.22 2.76-1.68.6-3.36.06-3.72-1.2-.36-1.26.72-2.7 2.4-3.3.9-.3 1.8-.36 2.52-.12V4.56c0-.42-.18-.66-.54-.72-.12 0-.24-.06-.36-.06h-.06C4.26 3.72 0 8.16 0 13.56 0 19.32 4.68 24 10.44 24c5.28 0 9.6-3.84 10.32-8.88-.36.24-1.26.72-1.98.42z"/></svg>Asana</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M21.98 7.448L19.62 0H4.347L2.02 7.448c-1.352 4.312.03 9.206 3.815 12.015L12.007 24l6.157-4.552c3.755-2.78 5.182-7.688 3.815-12z"/></svg>Auth0</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M23.156 7.394c0-1.028-.357-1.903-1.064-2.61-.707-.714-1.572-1.064-2.6-1.064-.55 0-1.064.136-1.543.4-.48.271-.857.636-1.136 1.1-.278.464-.414.964-.414 1.5v.107c-.107-.043-.279-.064-.507-.064h-.572c-.229 0-.4.021-.507.064v-.107c0-.536-.143-1.036-.421-1.5-.279-.464-.657-.829-1.136-1.1a2.899 2.899 0 0 0-1.5-.4c-1.036 0-1.907.35-2.614 1.064-.707.707-1.064 1.582-1.064 2.61 0 .736.2 1.407.593 2.029.4.614.907 1.064 1.536 1.35l5.921 2.536a.847.847 0 0 0 .357.071c.136 0 .25-.021.357-.071L23.156 10.773c.636-.286 1.143-.736 1.536-1.35.393-.622.593-1.293.593-2.029zm-9.42 4.714L7.635 9.594c-.414-.193-.75-.5-1.007-.907-.264-.414-.393-.857-.393-1.336 0-.707.25-1.307.75-1.807S7.892 4.8 8.599 4.8c.478 0 .914.121 1.3.357.386.243.693.572.921.993.229.414.343.871.343 1.357v.679c0 .314.136.507.4.571l.643.172a.69.69 0 0 0 .186.021.69.69 0 0 0 .186-.021l.636-.172c.271-.064.407-.257.407-.571v-.679c0-.486.114-.943.343-1.357.229-.421.536-.75.921-.993.386-.236.829-.357 1.307-.357.707 0 1.3.243 1.8.743s.75 1.1.75 1.807c0 .479-.129.922-.393 1.336a2.03 2.03 0 0 1-1.007.907l-6.1 2.514zM23.877 12.4l-1.264-.55a.33.33 0 0 0-.143-.028.39.39 0 0 0-.307.157.39.39 0 0 0-.064.343l.014.086c.014.05.021.1.021.143 0 .714-.407 1.336-1.221 1.871-.821.529-1.836.857-3.05 1l-3.95 1.636a.847.847 0 0 1-.357.071c-.136 0-.25-.021-.357-.071L9.249 15.422c-1.214-.143-2.229-.471-3.05-1-.814-.535-1.221-1.157-1.221-1.871 0-.043.007-.093.021-.143l.014-.086a.39.39 0 0 0-.064-.343.39.39 0 0 0-.307-.157.33.33 0 0 0-.143.028l-1.264.55c-.557.243-1.007.621-1.343 1.143-.343.521-.507 1.1-.507 1.728 0 1.121.557 2.064 1.671 2.821 1.057.714 2.464 1.207 4.2 1.457l4.307 1.786a.847.847 0 0 0 .357.071c.136 0 .25-.021.357-.071l4.307-1.786c1.736-.25 3.143-.743 4.2-1.457 1.114-.757 1.671-1.7 1.671-2.821 0-.629-.164-1.207-.507-1.728a3.325 3.325 0 0 0-1.343-1.143z" transform="scale(.87) translate(1.6 1)"/></svg>Webflow</span>
      <span class="marquee__item"><svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M.778 1.213a.768.768 0 0 0-.768.892l3.263 19.81c.084.5.515.868 1.022.873H19.95a.772.772 0 0 0 .77-.646l3.27-20.03a.768.768 0 0 0-.768-.891H.778zM14.52 15.53H9.522L8.17 8.466h7.561l-1.211 7.064z"/></svg>Bitbucket</span>
    </div>
  </div>
</div>
</div>

<!-- WHY -->
<div class="section section-tint-1">
<div class="section-inner">
  <div class="section-label">Why</div>
  <h2 class="section-title">Know the second something happens</h2>
  <div class="section-desc">When a payment fails, a deploy breaks, or an error spikes &mdash; you find out from a desktop notification, not 20 minutes later when a customer complains.</div>

  <div class="why-grid">
    <div class="why-card">
      <div class="why-icon"><i data-lucide="layout-dashboard"></i></div>
      <h3>One feed, every service</h3>
      <p>Stripe, GitHub, Sentry, Slack, Linear &mdash; all in one terminal. No more switching between five dashboards to check if a webhook fired.</p>
    </div>
    <div class="why-card">
      <div class="why-icon"><i data-lucide="zap"></i></div>
      <h3>Instant reaction time</h3>
      <p>Desktop notifications the moment an event arrives. The faster you know something happened, the faster you fix it.</p>
    </div>
    <div class="why-card">
      <div class="why-icon"><i data-lucide="users"></i></div>
      <h3>Whole team, one command</h3>
      <p>Set it up once. Teammates follow your workspace and get every channel &mdash; current and future. No per-person configuration.</p>
    </div>
  </div>
</div>
</div>

<!-- USE CASES -->
<div class="section section-tint-2">
<div class="section-inner">
  <div class="section-label">Use Cases</div>
  <h2 class="section-title">How teams actually use it</h2>
  <div class="section-desc"></div>

  <div class="use-grid">
    <div class="use-card">
      <div class="use-icon ic-rose"><i data-lucide="alarm-clock"></i></div>
      <h3>Incident response</h3>
      <p>A Sentry error spikes at 2am. You get a desktop notification the second it fires &mdash; not 15 minutes later from a PagerDuty escalation. Open the TUI, inspect the payload, forward it to localhost, start debugging.</p>
    </div>
    <div class="use-card">
      <div class="use-icon ic-violet"><i data-lucide="credit-card"></i></div>
      <h3>Payment monitoring</h3>
      <p>Stripe sends <code>charge.failed</code> on a high-value B2B invoice. You see it immediately instead of discovering it in the dashboard the next morning. Catch failed payments before customers churn.</p>
    </div>
    <div class="use-card">
      <div class="use-icon ic-blue"><i data-lucide="rocket"></i></div>
      <h3>Deploy pipeline</h3>
      <p>GitHub push → Vercel <code>deployment.ready</code> or <code>deployment.error</code>. The whole team sees deploys in real time without watching GitHub Actions or the Vercel dashboard.</p>
    </div>
    <div class="use-card">
      <div class="use-icon ic-green"><i data-lucide="zap"></i></div>
      <h3>Quick webhook testing</h3>
      <p>Just added a webhook to your app? Run <code>dread init</code>, paste the URL, trigger an event. Instantly see the full payload, headers, and status in your terminal &mdash; no Postman, no <code>curl</code>, no log digging. Test it works before you ship.</p>
    </div>
    <div class="use-card">
      <div class="use-icon ic-cyan"><i data-lucide="code"></i></div>
      <h3>Local development</h3>
      <p>Building a webhook handler? <code>dread --forward http://localhost:3000/webhook</code> sends every real event to your local server. No ngrok, no tunnel config. Replay past events with one command.</p>
    </div>
    <div class="use-card">
      <div class="use-icon ic-amber"><i data-lucide="building-2"></i></div>
      <h3>Startups with no observability</h3>
      <p>No Datadog, no Grafana, no PagerDuty budget yet. Get webhook-driven awareness across every service for free with a single binary.</p>
    </div>
    <div class="use-card">
      <div class="use-icon ic-cyan"><i data-lucide="test-tube-2"></i></div>
      <h3>QA &amp; staging verification</h3>
      <p>Someone pushes to staging. QA follows the team workspace and sees the GitHub push, the Vercel deploy, and any Sentry errors &mdash; all in one terminal.</p>
    </div>
  </div>
</div>
</div>

<!-- QUICK START -->
<div class="section">
<div class="section-inner">
  <div class="section-label">Quick Start</div>
  <h2 class="section-title">Three commands. That's it.</h2>
  <div class="section-desc">Install, create a channel, paste the webhook URL into Stripe / GitHub / Sentry / anything. Desktop notifications start immediately.</div>

  <div class="steps">
    <div class="step-row">
      <div class="step-num"><span class="step-n step-n-1">1</span><span class="step-label">Install</span></div>
      <div class="step-content">
        <div class="copy-wrap">
          <pre><code>curl -sSL dread.sh/install | sh</code></pre>
          <button class="copy-btn" onclick="copyText('curl -sSL dread.sh/install | sh', this)" type="button"><i data-lucide="copy"></i></button>
        </div>
      </div>
    </div>
    <div class="step-row">
      <div class="step-num"><span class="step-n step-n-2">2</span><span class="step-label">Create a channel</span></div>
      <div class="step-content">
        <div class="copy-wrap">
          <pre><code><span class="kw">$</span> dread new "Stripe Prod"

<span class="o">Created channel: Stripe Prod (ch_stripe-prod_a1b2c3)
Webhook URL:     </span><span class="h">https://dread.sh/wh/ch_stripe-prod_a1b2c3</span></code></pre>
          <button class="copy-btn" onclick="copyText('dread new &quot;Stripe Prod&quot;', this)" type="button"><i data-lucide="copy"></i></button>
        </div>
      </div>
    </div>
    <div class="step-row">
      <div class="step-num"><span class="step-n step-n-3">3</span><span class="step-label">Wire up the webhook</span></div>
      <div class="step-content">
        <div class="copy-wrap">
          <pre><code><span class="c"># paste the URL into Stripe, GitHub, Slack, Linear...</span>
<span class="c"># notifications start automatically</span>
<span class="kw">$</span> dread <span class="c"># open the TUI anytime</span></code></pre>
          <button class="copy-btn" onclick="copyText('dread', this)" type="button"><i data-lucide="copy"></i></button>
        </div>
      </div>
    </div>
  </div>
</div>
</div>

<!-- WORKSPACE FLOW -->
<div class="section section-tint-2" id="workspace">
<div class="section-inner">
  <div class="section-label">Team Workspaces</div>
  <h2 class="section-title">Share once, sync forever</h2>
  <div class="section-desc">A workspace is your set of channels. Teammates follow it with one command. Every channel you add later auto-propagates on their next reconnect.</div>

  <div class="flow-grid">
    <div class="flow-card">
      <div class="flow-card-icon ic-green"><i data-lucide="plus"></i></div>
      <h3>Lead creates channels</h3>
      <p>Each <code>dread new</code> auto-publishes your workspace. No extra steps.</p>
      <div class="copy-wrap">
        <pre><code><span class="kw">$</span> dread new "Stripe Prod"
<span class="o">Webhook URL: </span><span class="h">https://dread.sh/wh/ch_stripe...</span>
<span class="o">Workspace published</span>

<span class="kw">$</span> dread new "GitHub Deploys"
<span class="o">Webhook URL: </span><span class="h">https://dread.sh/wh/ch_github...</span>
<span class="o">Workspace published</span></code></pre>
        <button class="copy-btn" onclick="copyText('dread new &quot;Stripe Prod&quot;', this)" type="button"><i data-lucide="copy"></i></button>
      </div>
    </div>

    <div class="flow-card">
      <div class="flow-card-icon ic-violet"><i data-lucide="share-2"></i></div>
      <h3>Share your workspace</h3>
      <p>One ID covers all your channels &mdash; current and future.</p>
      <div class="copy-wrap">
        <pre><code><span class="kw">$</span> dread share

<span class="o">Share this with your team:</span>
  <span class="h">dread follow ws_a1b2c3d4e5f6</span>

<span class="o">They'll get all your channels
(and any you add later).</span></code></pre>
        <button class="copy-btn" onclick="copyText('dread share', this)" type="button"><i data-lucide="copy"></i></button>
      </div>
    </div>

    <div class="flow-card flow-card-full">
      <div class="flow-inner">
        <div>
          <div class="flow-card-icon ic-blue"><i data-lucide="user-plus"></i></div>
          <h3>Teammates follow once</h3>
          <p>One command subscribes to every channel in the workspace. New channels sync automatically on reconnect &mdash; no manual adding.</p>
        </div>
        <div class="copy-wrap">
          <pre><code><span class="kw">$</span> curl -sSL dread.sh/install | sh
<span class="kw">$</span> dread follow <span class="ws">ws_a1b2c3d4e5f6</span>

<span class="o">Following workspace ws_a1b2... (3 channels):</span>
  <span class="o">Stripe Prod        ch_stripe-prod_a1b2c3</span>
  <span class="o">GitHub Deploys     ch_github-deploys_d4e5f6</span>
  <span class="o">Sentry Alerts      ch_sentry-alerts_g7h8i9</span>

<span class="o">New channels will sync automatically.</span></code></pre>
          <button class="copy-btn" onclick="copyText('dread follow ws_a1b2c3d4e5f6', this)" type="button"><i data-lucide="copy"></i></button>
        </div>
      </div>
    </div>
  </div>
</div>
</div>

<!-- FEATURES -->
<div class="section" id="features">
<div class="section-inner">
  <div class="section-label">Features</div>
  <h2 class="section-title">Everything you need, nothing you don't</h2>
  <div class="section-desc">No accounts, no config files, no environment variables. CLI or browser &mdash; your choice.</div>

  <div class="feat-grid">
    <div class="feat">
      <div class="feat-icon ic-green"><i data-lucide="bell"></i></div>
      <h3>Desktop notifications</h3>
      <p>Native macOS + Linux with customisable sounds. Background or terminal.</p>
    </div>
    <div class="feat">
      <div class="feat-icon ic-blue"><i data-lucide="terminal"></i></div>
      <h3>Terminal TUI</h3>
      <p>Live feed of all webhook events with full payload inspection.</p>
    </div>
    <div class="feat">
      <div class="feat-icon ic-violet"><i data-lucide="users"></i></div>
      <h3>Team workspaces</h3>
      <p>Follow a workspace once. New channels auto-sync on reconnect.</p>
    </div>
    <div class="feat">
      <div class="feat-icon ic-orange"><i data-lucide="layers"></i></div>
      <h3>Multiple channels</h3>
      <p>Separate channels per service &mdash; Stripe, GitHub, Slack, whatever.</p>
    </div>
    <div class="feat">
      <div class="feat-icon ic-amber"><i data-lucide="filter"></i></div>
      <h3>Event filtering</h3>
      <p>Filter by source, type, or content in the TUI and watch mode.</p>
    </div>
    <div class="feat">
      <div class="feat-icon ic-cyan"><i data-lucide="history"></i></div>
      <h3>Event history</h3>
      <p>Scroll back through past events, stored server-side.</p>
    </div>
    <div class="feat">
      <div class="feat-icon ic-rose"><i data-lucide="arrow-right-to-line"></i></div>
      <h3>Webhook forwarding</h3>
      <p>Forward events to localhost or any URL for local development.</p>
    </div>
    <div class="feat">
      <div class="feat-icon ic-green"><i data-lucide="rotate-ccw"></i></div>
      <h3>Event replay</h3>
      <p>Re-forward any past event to a URL for debugging.</p>
    </div>
    <div class="feat">
      <div class="feat-icon ic-blue"><i data-lucide="refresh-cw"></i></div>
      <h3>Auto-reconnect</h3>
      <p>Drops connection? Reconnects in 3s, picks up new channels.</p>
    </div>
    <div class="feat">
      <div class="feat-icon ic-orange"><i data-lucide="power"></i></div>
      <h3>Runs at login</h3>
      <p>Installs as a launchd/systemd service. Starts automatically.</p>
    </div>
    <div class="feat">
      <div class="feat-icon ic-violet"><i data-lucide="plug"></i></div>
      <h3>Works with everything</h3>
      <p>Auto-detects 60+ sources &mdash; Stripe, GitHub, Vercel, and more. Just paste the URL.</p>
    </div>
    <div class="feat">
      <div class="feat-icon ic-amber"><i data-lucide="layout-dashboard"></i></div>
      <h3>Web dashboard</h3>
      <p>View live events in the <a href="/dashboard" style="color:var(--amber)">browser</a> &mdash; no install needed.</p>
    </div>
    <div class="feat">
      <div class="feat-icon ic-cyan"><i data-lucide="message-square"></i></div>
      <h3>Slack / Discord</h3>
      <p>Forward events as rich messages to Slack or Discord channels.</p>
    </div>
    <div class="feat">
      <div class="feat-icon ic-rose"><i data-lucide="download"></i></div>
      <h3>Export &amp; Digest</h3>
      <p>Export events as JSON/CSV. Get daily digest summaries by source.</p>
    </div>
    <div class="feat">
      <div class="feat-icon ic-green"><i data-lucide="activity"></i></div>
      <h3>Status page</h3>
      <p>Public <a href="/status/demo" style="color:var(--accent)">status page</a> per workspace with live freshness indicators.</p>
    </div>
    <div class="feat">
      <div class="feat-icon ic-orange"><i data-lucide="bell-off"></i></div>
      <h3>Mute &amp; Alerts</h3>
      <p>Mute noisy channels. Set threshold alerts for event spikes.</p>
    </div>
    <div class="feat">
      <div class="feat-icon ic-amber"><i data-lucide="star"></i></div>
      <h3>Bookmarks &amp; Diff</h3>
      <p>Star important events. Auto-diff consecutive payloads from the same source.</p>
    </div>
    <div class="feat">
      <div class="feat-icon ic-cyan"><i data-lucide="command"></i></div>
      <h3>Command palette</h3>
      <p>Ctrl+P for fuzzy-searchable command palette. Full keyboard navigation.</p>
    </div>
    <div class="feat">
      <div class="feat-icon ic-violet"><i data-lucide="bar-chart-2"></i></div>
      <h3>Stats &amp; Swimlane</h3>
      <p>Source breakdown, status charts, heatmap, and per-source swimlane timeline.</p>
    </div>
    <div class="feat">
      <div class="feat-icon ic-rose"><i data-lucide="bell-ring"></i></div>
      <h3>Desktop notifications</h3>
      <p>Native desktop alerts the moment a webhook arrives. macOS and Linux.</p>
    </div>
  </div>
</div>
</div>

<!-- COMMANDS -->
<div class="section section-tint-3" id="commands">
<div class="section-inner">
  <div class="section-label">Reference</div>
  <h2 class="section-title">Commands</h2>
  <div class="section-desc"></div>

  <div class="cmd-grid">
    <div class="cmd-group">
      <div class="cmd-group-header">Basics</div>
      <pre><code>dread                       <span class="c"># launch TUI</span>
dread new "Stripe Prod"     <span class="c"># create a channel</span>
dread list                  <span class="c"># show channels + URLs</span>
dread logs                  <span class="c"># print recent events</span>
dread status                <span class="c"># channels + service info</span></code></pre>
    </div>
    <div class="cmd-group">
      <div class="cmd-group-header">Team</div>
      <pre><code>dread share                 <span class="c"># print workspace ID</span>
dread follow &lt;ws-id&gt;        <span class="c"># follow a workspace</span>
dread unfollow &lt;ws-id&gt;      <span class="c"># unfollow</span>
dread add &lt;id&gt; "Name"       <span class="c"># add single channel</span>
dread remove &lt;id&gt;           <span class="c"># remove a channel</span></code></pre>
    </div>
    <div class="cmd-group">
      <div class="cmd-group-header">Notifications</div>
      <pre><code>dread service install        <span class="c"># background service</span>
dread service uninstall      <span class="c"># remove service</span>
dread watch                  <span class="c"># headless mode</span>
dread watch --filter stripe  <span class="c"># filtered</span>
dread watch --slack &lt;url&gt;    <span class="c"># forward to Slack</span>
dread watch --discord &lt;url&gt;  <span class="c"># forward to Discord</span>
dread mute &lt;id&gt;              <span class="c"># silence a channel</span>
dread unmute &lt;id&gt;            <span class="c"># unmute a channel</span></code></pre>
    </div>
    <div class="cmd-group">
      <div class="cmd-group-header">Data &amp; Alerts</div>
      <pre><code>dread digest                <span class="c"># event summary (24h)</span>
dread digest --hours 8      <span class="c"># custom window</span>
dread alert add &lt;p&gt; &lt;n&gt; &lt;m&gt; <span class="c"># threshold alert</span>
dread alert list            <span class="c"># list rules</span>
dread alert remove &lt;idx&gt;    <span class="c"># remove a rule</span></code></pre>
    </div>
    <div class="cmd-group">
      <div class="cmd-group-header">Development</div>
      <pre><code>dread --forward http://...  <span class="c"># forward to local</span>
dread --filter payment      <span class="c"># filter TUI</span>
dread test &lt;id&gt;             <span class="c"># send test event</span>
dread replay &lt;event-id&gt;     <span class="c"># re-forward event</span></code></pre>
    </div>
  </div>
</div>
</div>

<!-- FOOTER -->
<footer>
  <div class="footer-inner">
    <span class="footer-brand">DREAD</span>
    <span class="footer-status"><span class="footer-status-dot"></span> Systems operational</span>
    <div class="footer-links">
      <a href="/docs">Docs</a>
      <a href="/blog">Blog</a>
      <a href="/changelog">Changelog</a>
      <a href="/download">Download</a>
      <a href="https://github.com/nigel-engel/dread.sh" target="_blank" aria-label="GitHub"><svg width="20" height="20" viewBox="0 0 24 24" fill="currentColor"><path d="M12 0C5.37 0 0 5.37 0 12c0 5.31 3.435 9.795 8.205 11.385.6.105.825-.255.825-.57 0-.285-.015-1.23-.015-2.235-3.015.555-3.795-.735-4.035-1.41-.135-.345-.72-1.41-1.23-1.695-.42-.225-1.02-.78-.015-.795.945-.015 1.62.87 1.845 1.23 1.08 1.815 2.805 1.305 3.495.99.105-.78.42-1.305.765-1.605-2.67-.3-5.46-1.335-5.46-5.925 0-1.305.465-2.385 1.23-3.225-.12-.3-.54-1.53.12-3.18 0 0 1.005-.315 3.3 1.23.96-.27 1.98-.405 3-.405s2.04.135 3 .405c2.295-1.56 3.3-1.23 3.3-1.23.66 1.65.24 2.88.12 3.18.765.84 1.23 1.905 1.23 3.225 0 4.605-2.805 5.625-5.475 5.925.435.375.81 1.095.81 2.22 0 1.605-.015 2.895-.015 3.3 0 .315.225.69.825.57A12.02 12.02 0 0024 12c0-6.63-5.37-12-12-12z"/></svg></a>
    </div>
  </div>
</footer>

<script>
lucide.createIcons();

function toggleTheme() {
  var root = document.documentElement;
  var icon = document.getElementById('theme-icon');
  if (root.classList.contains('light')) {
    root.classList.remove('light');
    localStorage.setItem('theme', 'dark');
    icon.setAttribute('data-lucide', 'moon');
  } else {
    root.classList.add('light');
    localStorage.setItem('theme', 'light');
    icon.setAttribute('data-lucide', 'sun');
  }
  lucide.createIcons({attrs:{class:'lucide'},nameAttr:'data-lucide'});
}

(function() {
  var saved = localStorage.getItem('theme');
  if (saved === 'light') {
    document.documentElement.classList.add('light');
    var icon = document.getElementById('theme-icon');
    if (icon) icon.setAttribute('data-lucide', 'sun');
    lucide.createIcons({attrs:{class:'lucide'},nameAttr:'data-lucide'});
  }

})();

(function() {
  var events = [
    {time:'1h ago', src:'stripe', cls:'source-stripe', msg:'charge.succeeded $120.00 on Visa ending 4242 — customer cus_NffrFeUfNV2Hib'},
    {time:'41m ago', src:'github', cls:'source-github', msg:'pull_request.merged #139 "Add webhook retry logic" → main by nigel'},
    {time:'33m ago', src:'sentry', cls:'source-sentry', msg:'TypeError: Cannot read properties of undefined (reading \'map\') in /api/webhooks/ingest'},
    {time:'24m ago', src:'vercel', cls:'source-vercel', msg:'deployment.ready dread-sh-git-main-a1b2c3.vercel.app promoted to production'},
    {time:'12m ago', src:'supabase', cls:'source-supabase', msg:'db-webhook INSERT on public.profiles row id 4a8f — triggered by auth.users update'},
    {time:'6m ago', src:'shopify', cls:'source-shopify', msg:'orders/create Order #1042 — 3 items, $89.00 USD, shipping to San Francisco, CA'},
    {time:'2m ago', src:'linear', cls:'source-linear', msg:'Issue.update DRD-42 "Webhook retries not working" status changed to In Review'},
    {time:'5s ago', src:'aws', cls:'source-aws', msg:'SNS CloudWatch alarm CPUUtilization > 90% on i-0a1b2c3d4e prod-api-2'}
  ];
  var body = document.getElementById('terminal-body');
  var title = document.getElementById('terminal-title');
  title.textContent = 'dread.sh - ' + events.length + ' events';
  events.forEach(function(e) {
    var row = document.createElement('div');
    row.className = 'terminal-row';
    row.innerHTML = '<span class="time">' + e.time + '</span><span class="' + e.cls + '">' + e.src + '</span><span>' + e.msg + '</span>';
    body.appendChild(row);
  });
})();

// Live stats
(function() {
  function fmt(n) {
    if (n >= 1000000) return (n/1000000).toFixed(1) + 'M';
    if (n >= 1000) return (n/1000).toFixed(1) + 'k';
    return String(n);
  }
  function load() {
    fetch('/api/live-stats').then(function(r) { return r.json(); }).then(function(d) {
      var wk = document.getElementById('ls-week');
      var tot = document.getElementById('ls-total');
      var up = document.getElementById('ls-uptime');
      if (wk) wk.textContent = fmt(d.EventsWeek || 0);
      if (tot) tot.textContent = fmt(d.EventsTotal || 0);
      if (up) {
        var days = d.UptimeDays || 0;
        var hrs = (d.UptimeHours || 0) % 24;
        up.textContent = days > 0 ? days + 'd ' + hrs + 'h' : hrs + 'h';
      }
    }).catch(function() {});
  }
  load();
  setInterval(load, 30000);
})();

function copyText(text, el) {
  if (typeof gtag === 'function') {
    gtag('event', 'copy_install_command', { event_category: 'engagement', event_label: text });
  }
  navigator.clipboard.writeText(text).then(function() {
    var btn = el.classList.contains('copy-btn') ? el : el.querySelector('.copy-btn');
    if (!btn) return;
    btn.classList.add('copied');
    var svg = btn.querySelector('svg');
    if (svg) svg.setAttribute('data-lucide', 'check');
    lucide.createIcons({attrs:{class:'lucide'},nameAttr:'data-lucide'});
    setTimeout(function() {
      btn.classList.remove('copied');
      if (svg) svg.setAttribute('data-lucide', 'copy');
      lucide.createIcons({attrs:{class:'lucide'},nameAttr:'data-lucide'});
    }, 1500);
  });
}
</script>
</body>
</html>`

const installScript = `#!/bin/sh
set -e

REPO="nigel-engel/dread.sh"
BINARY="dread"
INSTALL_DIR="$HOME/.local/bin"

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)

case "$ARCH" in
  x86_64|amd64) ARCH="amd64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *) echo "Unsupported architecture: $ARCH" >&2; exit 1 ;;
esac

case "$OS" in
  darwin|linux) ;;
  *) echo "Unsupported OS: $OS" >&2; exit 1 ;;
esac

TARBALL="${BINARY}_${OS}_${ARCH}.tar.gz"
URL="https://github.com/${REPO}/releases/latest/download/${TARBALL}"

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

echo "Downloading dread for ${OS}/${ARCH}..."
curl -fSL "$URL" -o "$TMPDIR/$TARBALL" || { echo "Download failed. Check your internet connection." >&2; exit 1; }
tar -xzf "$TMPDIR/$TARBALL" -C "$TMPDIR" || { echo "Failed to extract archive." >&2; exit 1; }

mkdir -p "$INSTALL_DIR"

# Stop running dread processes so we can replace the binary
if command -v pkill >/dev/null 2>&1; then
  pkill -f "dread watch" 2>/dev/null || true
fi
if [ "$OS" = "darwin" ]; then
  launchctl unload "$HOME/Library/LaunchAgents/dev.dread.watch.plist" 2>/dev/null || true
fi

# Remove old binary first (handles "text file busy" on some systems)
rm -f "$INSTALL_DIR/$BINARY" 2>/dev/null || true
mv "$TMPDIR/$BINARY" "$INSTALL_DIR/$BINARY"

chmod +x "$INSTALL_DIR/$BINARY"

NEW_VERSION=$("$INSTALL_DIR/$BINARY" --version 2>/dev/null || echo "latest")
echo "Installed dread ${NEW_VERSION} to $INSTALL_DIR/$BINARY"

# Set up background notifications
if [ "$OS" = "darwin" ]; then
  PLIST="$HOME/Library/LaunchAgents/dev.dread.watch.plist"
  mkdir -p "$HOME/Library/LaunchAgents"
  cat > "$PLIST" << PLISTEOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>dev.dread.watch</string>
	<key>ProgramArguments</key>
	<array>
		<string>$HOME/.local/bin/dread</string>
		<string>watch</string>
	</array>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>/tmp/dread-watch.log</string>
	<key>StandardErrorPath</key>
	<string>/tmp/dread-watch.log</string>
	<key>ProcessType</key>
	<string>Background</string>
</dict>
</plist>
PLISTEOF
  launchctl bootout gui/$(id -u) "$PLIST" 2>/dev/null || true
  launchctl bootstrap gui/$(id -u) "$PLIST"
  echo "Background notifications enabled (launchd)"

  # Build Dread.app for branded desktop notifications
  if command -v swiftc >/dev/null 2>&1; then
    APP_DIR="$HOME/.config/dread/Dread.app"
    rm -rf "$APP_DIR"
    mkdir -p "$APP_DIR/Contents/MacOS" "$APP_DIR/Contents/Resources"

    # Compile notifier
    cat > "$TMPDIR/notifier.swift" << 'SWIFTEOF'
import Cocoa
import UserNotifications
class D:NSObject,NSApplicationDelegate,UNUserNotificationCenterDelegate{func userNotificationCenter(_ c:UNUserNotificationCenter,willPresent n:UNNotification,withCompletionHandler h:@escaping(UNNotificationPresentationOptions)->Void){h([.banner,.sound])}}
let a=CommandLine.arguments;var t="dread.sh",m="",s="Sosumi";var i=1;while i<a.count{switch a[i]{case "-title" where i+1<a.count:i+=1;t=a[i];case "-message" where i+1<a.count:i+=1;m=a[i];case "-sound" where i+1<a.count:i+=1;s=a[i];default:break};i+=1}
let app=NSApplication.shared;let d=D();app.delegate=d;let c=UNUserNotificationCenter.current();c.delegate=d;let sem=DispatchSemaphore(value:0)
c.requestAuthorization(options:[.alert,.sound]){g,_ in guard g else{sem.signal();return};let n=UNMutableNotificationContent();n.title=t;n.body=m;n.sound=UNNotificationSound(named:UNNotificationSoundName(s));let r=UNNotificationRequest(identifier:UUID().uuidString,content:n,trigger:nil);c.add(r){_ in sem.signal()}};_=sem.wait(timeout:.now()+5)
SWIFTEOF
    swiftc "$TMPDIR/notifier.swift" -o "$APP_DIR/Contents/MacOS/Dread" -framework Cocoa -framework UserNotifications -suppress-warnings 2>/dev/null

    # Generate D_ icon
    curl -sL "https://dread.sh/icon.png" -o "$TMPDIR/icon.png"
    mkdir -p "$TMPDIR/dread.iconset"
    for sz in 16 32 64 128 256; do
      sips -z $sz $sz "$TMPDIR/icon.png" --out "$TMPDIR/dread.iconset/icon_${sz}x${sz}.png" >/dev/null 2>&1
    done
    cp "$TMPDIR/dread.iconset/icon_32x32.png" "$TMPDIR/dread.iconset/icon_16x16@2x.png"
    cp "$TMPDIR/dread.iconset/icon_64x64.png" "$TMPDIR/dread.iconset/icon_32x32@2x.png"
    cp "$TMPDIR/dread.iconset/icon_256x256.png" "$TMPDIR/dread.iconset/icon_128x128@2x.png"
    rm -f "$TMPDIR/dread.iconset/icon_64x64.png"
    iconutil -c icns "$TMPDIR/dread.iconset" -o "$APP_DIR/Contents/Resources/AppIcon.icns" 2>/dev/null

    # Info.plist
    cat > "$APP_DIR/Contents/Info.plist" << 'PLISTEOF2'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleExecutable</key><string>Dread</string>
<key>CFBundleIdentifier</key><string>sh.dread.notifier</string>
<key>CFBundleName</key><string>Dread</string>
<key>CFBundleIconFile</key><string>AppIcon</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>CFBundleVersion</key><string>1.0</string>
</dict></plist>
PLISTEOF2
    echo "Dread.app installed (branded notifications)"
  fi

elif [ "$OS" = "linux" ]; then
  UNIT_DIR="$HOME/.config/systemd/user"
  mkdir -p "$UNIT_DIR"
  cat > "$UNIT_DIR/dread-watch.service" << 'UNITEOF'
[Unit]
Description=dread webhook notifications
After=network-online.target

[Service]
ExecStart=%h/.local/bin/dread watch
Restart=always
RestartSec=3

[Install]
WantedBy=default.target
UNITEOF
  systemctl --user daemon-reload
  systemctl --user enable --now dread-watch.service
  echo "Background notifications enabled (systemd)"
fi

# Report successful install (non-blocking, silent)
curl -sS -X POST https://dread.sh/api/installed >/dev/null 2>&1 &

echo ""

# Auto-add to PATH if not already there
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *)
    LINE='export PATH="$HOME/.local/bin:$PATH"'
    RCFILE=""
    if [ -n "$ZSH_VERSION" ] || [ "$(basename "$SHELL")" = "zsh" ]; then
      RCFILE="$HOME/.zshrc"
    elif [ -f "$HOME/.bashrc" ]; then
      RCFILE="$HOME/.bashrc"
    elif [ -f "$HOME/.bash_profile" ]; then
      RCFILE="$HOME/.bash_profile"
    fi
    if [ -n "$RCFILE" ]; then
      if ! grep -qF '.local/bin' "$RCFILE" 2>/dev/null; then
        echo "" >> "$RCFILE"
        echo "$LINE" >> "$RCFILE"
      fi
      export PATH="$INSTALL_DIR:$PATH"
      echo "Added ~/.local/bin to PATH (in $RCFILE)"
    else
      echo "Add this to your shell profile:"
      echo "  $LINE"
    fi
    ;;
esac

echo ""
echo "Next: dread new \"My Channel\""
`

const docsPage = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="description" content="Complete documentation for dread — CLI commands, configuration, webhook setup, Slack and Discord forwarding, threshold alerts, and team workspaces.">
<link rel="canonical" href="https://dread.sh/docs">
<meta property="og:type" content="website">
<meta property="og:site_name" content="dread.sh">
<meta property="og:title" content="Documentation - dread.sh Webhook CLI">
<meta property="og:description" content="Complete documentation for dread — CLI commands, configuration, webhook setup, Slack and Discord forwarding, and team workspaces.">
<meta property="og:url" content="https://dread.sh/docs">
<meta property="og:image" content="https://dread.sh/og.png">
<meta name="twitter:card" content="summary_large_image">
<meta name="twitter:title" content="Documentation - dread.sh Webhook CLI">
<meta name="twitter:description" content="Complete documentation for dread — CLI commands, configuration, webhook setup, Slack and Discord forwarding, and team workspaces.">
<meta name="twitter:image" content="https://dread.sh/og.png">
<script>if(localStorage.getItem('theme')==='light')document.documentElement.classList.add('light')</script>
<link rel="icon" href="data:image/svg+xml,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 100 100'><circle cx='50' cy='50' r='40' fill='%23c37960'/></svg>">
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Press+Start+2P&display=swap">
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/geist@1.3.1/dist/fonts/geist-sans/style.min.css">
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/geist@1.3.1/dist/fonts/geist-mono/style.min.css">
<title>Documentation - dread.sh Webhook CLI</title>
<script src="https://unpkg.com/lucide@0.469.0/dist/umd/lucide.min.js"></script>
<style>
  :root {
    --bg: oklch(10% 0.003 256);
    --surface: oklch(16% 0.003 256);
    --surface-hover: oklch(20% 0.003 256);
    --border: oklch(23% 0.003 256);
    --border-subtle: oklch(18% 0.003 256);
    --text: oklch(98.5% 0.003 256);
    --text-secondary: oklch(70.5% 0.003 256);
    --text-muted: oklch(55.2% 0.003 256);
    --text-dim: oklch(40% 0.003 256);
    --accent: oklch(65% 0.1 40);
    --accent-dim: oklch(47% 0.09 36);
    --accent-glow: oklch(55% 0.1 38 / 0.15);
    --accent-glow-strong: oklch(55% 0.1 38 / 0.3);
    --orange: oklch(75% 0.18 55);
    --orange-dim: oklch(52% 0.16 55);
    --blue: oklch(70.7% 0.165 254.62);
    --violet: oklch(70.2% 0.183 293.54);
    --amber: oklch(82.8% 0.189 84.43);
    --rose: oklch(71.2% 0.194 13.43);
    --cyan: oklch(78.9% 0.154 211.53);
    --nav-bg: oklch(10% 0.003 256 / 0.85);
  }

  :root.light {
    --bg: oklch(98% 0.003 256);
    --surface: oklch(97% 0.003 256);
    --surface-hover: oklch(94% 0.003 256);
    --border: oklch(85% 0.003 256);
    --border-subtle: oklch(90% 0.003 256);
    --text: oklch(15% 0.003 256);
    --text-secondary: oklch(35% 0.003 256);
    --text-muted: oklch(50% 0.003 256);
    --text-dim: oklch(65% 0.003 256);
    --accent: oklch(50% 0.12 40);
    --accent-dim: oklch(40% 0.1 36);
    --accent-glow: oklch(50% 0.12 40 / 0.1);
    --accent-glow-strong: oklch(50% 0.12 40 / 0.2);
    --orange: oklch(55% 0.18 55);
    --orange-dim: oklch(45% 0.16 55);
    --blue: oklch(50% 0.165 254.62);
    --violet: oklch(50% 0.183 293.54);
    --amber: oklch(55% 0.189 84.43);
    --rose: oklch(55% 0.194 13.43);
    --cyan: oklch(50% 0.154 211.53);
    --nav-bg: oklch(98% 0.003 256 / 0.85);
  }

  * { margin: 0; padding: 0; box-sizing: border-box; }

  html, body { overscroll-behavior: none; }

  html { font-size: 18px; }

  body {
    font-family: "Geist", -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
    background: var(--bg);
    color: var(--text-secondary);
    line-height: 1.6;
    -webkit-font-smoothing: antialiased;
  }

  code, pre, kbd {
    font-family: "Geist Mono", ui-monospace, "Cascadia Code", "Source Code Pro", Menlo, Consolas, monospace;
  }

  /*! NAV_CSS */

  /* ---- DOCS LAYOUT ---- */
  .docs-layout {
    display: flex; min-height: 100vh; padding-top: 56px;
  }

  /* ---- SIDEBAR ---- */
  .docs-sidebar {
    width: 260px; position: fixed; top: 56px; bottom: 0; left: 0;
    border-right: 1px solid var(--border);
    background: var(--bg);
    overflow-y: auto; padding: 24px 0;
    z-index: 50;
  }
  .docs-sidebar::-webkit-scrollbar { width: 4px; }
  .docs-sidebar::-webkit-scrollbar-track { background: transparent; }
  .docs-sidebar::-webkit-scrollbar-thumb { background: var(--border); border-radius: 4px; }

  .docs-sidebar-group { margin-bottom: 8px; }
  .docs-sidebar-label {
    font-size: 0.7rem; text-transform: uppercase;
    letter-spacing: 0.08em; color: var(--text-muted);
    padding: 8px 24px 4px; font-weight: 600;
  }
  .docs-sidebar a {
    display: block; padding: 5px 24px 5px 28px;
    font-size: 0.8rem; color: var(--text-muted);
    text-decoration: none; transition: color 0.15s, background 0.15s;
    border-left: 2px solid transparent; margin-left: -1px;
  }
  .docs-sidebar a:hover { color: var(--text); }
  .docs-sidebar a.active {
    color: var(--accent); background: var(--accent-glow);
    border-left-color: var(--accent);
  }

  /* ---- CONTENT ---- */
  .docs-content {
    margin-left: 260px; flex: 1;
    max-width: 920px; padding: 48px 48px 120px;
  }
  .docs-section { margin-bottom: 64px; scroll-margin-top: 80px; }
  .docs-section h2 {
    font-size: 1.5rem; color: var(--text); font-weight: 700;
    letter-spacing: -0.02em; margin-bottom: 8px;
  }
  .docs-section h3 {
    font-size: 1.1rem; color: var(--text); font-weight: 600;
    letter-spacing: -0.01em; margin: 32px 0 12px;
  }
  .docs-section h3:first-child { margin-top: 0; }
  .docs-section p {
    font-size: 0.85rem; color: var(--text-secondary);
    line-height: 1.7; margin-bottom: 16px;
  }
  .docs-section ul, .docs-section ol {
    font-size: 0.85rem; color: var(--text-secondary);
    line-height: 1.7; margin: 0 0 16px 20px;
  }
  .docs-section li { margin-bottom: 4px; }
  .docs-section code {
    font-size: 0.8rem; background: var(--surface);
    padding: 2px 6px; border-radius: 4px; color: var(--accent);
  }
  .docs-section pre {
    background: var(--surface); border: 1px solid var(--border);
    border-radius: 8px; padding: 16px 20px; overflow-x: auto;
    font-size: 0.8rem; margin-bottom: 16px; line-height: 1.7;
  }
  .docs-section pre code {
    background: none; padding: 0; color: var(--text);
    border-radius: 0;
  }
  .docs-section .section-divider {
    border: none; border-top: 1px solid var(--border-subtle);
    margin: 32px 0;
  }

  /* code highlight classes */
  .c { color: var(--text-dim); }
  .o { color: var(--text-muted); }
  .h { color: var(--accent); }
  .kw { color: var(--orange); }

  /* ---- TABLES ---- */
  .docs-section table {
    width: 100%; border-collapse: collapse; margin-bottom: 16px;
    font-size: 0.8rem;
  }
  .docs-section th {
    text-align: left; padding: 10px 14px;
    background: var(--surface); color: var(--text);
    font-weight: 600; font-size: 0.75rem;
    text-transform: uppercase; letter-spacing: 0.04em;
    border: 1px solid var(--border);
  }
  .docs-section td {
    padding: 10px 14px; border: 1px solid var(--border);
    color: var(--text-secondary);
  }
  .docs-section td code {
    font-size: 0.75rem;
  }

  /* ---- COPY BUTTON ---- */
  .copy-wrap { position: relative; }
  .copy-btn {
    position: absolute; top: 8px; right: 8px;
    background: var(--border); border: 1px solid var(--border);
    border-radius: 6px; padding: 5px 6px; cursor: pointer;
    display: flex; align-items: center; justify-content: center;
    color: var(--text-muted);
    opacity: 0; transition: opacity 0.15s, color 0.15s, background 0.15s;
  }
  .copy-wrap:hover .copy-btn { opacity: 1; }
  .copy-btn:hover { color: var(--text); background: var(--surface-hover); }
  .copy-btn svg { width: 14px; height: 14px; pointer-events: none; }
  .copy-btn.copied { color: var(--accent); }

  .docs-overlay {
    display: none; position: fixed; inset: 0; top: 56px;
    background: oklch(0% 0 0 / 0.5); z-index: 40;
  }

  /* ---- MOBILE ---- */
  @media (max-width: 768px) {
    .docs-menu-btn { display: flex; }
    .docs-sidebar {
      transform: translateX(-100%);
      transition: transform 0.2s ease;
      z-index: 50; width: 280px;
    }
    .docs-sidebar.open { transform: translateX(0); }
    .docs-overlay.open { display: block; }
    .docs-content { margin-left: 0; padding: 32px 20px 120px; }
  }
</style>
</head>
<body>

<!-- NAV_HTML -->

<div class="docs-overlay" id="docs-overlay"></div>

<div class="docs-layout">
  <!-- SIDEBAR -->
  <aside class="docs-sidebar" id="docs-sidebar">
    <div class="docs-sidebar-group">
      <div class="docs-sidebar-label">Getting Started</div>
      <a href="#installation">Installation</a>
      <a href="#first-channel">Your First Channel</a>
      <a href="#wire-webhook">Wire Up a Webhook</a>
    </div>
    <div class="docs-sidebar-group">
      <div class="docs-sidebar-label">TUI Features</div>
      <a href="#tui-header">Header &amp; Logo</a>
      <a href="#tui-status">Status Indicators</a>
      <a href="#tui-sparkline">Sparkline &amp; Health</a>
      <a href="#tui-split">Split Pane</a>
      <a href="#tui-tabs">Tabs &amp; Stats</a>
      <a href="#tui-pause">Pause &amp; Toasts</a>
      <a href="#tui-filters">Advanced Filters</a>
      <a href="#tui-bookmarks">Bookmarks</a>
      <a href="#tui-diff">Auto-Diff</a>
      <a href="#tui-grouping">Event Grouping</a>
      <a href="#tui-palette">Command Palette</a>
      <a href="#tui-mouse">Mouse Support</a>
      <a href="#tui-export">HTML Export</a>
      <a href="#tui-forward">Forward Response</a>
      <a href="#tui-swimlane">Swimlane Timeline</a>
    </div>
    <div class="docs-sidebar-group">
      <div class="docs-sidebar-label">CLI Reference</div>
      <a href="#cli-dread">dread (TUI)</a>
      <a href="#cli-new">dread new</a>
      <a href="#cli-list">dread list</a>
      <a href="#cli-logs">dread logs</a>
      <a href="#cli-status">dread status</a>
      <a href="#cli-test">dread test</a>
      <a href="#cli-add-remove">dread add / remove</a>
      <a href="#cli-watch">dread watch</a>
      <a href="#cli-service">dread service</a>
      <a href="#cli-replay">dread replay</a>
    </div>
    <div class="docs-sidebar-group">
      <div class="docs-sidebar-label">Webhooks</div>
      <a href="#how-it-works">How It Works</a>
      <a href="#supported-sources">Supported Sources</a>
      <a href="#custom-webhooks">Custom Webhooks</a>
    </div>
    <div class="docs-sidebar-group">
      <div class="docs-sidebar-label">Team Workspaces</div>
      <a href="#sharing">Sharing a Workspace</a>
      <a href="#following">Following a Workspace</a>
      <a href="#auto-sync">Auto-Sync</a>
    </div>
    <div class="docs-sidebar-group">
      <div class="docs-sidebar-label">Notifications</div>
      <a href="#desktop-notifs">Desktop Notifications</a>
      <a href="#watch-mode">Watch Mode</a>
      <a href="#filtering">Filtering Events</a>
      <a href="#muting">Muting Channels</a>
      <a href="#alert-rules">Alert Rules</a>
    </div>
    <div class="docs-sidebar-group">
      <div class="docs-sidebar-label">Forwarding &amp; Replay</div>
      <a href="#forward">Forward to Localhost</a>
      <a href="#slack-discord-fwd">Slack / Discord</a>
      <a href="#replay">Replay Past Events</a>
    </div>
    <div class="docs-sidebar-group">
      <div class="docs-sidebar-label">Data &amp; Tools</div>
      <a href="#export">Export Events</a>
      <a href="#digest">Daily Digest</a>
      <a href="#status-page-doc">Status Page</a>
    </div>
    <div class="docs-sidebar-group">
      <div class="docs-sidebar-label">Web Dashboard</div>
      <a href="#dashboard-overview">Overview</a>
      <a href="#dashboard-connect">Connecting</a>
      <a href="#dashboard-features">Features</a>
    </div>
  </aside>

  <!-- CONTENT -->
  <main class="docs-content">

    <!-- GETTING STARTED -->
    <section class="docs-section" id="installation">
      <h2>Getting Started</h2>
      <h3>Installation</h3>
      <p>Install dread with a single command. The script downloads the correct binary for your platform and sets up a background service for desktop notifications.</p>
      <div class="copy-wrap">
        <pre><code>curl -sSL dread.sh/install | sh</code></pre>
        <button class="copy-btn" onclick="copyText('curl -sSL dread.sh/install | sh', this)" type="button"><i data-lucide="copy"></i></button>
      </div>
      <p>This will:</p>
      <ul>
        <li>Download the <code>dread</code> binary to <code>~/.local/bin</code></li>
        <li>Automatically add <code>~/.local/bin</code> to your PATH (no sudo required)</li>
        <li>Set up a background service (<code>launchd</code> on macOS, <code>systemd</code> on Linux) for desktop notifications</li>
        <li>Start listening for webhook events immediately</li>
      </ul>
      <p>Supported platforms: macOS and Linux (amd64 and arm64). Re-run the same command to update to the latest version.</p>
    </section>

    <section class="docs-section" id="first-channel">
      <h3>Your First Channel</h3>
      <p>A channel is a webhook endpoint. Create one for each service you want to receive events from.</p>
      <div class="copy-wrap">
        <pre><code><span class="kw">$</span> dread new "Stripe Prod"

<span class="o">Created channel: Stripe Prod (ch_stripe-prod_a1b2c3)</span>
<span class="o">Webhook URL:     </span><span class="h">https://dread.sh/wh/ch_stripe-prod_a1b2c3</span></code></pre>
        <button class="copy-btn" onclick="copyText('dread new &quot;Stripe Prod&quot;', this)" type="button"><i data-lucide="copy"></i></button>
      </div>
      <p>The command prints the channel ID and a webhook URL you can paste into any service's webhook settings.</p>
    </section>

    <section class="docs-section" id="wire-webhook">
      <h3>Wire Up a Webhook</h3>
      <p>Copy the webhook URL from the previous step and paste it into your service's webhook configuration (Stripe dashboard, GitHub repo settings, Slack app config, etc.).</p>
      <p>Once the service starts sending events, you'll see them immediately:</p>
      <div class="copy-wrap">
        <pre><code><span class="c"># paste the URL into Stripe, GitHub, Slack, Linear...</span>
<span class="c"># notifications start automatically</span>

<span class="kw">$</span> dread  <span class="c"># open the TUI to see events live</span></code></pre>
        <button class="copy-btn" onclick="copyText('dread', this)" type="button"><i data-lucide="copy"></i></button>
      </div>
      <p>Desktop notifications fire automatically from the background service. Open the TUI anytime to browse events with full payload inspection.</p>
    </section>

    <!-- CLI REFERENCE -->
    <!-- TUI FEATURES -->
    <section class="docs-section" id="tui-header">
      <h2>TUI Features</h2>
      <h3>Header &amp; Logo</h3>
      <p>When you launch <code>dread</code>, the TUI displays a 3-column header inside a rounded box:</p>
      <ul>
        <li><strong>Left column</strong> &mdash; ASCII art DREAD logo in the brand brown colour, with the version number below</li>
        <li><strong>Centre column</strong> &mdash; time-of-day greeting, connected channel count, session uptime, channel health dots, and success/failure/neutral event counts</li>
        <li><strong>Right column</strong> &mdash; total event count, events-per-minute rate, a sparkline showing event volume over the last hour, and rotating command tips that cycle every 5 seconds</li>
      </ul>
      <p>The header is always visible and updates in real time as events arrive.</p>
    </section>

    <section class="docs-section" id="tui-status">
      <h3>Status Indicators</h3>
      <p>Each event in the TUI list (and in the web dashboard) is classified and shown with a coloured dot:</p>
      <ul>
        <li><strong style="color:#98C379">&#9679; Green</strong> &mdash; success events (keywords: succeeded, completed, paid, captured, created, active, resolved, delivered, merged, approved, ready)</li>
        <li><strong style="color:#E06C75">&#9679; Red</strong> &mdash; failure events (keywords: fail, error, denied, declined, expired, canceled, refused, rejected, dispute, alert, incident, critical, warning, overdue)</li>
        <li><strong style="color:#666666">&#9679; Gray</strong> &mdash; neutral events that don't match either category</li>
      </ul>
      <p>Classification is based on keyword matching against the event type and summary fields. This works automatically with any webhook source.</p>
    </section>

    <section class="docs-section" id="tui-sparkline">
      <h3>Sparkline &amp; Health</h3>
      <p>The header's right column includes a Unicode sparkline (using block characters ▁▂▃▄▅▆▇█) that visualises event volume over the last 60 minutes in twelve 5-minute buckets.</p>
      <p><strong>Channel health dots</strong> appear in the centre column. Each channel shows a dot that is green if it received an event within the last 30 minutes, or gray if it's stale. This gives a quick at-a-glance view of which integrations are actively sending.</p>
      <p>The TUI also checks <code>/api/version</code> on startup and displays a notification in the header if a newer version of dread is available.</p>
    </section>

    <section class="docs-section" id="tui-split">
      <h3>Split Pane</h3>
      <p>Press <code>s</code> to toggle a master-detail split view. The screen divides into two halves: the event list on the left and the payload detail on the right. Selecting an event in the list instantly updates the right pane without leaving the list.</p>
      <p>This is ideal for debugging &mdash; you can navigate through events with <code>j</code>/<code>k</code> while continuously seeing the full payload beside it.</p>
    </section>

    <section class="docs-section" id="tui-tabs">
      <h3>Tabs &amp; Stats</h3>
      <p>The TUI has three tabs, switched with number keys:</p>
      <ul>
        <li><code>1</code> <strong>Live</strong> &mdash; the full event stream (default)</li>
        <li><code>2</code> <strong>Errors</strong> &mdash; auto-filtered to show only failure events (errors, denials, alerts, etc.)</li>
        <li><code>3</code> <strong>Stats</strong> &mdash; bar charts showing events by source, success/failure/neutral breakdown with percentages, and a 7-day &times; 24-hour activity heatmap</li>
      </ul>
      <p>The Errors tab directly serves the core use case: quickly seeing what failed. The Stats tab gives a bird's-eye view of webhook traffic patterns.</p>
    </section>

    <section class="docs-section" id="tui-pause">
      <h3>Pause &amp; Toasts</h3>
      <p>Press <code>p</code> or <code>Space</code> to pause the live event feed. While paused, new events are buffered in the background and a counter shows how many are waiting. Press <code>p</code> again to unpause and flush all buffered events.</p>
      <p>When a failure event arrives (regardless of pause state), a red toast notification appears above the footer showing the source and summary. Toasts auto-dismiss after 5 seconds. Up to 3 toasts can be visible at once.</p>
    </section>

    <section class="docs-section" id="tui-filters">
      <h3>Advanced Filters</h3>
      <p>Press <code>/</code> to open the filter prompt. The filter supports several modes:</p>
      <ul>
        <li><strong>Substring match</strong> &mdash; type any text to match against source, type, summary, channel, and the raw JSON payload</li>
        <li><strong>Exclusion</strong> &mdash; prefix with <code>!</code> to exclude matching events (e.g., <code>!test</code>)</li>
        <li><strong>Field-specific</strong> &mdash; use <code>source:stripe</code>, <code>type:checkout</code>, or <code>channel:prod</code> to filter by a specific field</li>
        <li><strong>Filter history</strong> &mdash; press <code>↑</code>/<code>↓</code> in the filter prompt to browse previous filters</li>
      </ul>
      <p>Deep payload search is also supported &mdash; filtering searches inside the raw JSON body, so you can find events by a customer ID or charge amount buried in the payload.</p>
    </section>

    <section class="docs-section" id="tui-bookmarks">
      <h3>Bookmarks</h3>
      <p>Press <code>f</code> to star/bookmark any event. Bookmarked events show a ★ indicator in the event list and the header shows a total bookmark count.</p>
      <p>Press <code>F</code> to toggle bookmark-only view, filtering the list to show only bookmarked events. Press <code>F</code> again to return to the full list.</p>
    </section>

    <section class="docs-section" id="tui-diff">
      <h3>Auto-Diff</h3>
      <p>Press <code>d</code> in the detail view to see a line-by-line diff of the current event's payload compared to the previous event from the same source. Added lines are shown in green, removed lines in red. Press <code>d</code> again to return to the normal payload view.</p>
    </section>

    <section class="docs-section" id="tui-grouping">
      <h3>Event Grouping</h3>
      <p>Press <code>g</code> to toggle event grouping. When enabled, consecutive events with the same source and type within 60 seconds are collapsed into a single row with a ×N badge showing the burst count. This reduces noise from high-volume event sources.</p>
    </section>

    <section class="docs-section" id="tui-palette">
      <h3>Command Palette</h3>
      <p>Press <code>Ctrl+P</code> to open a command palette overlay with fuzzy search. Type to filter commands, use ↑↓ to navigate, and Enter to execute. All major TUI actions are available through the palette.</p>
    </section>

    <section class="docs-section" id="tui-mouse">
      <h3>Mouse Support</h3>
      <p>The TUI supports mouse interaction. Click on an event in the list to select it. Scroll to navigate. In split pane mode, clicking an event also updates the detail pane.</p>
    </section>

    <section class="docs-section" id="tui-export">
      <h3>HTML Export</h3>
      <p>Press <code>x</code> to export the current session as a styled HTML report. The export includes all filtered events with timestamps, sources, types, summaries, status classification, and collapsible JSON payloads. The file is saved to the current directory.</p>
    </section>

    <section class="docs-section" id="tui-forward">
      <h3>Forward Response Capture</h3>
      <p>When using <code>--forward</code>, each forwarded event now shows a status badge in the event list (e.g. →200 for success, →err for failures). In the detail view, the full forward response is shown including HTTP status code, response headers, response body, and round-trip duration.</p>
    </section>

    <section class="docs-section" id="tui-swimlane">
      <h3>Swimlane Timeline</h3>
      <p>The Stats tab (press <code>3</code>) now includes a swimlane timeline visualization showing per-source event activity over the last 60 minutes. Each source gets its own horizontal lane with filled blocks indicating minutes with activity.</p>
    </section>


    <section class="docs-section" id="cli-dread">
      <h2>CLI Reference</h2>
      <h3>dread</h3>
      <p>Launch the interactive terminal UI. Shows a live feed of webhook events across all your channels with full payload inspection.</p>
      <div class="copy-wrap">
        <pre><code><span class="kw">$</span> dread [flags]</code></pre>
        <button class="copy-btn" onclick="copyText('dread', this)" type="button"><i data-lucide="copy"></i></button>
      </div>
      <table>
        <tr><th>Flag</th><th>Default</th><th>Description</th></tr>
        <tr><td><code>--server</code></td><td><code>dread.sh</code></td><td>Server URL to connect to</td></tr>
        <tr><td><code>--filter</code></td><td></td><td>Filter events by substring (source, type, summary, channel)</td></tr>
        <tr><td><code>--forward</code></td><td></td><td>Forward incoming events to a URL</td></tr>
      </table>
      <p><strong>Keybindings:</strong></p>
      <table>
        <tr><th>Key</th><th>Action</th></tr>
        <tr><td><code>q</code></td><td>Quit</td></tr>
        <tr><td><code>j</code> / <code>k</code></td><td>Navigate up / down</td></tr>
        <tr><td><code>enter</code></td><td>View event detail &amp; payload</td></tr>
        <tr><td><code>/</code></td><td>Filter events</td></tr>
        <tr><td><code>r</code></td><td>Replay event</td></tr>
        <tr><td><code>c</code></td><td>Copy webhook URL (list) or payload (detail)</td></tr>
        <tr><td><code>p</code> / <code>Space</code></td><td>Pause / resume live feed</td></tr>
        <tr><td><code>s</code></td><td>Toggle split pane (list + detail side-by-side)</td></tr>
        <tr><td><code>?</code></td><td>Show help overlay with all keybindings</td></tr>
        <tr><td><code>1</code></td><td>Live tab (all events)</td></tr>
        <tr><td><code>2</code></td><td>Errors tab (failures only)</td></tr>
        <tr><td><code>3</code></td><td>Stats tab (charts, heatmap &amp; swimlane)</td></tr>
        <tr><td><code>f</code></td><td>Bookmark / unbookmark event</td></tr>
        <tr><td><code>F</code></td><td>Toggle bookmarks-only view</td></tr>
        <tr><td><code>d</code></td><td>Diff with previous same-source event</td></tr>
        <tr><td><code>g</code></td><td>Toggle event grouping</td></tr>
        <tr><td><code>x</code></td><td>Export session as HTML</td></tr>
        <tr><td><code>Ctrl+P</code></td><td>Command palette</td></tr>
        <tr><td><code>esc</code></td><td>Back / clear filter</td></tr>
      </table>
    </section>

    <section class="docs-section" id="cli-new">
      <h3>dread new</h3>
      <p>Create a new webhook channel. Returns the channel ID and the webhook URL to paste into your service.</p>
      <div class="copy-wrap">
        <pre><code><span class="kw">$</span> dread new &lt;name&gt;</code></pre>
        <button class="copy-btn" onclick="copyText('dread new', this)" type="button"><i data-lucide="copy"></i></button>
      </div>
      <p><strong>Example:</strong></p>
      <div class="copy-wrap">
        <pre><code><span class="kw">$</span> dread new "GitHub Deploys"

<span class="o">Created channel: GitHub Deploys (ch_github-deploys_d4e5f6)</span>
<span class="o">Webhook URL:     </span><span class="h">https://dread.sh/wh/ch_github-deploys_d4e5f6</span></code></pre>
        <button class="copy-btn" onclick="copyText('dread new &quot;GitHub Deploys&quot;', this)" type="button"><i data-lucide="copy"></i></button>
      </div>
    </section>

    <section class="docs-section" id="cli-list">
      <h3>dread list</h3>
      <p>Show all channels with their IDs and webhook URLs.</p>
      <div class="copy-wrap">
        <pre><code><span class="kw">$</span> dread list

<span class="o">Stripe Prod      ch_stripe-prod_a1b2c3       </span><span class="h">https://dread.sh/wh/ch_stripe-prod_a1b2c3</span>
<span class="o">GitHub Deploys   ch_github-deploys_d4e5f6    </span><span class="h">https://dread.sh/wh/ch_github-deploys_d4e5f6</span>
<span class="o">Sentry Alerts    ch_sentry-alerts_g7h8i9     </span><span class="h">https://dread.sh/wh/ch_sentry-alerts_g7h8i9</span></code></pre>
        <button class="copy-btn" onclick="copyText('dread list', this)" type="button"><i data-lucide="copy"></i></button>
      </div>
    </section>

    <section class="docs-section" id="cli-logs">
      <h3>dread logs</h3>
      <p>Print recent webhook events to stdout. Useful for scripting or quick checks without opening the TUI.</p>
      <div class="copy-wrap">
        <pre><code><span class="kw">$</span> dread logs [--limit N]</code></pre>
        <button class="copy-btn" onclick="copyText('dread logs', this)" type="button"><i data-lucide="copy"></i></button>
      </div>
      <table>
        <tr><th>Flag</th><th>Default</th><th>Description</th></tr>
        <tr><td><code>--limit</code></td><td><code>20</code></td><td>Number of events to show</td></tr>
      </table>
      <p><strong>Example:</strong></p>
      <div class="copy-wrap">
        <pre><code><span class="kw">$</span> dread logs --limit 5

<span class="o">2m ago   stripe   invoice.paid $249.00</span>
<span class="o">5m ago   github   PR merged #139 → main</span>
<span class="o">12m ago  sentry   TypeError: Cannot read prop…</span>
<span class="o">18m ago  linear   Issue ENG-481 moved to Done</span>
<span class="o">25m ago  slack    #deploys: Production deploy v2.4.1</span></code></pre>
        <button class="copy-btn" onclick="copyText('dread logs --limit 5', this)" type="button"><i data-lucide="copy"></i></button>
      </div>
    </section>

    <section class="docs-section" id="cli-status">
      <h3>dread status</h3>
      <p>Show channel overview, last event timestamps, and background service info.</p>
      <div class="copy-wrap">
        <pre><code><span class="kw">$</span> dread status

<span class="o">Channels: 3</span>
<span class="o">  Stripe Prod      last event 2m ago</span>
<span class="o">  GitHub Deploys   last event 5m ago</span>
<span class="o">  Sentry Alerts    last event 12m ago</span>
<span class="o"></span>
<span class="o">Service: running (launchd)</span>
<span class="o">Server:  dread.sh (connected)</span></code></pre>
        <button class="copy-btn" onclick="copyText('dread status', this)" type="button"><i data-lucide="copy"></i></button>
      </div>
    </section>

    <section class="docs-section" id="cli-test">
      <h3>dread test</h3>
      <p>Send a test webhook event to a channel. Useful for verifying your setup end-to-end.</p>
      <div class="copy-wrap">
        <pre><code><span class="kw">$</span> dread test &lt;channel-id&gt;</code></pre>
        <button class="copy-btn" onclick="copyText('dread test', this)" type="button"><i data-lucide="copy"></i></button>
      </div>
      <p><strong>Example:</strong></p>
      <div class="copy-wrap">
        <pre><code><span class="kw">$</span> dread test ch_stripe-prod_a1b2c3

<span class="o">Sent test event to Stripe Prod (ch_stripe-prod_a1b2c3)</span></code></pre>
        <button class="copy-btn" onclick="copyText('dread test ch_stripe-prod_a1b2c3', this)" type="button"><i data-lucide="copy"></i></button>
      </div>
    </section>

    <section class="docs-section" id="cli-add-remove">
      <h3>dread add / remove</h3>
      <p>Manually subscribe to or unsubscribe from individual channels by ID.</p>
      <div class="copy-wrap">
        <pre><code><span class="kw">$</span> dread add &lt;channel-id&gt; "Display Name"
<span class="kw">$</span> dread remove &lt;channel-id&gt;</code></pre>
        <button class="copy-btn" onclick="copyText('dread add', this)" type="button"><i data-lucide="copy"></i></button>
      </div>
      <p><strong>Example:</strong></p>
      <div class="copy-wrap">
        <pre><code><span class="kw">$</span> dread add ch_stripe-prod_a1b2c3 "Stripe Prod"
<span class="o">Added channel: Stripe Prod</span>

<span class="kw">$</span> dread remove ch_stripe-prod_a1b2c3
<span class="o">Removed channel: ch_stripe-prod_a1b2c3</span></code></pre>
        <button class="copy-btn" onclick="copyText('dread add ch_stripe-prod_a1b2c3 &quot;Stripe Prod&quot;', this)" type="button"><i data-lucide="copy"></i></button>
      </div>
    </section>

    <section class="docs-section" id="cli-watch">
      <h3>dread watch</h3>
      <p>Run in headless notification mode. No TUI — just desktop notifications. The background service uses this internally, but you can run it manually too.</p>
      <div class="copy-wrap">
        <pre><code><span class="kw">$</span> dread watch [flags]</code></pre>
        <button class="copy-btn" onclick="copyText('dread watch', this)" type="button"><i data-lucide="copy"></i></button>
      </div>
      <table>
        <tr><th>Flag</th><th>Default</th><th>Description</th></tr>
        <tr><td><code>--server</code></td><td><code>dread.sh</code></td><td>Server URL to connect to</td></tr>
        <tr><td><code>--filter</code></td><td></td><td>Only notify on matching events</td></tr>
      </table>
      <p>Auto-reconnects after 3 seconds if the connection drops.</p>
    </section>

    <section class="docs-section" id="cli-service">
      <h3>dread service</h3>
      <p>Install or remove a background service so <code>dread watch</code> runs automatically &mdash; even after the terminal is closed or the machine restarts.</p>
      <div class="copy-wrap">
        <pre><code><span class="kw">$</span> dread service install
<span class="o">Background service installed and started.</span>
<span class="o">  Plist:  ~/Library/LaunchAgents/dev.dread.watch.plist</span>
<span class="o">  Logs:   ~/Library/Logs/dread.log</span>
<span class="o"></span>
<span class="o">Notifications will now appear even when the terminal is closed.</span></code></pre>
        <button class="copy-btn" onclick="copyText('dread service install', this)" type="button"><i data-lucide="copy"></i></button>
      </div>
      <table>
        <tr><th>Subcommand</th><th>Description</th></tr>
        <tr><td><code>install</code></td><td>Install and start the background service (<code>launchd</code> on macOS, <code>systemd</code> on Linux)</td></tr>
        <tr><td><code>uninstall</code></td><td>Stop and remove the background service</td></tr>
      </table>
      <p>On macOS, this creates a <code>launchd</code> agent that starts at login and auto-restarts on failure. On Linux, it creates a <code>systemd</code> user service. Logs are written to <code>~/Library/Logs/dread.log</code> (macOS) or available via <code>journalctl --user -u dread-watch</code> (Linux).</p>
      <p>Use <code>dread status</code> to check whether the background service is running.</p>
    </section>

    <section class="docs-section" id="cli-replay">
      <h3>dread replay</h3>
      <p>Re-forward a past event to a URL. Fetches the full event payload from the server and POSTs it to the target.</p>
      <div class="copy-wrap">
        <pre><code><span class="kw">$</span> dread replay &lt;event-id&gt; --forward &lt;url&gt;</code></pre>
        <button class="copy-btn" onclick="copyText('dread replay', this)" type="button"><i data-lucide="copy"></i></button>
      </div>
      <p><strong>Example:</strong></p>
      <div class="copy-wrap">
        <pre><code><span class="kw">$</span> dread replay evt_abc123 --forward http://localhost:3000/webhook

<span class="o">Replayed evt_abc123 → http://localhost:3000/webhook (200 OK)</span></code></pre>
        <button class="copy-btn" onclick="copyText('dread replay evt_abc123 --forward http://localhost:3000/webhook', this)" type="button"><i data-lucide="copy"></i></button>
      </div>
    </section>

    <!-- WEBHOOKS -->
    <section class="docs-section" id="how-it-works">
      <h2>Webhooks</h2>
      <h3>How It Works</h3>
      <p>When a service sends a POST request to your channel's webhook URL:</p>
      <ol>
        <li>The server receives the payload at <code>POST /wh/{channel-id}</code></li>
        <li>It auto-detects the source (Stripe, GitHub, etc.) from request headers</li>
        <li>It extracts the event type and a human-readable summary</li>
        <li>The event is stored server-side and broadcast to all connected clients via WebSocket</li>
        <li>Your desktop notification fires, and the TUI updates in real time</li>
      </ol>
    </section>

    <section class="docs-section" id="supported-sources">
      <h3>Supported Sources</h3>
      <p>dread auto-detects <strong>60+</strong> webhook sources from HTTP headers. Any unrecognised source is labelled "webhook" &mdash; add <code>?source=name</code> to your webhook URL to label it yourself.</p>
      <table>
        <tr><th>Category</th><th>Sources</th></tr>
        <tr><td>Payment &amp; Finance</td><td>Stripe, PayPal, Square, Razorpay, Paddle, Recurly, Coinbase, Plaid, Xero, QuickBooks</td></tr>
        <tr><td>Dev &amp; Code</td><td>GitHub, GitLab, Bitbucket, CircleCI, Travis CI, Buildkite</td></tr>
        <tr><td>Infrastructure</td><td>Vercel, Heroku, AWS SNS, Cloudflare</td></tr>
        <tr><td>Communication</td><td>Slack, Discord, Twilio, SendGrid, Mailchimp, Zendesk, Telegram, LINE</td></tr>
        <tr><td>Project Management</td><td>Linear, Jira, Notion, Trello, Airtable</td></tr>
        <tr><td>Monitoring</td><td>Sentry, PagerDuty, Grafana, Pingdom</td></tr>
        <tr><td>CMS &amp; Commerce</td><td>Shopify, WooCommerce, Contentful, Sanity, BigCommerce</td></tr>
        <tr><td>Auth &amp; Identity</td><td>Auth0, WorkOS, Svix (Clerk, Resend)</td></tr>
        <tr><td>Database</td><td>Supabase, PlanetScale</td></tr>
        <tr><td>SaaS</td><td>HubSpot, Typeform, Calendly, DocuSign, Zoom, Figma, Knock, Novu, LaunchDarkly, Customer.io, Pusher, Ably, Twitch, Zapier</td></tr>
      </table>
    </section>

    <section class="docs-section" id="custom-webhooks">
      <h3>Custom Webhooks</h3>
      <p>You can send webhooks from your own services. Just POST JSON to your channel URL:</p>
      <div class="copy-wrap">
        <pre><code><span class="kw">$</span> curl -X POST https://dread.sh/wh/ch_my-channel_abc123 \
  -H "Content-Type: application/json" \
  -d '{"event": "deploy.success", "env": "production"}'</code></pre>
        <button class="copy-btn" onclick="copyText('curl -X POST https://dread.sh/wh/ch_my-channel_abc123 -H &quot;Content-Type: application/json&quot; -d \'{}\'', this)" type="button"><i data-lucide="copy"></i></button>
      </div>
      <p>To set a custom source name, add <code>?source=name</code> to your webhook URL:</p>
      <div class="copy-wrap">
        <pre><code>https://dread.sh/wh/ch_my-channel_abc123<strong>?source=trigger.dev</strong></code></pre>
      </div>
      <p>This works with any service &mdash; just append <code>?source=</code> when pasting the URL into your webhook settings. You can also use the <code>X-Dread-Source</code> header for programmatic control:</p>
      <div class="copy-wrap">
        <pre><code><span class="kw">$</span> curl -X POST https://dread.sh/wh/ch_my-channel_abc123 \
  -H "Content-Type: application/json" \
  -H "X-Dread-Source: my-app" \
  -d '{"event": "deploy.success", "env": "production"}'</code></pre>
        <button class="copy-btn" onclick="copyText('curl -X POST https://dread.sh/wh/ch_my-channel_abc123 -H &quot;Content-Type: application/json&quot; -H &quot;X-Dread-Source: my-app&quot; -d \'{}\'', this)" type="button"><i data-lucide="copy"></i></button>
      </div>
    </section>

    <!-- TEAM WORKSPACES -->
    <section class="docs-section" id="sharing">
      <h2>Team Workspaces</h2>
      <h3>Sharing a Workspace</h3>
      <p>A workspace is your collection of channels. Share it with your team so everyone gets the same webhook feeds.</p>
      <div class="copy-wrap">
        <pre><code><span class="kw">$</span> dread share

<span class="o">Share this with your team:</span>
  <span class="h">dread follow ws_a1b2c3d4e5f6</span>

<span class="o">They'll get all your channels (and any you add later).</span></code></pre>
        <button class="copy-btn" onclick="copyText('dread share', this)" type="button"><i data-lucide="copy"></i></button>
      </div>
      <p>The workspace ID is generated from your local config. Running <code>dread share</code> publishes your current channel list to the server.</p>
    </section>

    <section class="docs-section" id="following">
      <h3>Following a Workspace</h3>
      <p>Teammates run a single command to subscribe to all channels in a workspace:</p>
      <div class="copy-wrap">
        <pre><code><span class="kw">$</span> dread follow ws_a1b2c3d4e5f6

<span class="o">Following workspace ws_a1b2... (3 channels):</span>
  <span class="o">Stripe Prod        ch_stripe-prod_a1b2c3</span>
  <span class="o">GitHub Deploys     ch_github-deploys_d4e5f6</span>
  <span class="o">Sentry Alerts      ch_sentry-alerts_g7h8i9</span>

<span class="o">New channels will sync automatically.</span></code></pre>
        <button class="copy-btn" onclick="copyText('dread follow ws_a1b2c3d4e5f6', this)" type="button"><i data-lucide="copy"></i></button>
      </div>
      <p>To stop following:</p>
      <div class="copy-wrap">
        <pre><code><span class="kw">$</span> dread unfollow ws_a1b2c3d4e5f6
<span class="o">Unfollowed workspace ws_a1b2c3d4e5f6</span></code></pre>
        <button class="copy-btn" onclick="copyText('dread unfollow ws_a1b2c3d4e5f6', this)" type="button"><i data-lucide="copy"></i></button>
      </div>
    </section>

    <section class="docs-section" id="auto-sync">
      <h3>Auto-Sync</h3>
      <p>When the workspace owner adds new channels, followers pick them up automatically on their next reconnect. No action needed — the background service handles it.</p>
      <p>The sync happens every time the WebSocket connection is established (on startup, after a network drop, etc.). Any new channels in the workspace are added to the follower's local config automatically.</p>
    </section>

    <!-- NOTIFICATIONS -->
    <section class="docs-section" id="desktop-notifs">
      <h2>Notifications</h2>
      <h3>Desktop Notifications</h3>
      <p>dread sends native desktop notifications for every webhook event. Run <code>dread service install</code> to set up a background service that starts at login and keeps running even after the terminal is closed.</p>
      <div class="copy-wrap">
        <pre><code><span class="kw">$</span> dread service install</code></pre>
        <button class="copy-btn" onclick="copyText('dread service install', this)" type="button"><i data-lucide="copy"></i></button>
      </div>
      <ul>
        <li><strong>macOS</strong> &mdash; uses a native notifier with sound. Notifications appear in Notification Centre.</li>
        <li><strong>Linux</strong> &mdash; uses <code>notify-send</code>. Works with any desktop environment that supports freedesktop notifications.</li>
      </ul>
      <p>This installs a <code>launchd</code> agent (macOS) or <code>systemd</code> user service (Linux) that auto-restarts on failure. To remove it, run <code>dread service uninstall</code>.</p>
      <h4 style="margin-top:24px;">Custom notification sound</h4>
      <p>Set the <code>"sound"</code> field in your config to change the notification sound (default: <code>Sosumi</code>):</p>
      <div class="copy-wrap">
        <pre><code>{
  "token": "dk_...",
  "channels": [...],
  <span class="kw">"sound": "Hero"</span>
}</code></pre>
      </div>
      <p><strong>macOS built-in sounds:</strong> Basso, Blow, Bottle, Frog, Funk, Glass, Hero, Morse, Ping, Pop, Purr, Sosumi, Submarine, Tink</p>
      <p>You can also use custom sounds on macOS by placing a <code>.aiff</code> file in <code>~/Library/Sounds/</code> and referencing it by name (without extension).</p>
      <p><strong>Linux:</strong> uses freedesktop sound names (e.g. <code>message-new-instant</code>). Support varies by desktop environment.</p>
      <p>You can also change the sound from the <a href="/dashboard">web dashboard</a> &mdash; open the sidebar and use the Notification Sound dropdown. Changes are saved to the workspace and synced to team members.</p>
    </section>

    <section class="docs-section" id="watch-mode">
      <h3>Watch Mode</h3>
      <p>Run the notification daemon manually without the TUI:</p>
      <div class="copy-wrap">
        <pre><code><span class="kw">$</span> dread watch</code></pre>
        <button class="copy-btn" onclick="copyText('dread watch', this)" type="button"><i data-lucide="copy"></i></button>
      </div>
      <p>Watch mode is headless — it connects to the server, listens for events, and fires desktop notifications. If the connection drops, it auto-reconnects after 3 seconds.</p>
      <p>This is the same process the background service runs. You can use it directly for debugging or if you prefer to manage the process yourself.</p>
    </section>

    <section class="docs-section" id="filtering">
      <h3>Filtering Events</h3>
      <p>Use the <code>--filter</code> flag to only see events matching a pattern. The filter is a case-insensitive substring match against source, type, summary, and channel name.</p>
      <div class="copy-wrap">
        <pre><code><span class="c"># only show Stripe events in the TUI</span>
<span class="kw">$</span> dread --filter stripe

<span class="c"># only get notifications for payment events</span>
<span class="kw">$</span> dread watch --filter payment

<span class="c"># filter for a specific channel</span>
<span class="kw">$</span> dread --filter "GitHub Deploys"</code></pre>
        <button class="copy-btn" onclick="copyText('dread --filter stripe', this)" type="button"><i data-lucide="copy"></i></button>
      </div>
      <p>In the TUI, press <code>/</code> to open the filter prompt interactively.</p>
    </section>

    <!-- FORWARDING & REPLAY -->
    <section class="docs-section" id="forward">
      <h2>Forwarding &amp; Replay</h2>
      <h3>Forward to Localhost</h3>
      <p>Forward webhook events to a local development server in real time:</p>
      <div class="copy-wrap">
        <pre><code><span class="kw">$</span> dread --forward http://localhost:3000/webhook</code></pre>
        <button class="copy-btn" onclick="copyText('dread --forward http://localhost:3000/webhook', this)" type="button"><i data-lucide="copy"></i></button>
      </div>
      <p>Every event that arrives is POSTed to the target URL with the original payload. dread adds the following headers for context:</p>
      <table>
        <tr><th>Header</th><th>Description</th></tr>
        <tr><td><code>X-Dread-Source</code></td><td>Detected source (stripe, github, etc.)</td></tr>
        <tr><td><code>X-Dread-Event-Type</code></td><td>Event type (invoice.paid, push, etc.)</td></tr>
        <tr><td><code>X-Dread-Channel</code></td><td>Channel ID</td></tr>
        <tr><td><code>X-Dread-Event-Id</code></td><td>Unique event ID</td></tr>
      </table>
      <p>Forwarding uses a 10-second timeout per request. The TUI continues to work normally while forwarding.</p>
    </section>

    <section class="docs-section" id="replay">
      <h3>Replay Past Events</h3>
      <p>Re-forward any past event to a URL. Useful for debugging webhook handlers without waiting for the real event to happen again.</p>
      <div class="copy-wrap">
        <pre><code><span class="kw">$</span> dread replay &lt;event-id&gt; --forward &lt;url&gt;</code></pre>
        <button class="copy-btn" onclick="copyText('dread replay', this)" type="button"><i data-lucide="copy"></i></button>
      </div>
      <p><strong>Example:</strong></p>
      <div class="copy-wrap">
        <pre><code><span class="kw">$</span> dread replay evt_abc123 --forward http://localhost:3000/webhook

<span class="o">Replayed evt_abc123 → http://localhost:3000/webhook (200 OK)</span></code></pre>
        <button class="copy-btn" onclick="copyText('dread replay evt_abc123 --forward http://localhost:3000/webhook', this)" type="button"><i data-lucide="copy"></i></button>
      </div>
      <p>The event is fetched from the server and POSTed to the target with the same <code>X-Dread-*</code> headers. You can find event IDs in the TUI detail view or from <code>dread logs</code>.</p>
    </section>

    <!-- WEB DASHBOARD -->
    <section class="docs-section" id="dashboard-overview">
      <h2>Web Dashboard</h2>
      <h3>Overview</h3>
      <p>The web dashboard at <a href="/dashboard">/dashboard</a> lets you view your live event feed in the browser without installing the CLI. It uses the same APIs as the CLI &mdash; no extra backend required.</p>
      <p>The workspace ID in the URL is the access key, same as the rest of the app. Anyone with the workspace ID can view the dashboard.</p>
    </section>

    <section class="docs-section" id="dashboard-connect">
      <h3>Connecting</h3>
      <p>Visit <a href="/dashboard">/dashboard</a> and enter your workspace ID (e.g. <code>ws_230a2bc06cb0</code>). Find your workspace ID by running:</p>
      <div class="copy-wrap">
        <pre><code><span class="kw">$</span> dread share</code></pre>
        <button class="copy-btn" onclick="copyText('dread share', this)" type="button"><i data-lucide="copy"></i></button>
      </div>
      <p>The dashboard remembers your last workspace ID in localStorage. You can also share direct links:</p>
      <div class="copy-wrap">
        <pre><code>https://dread.sh/dashboard?ws=ws_230a2bc06cb0</code></pre>
        <button class="copy-btn" onclick="copyText('https://dread.sh/dashboard?ws=ws_230a2bc06cb0', this)" type="button"><i data-lucide="copy"></i></button>
      </div>
    </section>

    <section class="docs-section" id="dashboard-features">
      <h3>Features</h3>
      <ul>
        <li><strong>Channel sidebar</strong> &mdash; lists all channels with their webhook URLs and copy buttons</li>
        <li><strong>Live event feed</strong> &mdash; events stream in real-time via WebSocket, with channel name, source, type, and summary</li>
        <li><strong>JSON payload viewer</strong> &mdash; click any event row to expand and see the full payload with syntax highlighting</li>
        <li><strong>Filter</strong> &mdash; search events by source, type, channel, or summary text</li>
        <li><strong>Pause/resume</strong> &mdash; pause the live stream to inspect events; buffered events flush on resume</li>
        <li><strong>Load more</strong> &mdash; scroll to the bottom and load older events with pagination</li>
        <li><strong>Tab notifications</strong> &mdash; unread event count appears in the browser tab title when the tab is in the background</li>
        <li><strong>Theme toggle</strong> &mdash; dark/light theme, same as the rest of the site</li>
        <li><strong>Mobile responsive</strong> &mdash; sidebar collapses to a hamburger menu on small screens</li>
        <li><strong>Mute toggle</strong> &mdash; per-channel mute via localStorage, suppresses browser notifications</li>
        <li><strong>Export</strong> &mdash; download events as JSON or CSV from the toolbar, or export as styled HTML</li>
        <li><strong>Replay</strong> &mdash; re-send any event to a URL from the event detail view</li>
        <li><strong>Bookmarks</strong> &mdash; star/bookmark events with ★, toggle bookmark-only view to filter to important events</li>
        <li><strong>Advanced filtering</strong> &mdash; <code>source:stripe</code>, <code>type:checkout</code>, <code>!error</code> exclusion, and free-text search</li>
        <li><strong>Stats panel</strong> &mdash; toggle source breakdown bars, success/failure/neutral status chart, and per-source swimlane timeline</li>
        <li><strong>Diff view</strong> &mdash; compare any event with the previous event from the same source, showing added/removed lines</li>
        <li><strong>Keyboard shortcuts</strong> &mdash; <code>j</code>/<code>k</code> navigate, <code>/</code> filter, <code>f</code> bookmark, <code>d</code> diff, <code>s</code> stats, <code>?</code> help overlay</li>
        <li><strong>HTML export</strong> &mdash; download the current session as a styled HTML report with collapsible payloads</li>
      </ul>
    </section>

    <!-- MUTING -->
    <section class="docs-section" id="muting">
      <h2>Muting Channels</h2>
      <p>Temporarily silence a noisy channel without unsubscribing:</p>
      <pre><code>dread mute ch_noisy_abc123
dread unmute ch_noisy_abc123</code></pre>
      <p>Muted channels continue receiving events server-side but won't trigger desktop notifications in watch mode or the TUI. The dashboard also offers a per-channel mute toggle saved in localStorage.</p>
    </section>

    <!-- ALERT RULES -->
    <section class="docs-section" id="alert-rules">
      <h2>Alert Rules</h2>
      <p>Set threshold alerts to fire when a pattern matches too many events in a time window:</p>
      <pre><code># Alert when 5+ sentry events in 10 minutes
dread alert add sentry 5 10

# List configured rules
dread alert list

# Remove a rule by index
dread alert remove 0</code></pre>
      <p>When the threshold is reached, you'll get a desktop notification. If Slack/Discord forwarding is configured, the alert is forwarded there too. Counters are in-memory and reset on restart.</p>
    </section>

    <!-- SLACK / DISCORD FORWARDING -->
    <section class="docs-section" id="slack-discord-fwd">
      <h2>Slack / Discord Forwarding</h2>
      <p>Forward webhook events to Slack or Discord in real time via <code>dread watch</code>:</p>
      <pre><code>dread watch --slack https://hooks.slack.com/services/T.../B.../xxx
dread watch --discord https://discord.com/api/webhooks/123/abc</code></pre>
      <p>Or set the URLs in <code>~/.config/dread/config.json</code>:</p>
      <pre><code>{
  "slack_url": "https://hooks.slack.com/services/...",
  "discord_url": "https://discord.com/api/webhooks/..."
}</code></pre>
      <p>Forwarding runs client-side in your <code>dread watch</code> process. Events are sent as rich messages &mdash; Slack uses blocks, Discord uses embeds.</p>
    </section>

    <!-- EXPORT -->
    <section class="docs-section" id="export">
      <h2>Export Events</h2>
      <p>Download events as JSON or CSV via the API (capped at 1000 per request):</p>
      <pre><code>curl "https://dread.sh/api/export?channels=ch_xxx&amp;format=csv" -o events.csv
curl "https://dread.sh/api/export?channels=ch_xxx&amp;format=json" -o events.json</code></pre>
      <p>The dashboard also includes an Export button in the toolbar.</p>
    </section>

    <!-- DIGEST -->
    <section class="docs-section" id="digest">
      <h2>Daily Digest</h2>
      <p>Get a summary of recent event activity:</p>
      <pre><code>dread digest            # last 24 hours
dread digest --hours 8  # last 8 hours</code></pre>
      <p>Shows total event count, breakdown by source, and the 10 most recent events. Also available via API: <code>GET /api/digest?channels=ch_xxx&amp;hours=24</code></p>
    </section>

    <!-- STATUS PAGE -->
    <section class="docs-section" id="status-page-doc">
      <h2>Status Page</h2>
      <p>Every workspace gets a public status page showing channel freshness:</p>
      <pre><code>https://dread.sh/status/ws_abc123def456</code></pre>
      <p>Channels are colour-coded by time since last event: green (&lt;5min), yellow (&lt;30min), red (&gt;30min), grey (no events). The page auto-refreshes every 30 seconds.</p>
    </section>

  </main>
</div>

<script>
lucide.createIcons();

/* ---- THEME ---- */
function toggleTheme() {
  var root = document.documentElement;
  var icon = document.getElementById('theme-icon');
  if (root.classList.contains('light')) {
    root.classList.remove('light');
    localStorage.setItem('theme', 'dark');
    icon.setAttribute('data-lucide', 'moon');
  } else {
    root.classList.add('light');
    localStorage.setItem('theme', 'light');
    icon.setAttribute('data-lucide', 'sun');
  }
  lucide.createIcons({attrs:{class:'lucide'},nameAttr:'data-lucide'});
}

(function() {
  var saved = localStorage.getItem('theme');
  if (saved === 'light') {
    document.documentElement.classList.add('light');
    var icon = document.getElementById('theme-icon');
    if (icon) icon.setAttribute('data-lucide', 'sun');
    lucide.createIcons({attrs:{class:'lucide'},nameAttr:'data-lucide'});
  }

})();

/* ---- MOBILE MENU ---- */
(function() {
  var btn = document.getElementById('menu-btn');
  var sidebar = document.getElementById('docs-sidebar');
  var overlay = document.getElementById('docs-overlay');
  function toggle() {
    sidebar.classList.toggle('open');
    overlay.classList.toggle('open');
  }
  btn.addEventListener('click', toggle);
  overlay.addEventListener('click', toggle);
})();

/* ---- SMOOTH SCROLL + CLOSE MOBILE MENU ---- */
(function() {
  var sidebar = document.getElementById('docs-sidebar');
  var overlay = document.getElementById('docs-overlay');
  var links = sidebar.querySelectorAll('a[href^="#"]');
  links.forEach(function(link) {
    link.addEventListener('click', function(e) {
      e.preventDefault();
      var target = document.querySelector(this.getAttribute('href'));
      if (target) {
        target.scrollIntoView({ behavior: 'smooth' });
      }
      if (sidebar.classList.contains('open')) {
        sidebar.classList.remove('open');
        overlay.classList.remove('open');
      }
    });
  });
})();

/* ---- SCROLL SPY ---- */
(function() {
  var sections = document.querySelectorAll('.docs-section');
  var links = document.querySelectorAll('.docs-sidebar a[href^="#"]');
  var linkMap = {};
  links.forEach(function(l) { linkMap[l.getAttribute('href').slice(1)] = l; });

  var observer = new IntersectionObserver(function(entries) {
    entries.forEach(function(entry) {
      if (entry.isIntersecting) {
        links.forEach(function(l) { l.classList.remove('active'); });
        var link = linkMap[entry.target.id];
        if (link) link.classList.add('active');
      }
    });
  }, { rootMargin: '-80px 0px -60% 0px', threshold: 0 });

  sections.forEach(function(s) { observer.observe(s); });
})();

/* ---- COPY BUTTONS ---- */
function copyText(text, el) {
  navigator.clipboard.writeText(text).then(function() {
    var btn = el.classList.contains('copy-btn') ? el : el.querySelector('.copy-btn');
    if (!btn) return;
    btn.classList.add('copied');
    var svg = btn.querySelector('svg');
    if (svg) svg.setAttribute('data-lucide', 'check');
    lucide.createIcons({attrs:{class:'lucide'},nameAttr:'data-lucide'});
    setTimeout(function() {
      btn.classList.remove('copied');
      if (svg) svg.setAttribute('data-lucide', 'copy');
      lucide.createIcons({attrs:{class:'lucide'},nameAttr:'data-lucide'});
    }, 1500);
  });
}
</script>
</body>
</html>`

const changelogPage = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="description" content="dread.sh changelog — version history, new features, bug fixes, and release notes.">
<link rel="canonical" href="https://dread.sh/changelog">
<meta property="og:type" content="website">
<meta property="og:site_name" content="dread.sh">
<meta property="og:title" content="Changelog - dread.sh Release Notes">
<meta property="og:description" content="Version history, new features, bug fixes, and release notes for dread.">
<meta property="og:url" content="https://dread.sh/changelog">
<meta property="og:image" content="https://dread.sh/og.png">
<meta name="twitter:card" content="summary_large_image">
<meta name="twitter:title" content="Changelog - dread.sh Release Notes">
<meta name="twitter:description" content="Version history, new features, bug fixes, and release notes for dread.">
<meta name="twitter:image" content="https://dread.sh/og.png">
<script>if(localStorage.getItem('theme')==='light')document.documentElement.classList.add('light')</script>
<link rel="icon" href="data:image/svg+xml,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 100 100'><circle cx='50' cy='50' r='40' fill='%23c37960'/></svg>">
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Press+Start+2P&display=swap">
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/geist@1.3.1/dist/fonts/geist-sans/style.min.css">
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/geist@1.3.1/dist/fonts/geist-mono/style.min.css">
<title>Changelog - dread.sh Release Notes</title>
<script src="https://unpkg.com/lucide@0.469.0/dist/umd/lucide.min.js"></script>
<style>
  :root {
    --bg: oklch(10% 0.003 256);
    --surface: oklch(16% 0.003 256);
    --surface-hover: oklch(20% 0.003 256);
    --border: oklch(23% 0.003 256);
    --border-subtle: oklch(18% 0.003 256);
    --text: oklch(98.5% 0.003 256);
    --text-secondary: oklch(70.5% 0.003 256);
    --text-muted: oklch(55.2% 0.003 256);
    --text-dim: oklch(40% 0.003 256);
    --accent: oklch(65% 0.1 40);
    --accent-dim: oklch(47% 0.09 36);
    --accent-glow: oklch(55% 0.1 38 / 0.15);
    --accent-glow-strong: oklch(55% 0.1 38 / 0.3);
    --nav-bg: oklch(10% 0.003 256 / 0.85);
  }

  :root.light {
    --bg: oklch(98% 0.003 256);
    --surface: oklch(97% 0.003 256);
    --surface-hover: oklch(94% 0.003 256);
    --border: oklch(85% 0.003 256);
    --border-subtle: oklch(90% 0.003 256);
    --text: oklch(15% 0.003 256);
    --text-secondary: oklch(35% 0.003 256);
    --text-muted: oklch(50% 0.003 256);
    --text-dim: oklch(65% 0.003 256);
    --accent: oklch(50% 0.12 40);
    --accent-dim: oklch(40% 0.1 36);
    --accent-glow: oklch(50% 0.12 40 / 0.1);
    --accent-glow-strong: oklch(50% 0.12 40 / 0.2);
    --nav-bg: oklch(98% 0.003 256 / 0.85);
  }

  * { margin: 0; padding: 0; box-sizing: border-box; }
  html, body { overscroll-behavior: none; }
  html { font-size: 18px; }
  body {
    font-family: "Geist", -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
    background: var(--bg);
    color: var(--text-secondary);
    line-height: 1.6;
    -webkit-font-smoothing: antialiased;
  }
  code, pre, kbd {
    font-family: "Geist Mono", ui-monospace, "Cascadia Code", "Source Code Pro", Menlo, Consolas, monospace;
  }

  /*! NAV_CSS */

  .changelog {
    max-width: 720px; margin: 0 auto;
    padding: 64px 24px 120px;
  }
  .changelog h1 {
    font-size: 2.2rem; font-weight: 700; color: var(--text);
    letter-spacing: -0.03em; margin-bottom: 8px;
  }
  .changelog .subtitle {
    color: var(--text-muted); font-size: 1rem; margin-bottom: 56px;
  }
  .changelog-entry {
    padding-bottom: 48px; margin-bottom: 48px;
    border-bottom: 1px solid var(--border-subtle);
  }
  .changelog-entry:last-child { border-bottom: none; margin-bottom: 0; }
  .changelog-date {
    font-size: 0.8rem; color: var(--accent); font-weight: 500;
    margin-bottom: 8px; letter-spacing: 0.02em;
  }
  .changelog-title {
    font-size: 1.35rem; font-weight: 600; color: var(--text);
    margin-bottom: 16px; letter-spacing: -0.02em;
  }
  .changelog-entry ul {
    list-style: none; padding: 0;
  }
  .changelog-entry li {
    position: relative; padding-left: 20px;
    margin-bottom: 8px; color: var(--text-secondary);
    font-size: 0.95rem;
  }
  .changelog-entry a { color: var(--violet); text-decoration: none; }
  .changelog-entry a:hover { text-decoration: underline; }
  .changelog-entry li::before {
    content: ""; position: absolute; left: 0; top: 10px;
    width: 6px; height: 6px; border-radius: 50%;
    background: var(--text-dim);
  }
</style>
</head>
<body>

<!-- NAV_HTML -->

<div class="changelog">
  <h1>Changelog</h1>
  <p class="subtitle">New updates and improvements to dread.sh</p>

  <div class="changelog-entry">
    <div class="changelog-date">March 6, 2026</div>
    <div class="changelog-title">10 more TUI features: bookmarks, diff, grouping, palette, export, swimlanes, channels tab</div>
    <ul>
      <li><strong>Star/bookmark events</strong> &mdash; press <code>f</code> to bookmark any event, <code>F</code> to toggle bookmark-only view. Bookmarked events show a ★ indicator</li>
      <li><strong>Auto-diff consecutive events</strong> &mdash; press <code>d</code> in detail view to see a line-by-line diff with the previous event from the same source</li>
      <li><strong>Event grouping / burst collapsing</strong> &mdash; press <code>g</code> to toggle grouping. Consecutive events with the same source+type within 60s are collapsed with a &times;N badge</li>
      <li><strong>Command palette</strong> &mdash; press <code>Ctrl+P</code> for a fuzzy-searchable command palette with all TUI actions</li>
      <li><strong>Mouse support</strong> &mdash; click events to select them, scroll to navigate. Mouse events are enabled via bubbletea cell motion mode</li>
      <li><strong>Session export as HTML</strong> &mdash; press <code>x</code> to export the current session as a styled HTML report with collapsible payloads</li>
      <li><strong>Forward response capture</strong> &mdash; forwarded events now show a &rarr;200 status badge in the event list, with full response details (status, headers, body, duration) in the detail view</li>
      <li><strong>Swimlane timeline</strong> &mdash; the Stats tab now includes a per-source swimlane showing event activity over the last 60 minutes</li>
      <li><strong>Bookmark count in header</strong> &mdash; the status line now shows ★ N when events are bookmarked</li>
    </ul>
  </div>

  <div class="changelog-entry">
    <div class="changelog-date">March 6, 2026</div>
    <div class="changelog-title">10 new TUI features: split pane, tabs, pause, help, toasts, heatmap</div>
    <ul>
      <li><strong>Master-detail split pane</strong> &mdash; press <code>s</code> to toggle a side-by-side view with event list on the left and live payload viewer on the right</li>
      <li><strong><code>?</code> help overlay</strong> &mdash; press <code>?</code> for a complete keybinding reference, grouped by category</li>
      <li><strong>Pause/resume feed</strong> &mdash; press <code>p</code> or <code>Space</code> to pause the live stream. Events buffer in the background with a counter showing how many are waiting</li>
      <li><strong>Tabbed views</strong> &mdash; <code>1</code> Live, <code>2</code> Errors (auto-filtered to failures), <code>3</code> Stats with bar charts and heatmap</li>
      <li><strong>Advanced filtering</strong> &mdash; <code>source:stripe</code> field-specific filters, <code>!error</code> exclusion, deep payload search, and filter history with ↑↓ arrows</li>
      <li><strong>Per-source sparklines</strong> &mdash; top 3 sources each get their own sparkline in the header for at-a-glance source comparison</li>
      <li><strong>In-TUI toast notifications</strong> &mdash; failure events trigger a red toast above the footer that auto-dismisses after 5 seconds</li>
      <li><strong>Activity heatmap</strong> &mdash; 7-day &times; 24-hour grid in the Stats tab showing event density with heat-mapped colours</li>
      <li><strong>Stats tab</strong> &mdash; bar charts for events by source and success/failure/neutral breakdown with percentages</li>
      <li><strong>Copy payload</strong> &mdash; press <code>c</code> in detail view to copy the full JSON payload to clipboard</li>
    </ul>
  </div>

  <div class="changelog-entry">
    <div class="changelog-date">March 6, 2026</div>
    <div class="changelog-title">DREAD ASCII logo, rich TUI header, event status indicators</div>
    <ul>
      <li><strong>DREAD ASCII art logo</strong> &mdash; the TUI now opens with a bold figlet-style DREAD banner in the brand brown colour, inspired by Claude Code's welcome screen</li>
      <li><strong>3-column TUI header</strong> &mdash; logo &vert; status info &vert; activity stats, wrapped in a rounded box with padding</li>
      <li><strong>Greeting &amp; session timer</strong> &mdash; time-of-day greeting and live session uptime in the header</li>
      <li><strong>Channel health dots</strong> &mdash; green/gray dots per channel showing which channels received events recently (30-min stale threshold)</li>
      <li><strong>Event sparkline</strong> &mdash; Unicode block-character sparkline showing event rate over the last hour in 5-minute buckets</li>
      <li><strong>Success / failure classification</strong> &mdash; events are classified as success (green &#9679;), failure (red &#9679;), or neutral (gray &#9679;) based on keyword matching on type and summary</li>
      <li><strong>Event status dots in TUI</strong> &mdash; each event row in the TUI list shows a coloured dot indicating success/failure/neutral</li>
      <li><strong>Event status dots in dashboard</strong> &mdash; the web dashboard now shows the same coloured status dots on each event row</li>
      <li><strong>Rotating command tips</strong> &mdash; 12 tips cycle every 5 seconds in the header, showing useful dread commands and keybindings</li>
      <li><strong>Update notifications</strong> &mdash; the TUI checks <code>/api/version</code> on startup and shows a notice when a newer version is available</li>
      <li><strong>Press Start 2P branding</strong> &mdash; website nav and footer logo now use the Press Start 2P pixel font at 1.15rem</li>
    </ul>
  </div>

  <div class="changelog-entry">
    <div class="changelog-date">March 6, 2026</div>
    <div class="changelog-title">URL-based source labelling, background service, 50+ How To guides</div>
    <ul>
      <li><strong><code>?source=name</code> URL parameter</strong> &mdash; append to any webhook URL to label events from services that aren't auto-detected (e.g. <code>?source=trigger.dev</code>). No custom headers needed</li>
      <li><strong>trigger.dev</strong> &mdash; dedicated summariser and colour in both dashboard and TUI</li>
      <li><strong>Per-source dashboard colours</strong> &mdash; 30+ named source colours using OKLCH, with hash-based fallback for unknown sources</li>
      <li><strong>Full-width dashboard</strong> &mdash; removed max-width constraint for better use of screen space</li>
      <li><strong><code>dread service install</code></strong> &mdash; installs a background service so notifications continue even after the terminal is closed</li>
      <li><strong><code>dread service uninstall</code></strong> &mdash; stops and removes the background service</li>
      <li><strong>macOS</strong> &mdash; uses <code>launchd</code> with auto-restart and login start. Logs to <code>~/Library/Logs/dread.log</code></li>
      <li><strong>Linux</strong> &mdash; uses <code>systemd</code> user service. Logs via <code>journalctl</code></li>
      <li><code>dread status</code> shows whether the background service is running</li>
      <li><strong>50+ How To guides</strong> &mdash; 10 per category: Payments, Developer Tools, Infrastructure, Communication, SaaS</li>
    </ul>
  </div>

  <div class="changelog-entry">
    <div class="changelog-date">March 3, 2026</div>
    <div class="changelog-title">108 webhook sources, 47 summarisers, TUI fix</div>
    <ul>
      <li><strong>108 auto-detected sources</strong> &mdash; added 60+ new webhook sources including Pipedrive, Asana, Webflow, Klaviyo, Cal.com, Monday, Chargebee, ActiveCampaign, BambooHR, Smartsheet, Hootsuite, Dropbox, Box, Help Scout, and many more</li>
      <li><strong>47 payload summarisers</strong> &mdash; 30 new summariser functions for richer event descriptions from Pipedrive, Asana, Webflow, Klaviyo, Squarespace, Ecwid, Box, Help Scout, Smartsheet, Cal.com, Monday, Chargebee, ActiveCampaign, Basecamp, and more</li>
      <li><strong>TUI duplicate event fix</strong> &mdash; fixed a bug where WebSocket reconnects caused duplicate events to pile up in the TUI (e.g. 16 real events showing as 122)</li>
      <li><strong>Default sound changed to Sosumi</strong></li>
    </ul>
  </div>

  <div class="changelog-entry">
    <div class="changelog-date">March 3, 2026</div>
    <div class="changelog-title">10 New Features</div>
    <ul>
      <li><strong>Health endpoint</strong> &mdash; <code>GET /health</code> returns server status, uptime, and event count</li>
      <li><strong>10 new payload parsers</strong> &mdash; rich summaries for Vercel, Sentry, PagerDuty, Jira, GitLab, PayPal, AWS SNS, Twitch, HubSpot, and Typeform</li>
      <li><strong>Event retention</strong> &mdash; automatic cleanup of events older than <code>DREAD_RETENTION_DAYS</code> (default 30)</li>
      <li><strong>Channel muting</strong> &mdash; <code>dread mute/unmute</code> to silence channels without unsubscribing; also in TUI and dashboard (localStorage)</li>
      <li><strong>Slack/Discord forwarding</strong> &mdash; <code>dread watch --slack</code> / <code>--discord</code> to forward events as rich messages</li>
      <li><strong>Event export</strong> &mdash; <code>GET /api/export</code> downloads events as JSON or CSV (capped at 1000); dashboard export button</li>
      <li><strong>Webhook replay</strong> &mdash; <code>POST /api/replay</code> re-sends any event to a URL; replay button in the dashboard</li>
      <li><strong>Daily digest</strong> &mdash; <code>dread digest</code> and <code>GET /api/digest</code> for event summaries by source</li>
      <li><strong>Threshold alerts</strong> &mdash; <code>dread alert add</code> fires notification + Slack/Discord when a pattern exceeds N events in M minutes</li>
      <li><strong>Status page</strong> &mdash; <code>/status/ws_xxx</code> shows live channel freshness with colour-coded cards and 30s auto-refresh</li>
      <li><strong><a href="/howto">How To guide</a></strong> &mdash; step-by-step setup for 50+ services, team features, alerts, export, and more</li>
    </ul>
  </div>

  <div class="changelog-entry">
    <div class="changelog-date">March 3, 2026</div>
    <div class="changelog-title">60+ auto-detected webhook sources</div>
    <ul>
      <li>Auto-detects webhook sources from HTTP headers — no configuration needed</li>
      <li>Payment: Stripe, PayPal, Square, Razorpay, Paddle, Recurly, Coinbase, Plaid, Xero, QuickBooks</li>
      <li>Dev: GitHub, GitLab, Bitbucket, CircleCI, Travis CI, Buildkite</li>
      <li>Infrastructure: Vercel, Heroku, AWS SNS, Cloudflare</li>
      <li>Communication: Slack, Discord, Twilio, SendGrid, Mailchimp, Zendesk, Telegram, LINE</li>
      <li>Project management: Linear, Jira, Notion, Trello, Airtable</li>
      <li>Monitoring: Sentry, PagerDuty, Grafana, Pingdom</li>
      <li>Commerce: Shopify, WooCommerce, Contentful, Sanity, BigCommerce</li>
      <li>Auth: Auth0, WorkOS, Svix (Clerk, Resend)</li>
      <li>Database: Supabase, PlanetScale</li>
      <li>SaaS: HubSpot, Typeform, Calendly, DocuSign, Zoom, Figma, Twitch, LaunchDarkly, and more</li>
      <li>User-Agent fallback detection for Zapier, Pingdom, WooCommerce, and others</li>
      <li>Custom sources via <code>?source=name</code> URL parameter or <code>X-Dread-Source</code> header</li>
    </ul>
  </div>

  <div class="changelog-entry">
    <div class="changelog-date">March 3, 2026</div>
    <div class="changelog-title">Custom notification sounds</div>
    <ul>
      <li>Notification sound is now configurable — set <code>"sound"</code> in <code>~/.config/dread/config.json</code></li>
      <li>Default sound changed from Funk to Sosumi</li>
      <li>macOS: any system sound name (Glass, Ping, Pop, Hero, Submarine, etc.)</li>
      <li>Linux: freedesktop sound names via <code>notify-send</code></li>
      <li>Custom sounds on macOS: drop a <code>.aiff</code> file in <code>~/Library/Sounds/</code></li>
      <li>Sound is also configurable from the <a href="/dashboard">web dashboard</a> sidebar</li>
    </ul>
  </div>

  <div class="changelog-entry">
    <div class="changelog-date">March 3, 2026</div>
    <div class="changelog-title">Web dashboard</div>
    <ul>
      <li>Added browser-based dashboard at <a href="/dashboard">dread.sh/dashboard</a> — view live events without installing the CLI</li>
      <li>Enter your workspace ID to see channels, webhook URLs, and a real-time event feed</li>
      <li>Click any event row to expand and see the full JSON payload with syntax highlighting</li>
      <li>Filter events by source, type, channel, or summary</li>
      <li>Pause/resume the live stream to inspect events without new ones pushing the list</li>
      <li>Deep linking support — share <code>/dashboard?ws=your_id</code> URLs with your team</li>
      <li>Unread event count shown in the browser tab title when the tab is in the background</li>
      <li>Load older events with pagination</li>
      <li>Live relative timestamps that update automatically</li>
      <li>Channel name shown on each event row for multi-channel workspaces</li>
      <li>Mobile responsive with collapsible sidebar</li>
    </ul>
  </div>

  <div class="changelog-entry">
    <div class="changelog-date">March 3, 2026</div>
    <div class="changelog-title">Changelog, GitHub stars, and install improvements</div>
    <ul>
      <li>Added changelog page at dread.sh/changelog</li>
      <li>Added GitHub star button to navigation and GitHub link to footer</li>
      <li>Install script now uses <code>~/.local/bin</code> — no sudo required</li>
      <li>Installer automatically adds <code>~/.local/bin</code> to your shell PATH</li>
      <li>Re-running the installer updates to the latest version</li>
      <li>Updated README with install instructions, CLI reference, and project links</li>
      <li>Updated documentation with new install details</li>
    </ul>
  </div>

  <div class="changelog-entry">
    <div class="changelog-date">March 3, 2026</div>
    <div class="changelog-title">Landing page redesign</div>
    <ul>
      <li>Redesigned landing page with live terminal preview and use cases section</li>
      <li>Added value proposition section highlighting key benefits</li>
      <li>Improved typography with Geist Sans and Geist Mono fonts</li>
      <li>Added copy-to-clipboard buttons on all code blocks</li>
      <li>Added Lucide icons across feature grid and flow cards</li>
    </ul>
  </div>

  <div class="changelog-entry">
    <div class="changelog-date">March 3, 2026</div>
    <div class="changelog-title">Documentation site</div>
    <ul>
      <li>Added full documentation page with sidebar navigation</li>
      <li>Covers CLI reference, webhook setup, team workspaces, and notifications</li>
    </ul>
  </div>

  <div class="changelog-entry">
    <div class="changelog-date">March 2, 2026</div>
    <div class="changelog-title">Team workspaces</div>
    <ul>
      <li>Added workspace follow model for sharing webhook feeds across a team</li>
      <li>New team commands for managing shared workspaces</li>
      <li>Install download tracking</li>
    </ul>
  </div>

  <div class="changelog-entry">
    <div class="changelog-date">March 2, 2026</div>
    <div class="changelog-title">Background notifications and installer</div>
    <ul>
      <li>Added dread watch for headless background desktop notifications</li>
      <li>Auto-setup background notifications on install</li>
      <li>Added curl installer at dread.sh/install</li>
      <li>Config auto-reloads on reconnect so new channels are picked up automatically</li>
    </ul>
  </div>

  <div class="changelog-entry">
    <div class="changelog-date">March 2, 2026</div>
    <div class="changelog-title">Initial release</div>
    <ul>
      <li>Go server with real-time webhook event streaming</li>
      <li>CLI with interactive TUI for monitoring events</li>
      <li>Desktop notifications for incoming webhooks</li>
      <li>Support for Stripe, GitHub, Sentry, and custom webhook sources</li>
      <li>Fly.io deployment with auto-deploy on push</li>
      <li>Homebrew tap for easy installation</li>
    </ul>
  </div>
</div>

<script>
function toggleTheme() {
  var root = document.documentElement;
  var isLight = root.classList.toggle('light');
  localStorage.setItem('theme', isLight ? 'light' : 'dark');
  var icon = document.getElementById('theme-icon');
  if (icon) {
    icon.setAttribute('data-lucide', isLight ? 'sun' : 'moon');
    lucide.createIcons({attrs:{class:'lucide'},nameAttr:'data-lucide'});
  }
}
(function() {
  if (localStorage.getItem('theme') === 'light') {
    document.documentElement.classList.add('light');
    var i = document.getElementById('theme-icon');
    if (i) i.setAttribute('data-lucide', 'sun');
  }
  lucide.createIcons({attrs:{class:'lucide'},nameAttr:'data-lucide'});

})();
</script>
</body>
</html>`

const dashboardPage = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<script>if(localStorage.getItem('theme')==='light')document.documentElement.classList.add('light')</script>
<link rel="icon" href="data:image/svg+xml,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 100 100'><circle cx='50' cy='50' r='40' fill='%23c37960'/></svg>">
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Press+Start+2P&display=swap">
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/geist@1.3.1/dist/fonts/geist-sans/style.min.css">
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/geist@1.3.1/dist/fonts/geist-mono/style.min.css">
<title>Live Webhook Dashboard - dread.sh</title>
<script src="https://unpkg.com/lucide@0.469.0/dist/umd/lucide.min.js"></script>
<style>
  :root {
    --bg: oklch(10% 0.003 256);
    --surface: oklch(16% 0.003 256);
    --surface-hover: oklch(20% 0.003 256);
    --border: oklch(23% 0.003 256);
    --border-subtle: oklch(18% 0.003 256);
    --text: oklch(98.5% 0.003 256);
    --text-secondary: oklch(70.5% 0.003 256);
    --text-muted: oklch(55.2% 0.003 256);
    --text-dim: oklch(40% 0.003 256);
    --accent: oklch(65% 0.1 40);
    --accent-dim: oklch(47% 0.09 36);
    --accent-glow: oklch(55% 0.1 38 / 0.15);
    --accent-glow-strong: oklch(55% 0.1 38 / 0.3);
    --orange: oklch(75% 0.18 55);
    --orange-dim: oklch(52% 0.16 55);
    --blue: oklch(70.7% 0.165 254.62);
    --violet: oklch(70.2% 0.183 293.54);
    --amber: oklch(82.8% 0.189 84.43);
    --rose: oklch(71.2% 0.194 13.43);
    --cyan: oklch(78.9% 0.154 211.53);
    --nav-bg: oklch(10% 0.003 256 / 0.85);
    --green: oklch(72% 0.17 142);
  }

  :root.light {
    --bg: oklch(98% 0.003 256);
    --surface: oklch(97% 0.003 256);
    --surface-hover: oklch(94% 0.003 256);
    --border: oklch(85% 0.003 256);
    --border-subtle: oklch(90% 0.003 256);
    --text: oklch(15% 0.003 256);
    --text-secondary: oklch(35% 0.003 256);
    --text-muted: oklch(50% 0.003 256);
    --text-dim: oklch(65% 0.003 256);
    --accent: oklch(50% 0.12 40);
    --accent-dim: oklch(40% 0.1 36);
    --accent-glow: oklch(50% 0.12 40 / 0.1);
    --accent-glow-strong: oklch(50% 0.12 40 / 0.2);
    --orange: oklch(55% 0.18 55);
    --orange-dim: oklch(45% 0.16 55);
    --blue: oklch(50% 0.165 254.62);
    --violet: oklch(50% 0.183 293.54);
    --amber: oklch(55% 0.189 84.43);
    --rose: oklch(55% 0.194 13.43);
    --cyan: oklch(50% 0.154 211.53);
    --nav-bg: oklch(98% 0.003 256 / 0.85);
    --green: oklch(45% 0.17 142);
  }

  * { margin: 0; padding: 0; box-sizing: border-box; }
  html, body { overscroll-behavior: none; }
  html { font-size: 18px; }

  body {
    font-family: "Geist", -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
    background: var(--bg);
    color: var(--text-secondary);
    line-height: 1.6;
    -webkit-font-smoothing: antialiased;
  }

  code, pre, kbd {
    font-family: "Geist Mono", ui-monospace, "Cascadia Code", "Source Code Pro", Menlo, Consolas, monospace;
  }

  /* NAV */
  /*! NAV_CSS */

  /* CONNECT SCREEN */
  .connect-screen {
    max-width: 440px; margin: 120px auto;
    padding: 0 24px; text-align: center;
  }
  .connect-screen h1 {
    font-size: 1.8rem; color: var(--text);
    font-weight: 600; letter-spacing: -0.02em;
    margin-bottom: 8px;
  }
  .connect-screen p {
    color: var(--text-muted); font-size: 0.9rem;
    margin-bottom: 32px;
  }
  .connect-form {
    display: flex; gap: 8px;
  }
  .connect-form input {
    flex: 1; padding: 12px 16px;
    background: var(--surface); border: 1px solid var(--border);
    border-radius: 8px; color: var(--text);
    font-family: "Geist Mono", monospace; font-size: 0.85rem;
    outline: none; transition: border-color 0.15s;
  }
  .connect-form input::placeholder { color: var(--text-dim); }
  .connect-form input:focus { border-color: var(--accent); }
  .connect-form button {
    padding: 12px 24px; background: var(--accent);
    border: none; border-radius: 8px; color: white;
    font-family: "Geist", sans-serif; font-size: 0.85rem;
    font-weight: 500; cursor: pointer; transition: opacity 0.15s;
    white-space: nowrap;
  }
  .connect-form button:hover { opacity: 0.9; }
  .connect-error {
    margin-top: 16px; color: var(--rose); font-size: 0.8rem;
    display: none;
  }

  /* DASHBOARD LAYOUT */
  .dashboard { display: none; }
  .dashboard.active { display: flex; }
  .dashboard {
    min-height: calc(100vh - 57px);
  }

  /* SIDEBAR */
  .sidebar {
    width: 300px; flex-shrink: 0;
    border-right: 1px solid var(--border);
    padding: 20px; overflow-y: auto;
    max-height: calc(100vh - 57px);
    position: sticky; top: 57px;
  }
  .sidebar-header {
    display: flex; align-items: center; justify-content: space-between;
    margin-bottom: 16px;
  }
  .sidebar-title {
    font-size: 0.75rem; text-transform: uppercase;
    letter-spacing: 0.1em; color: var(--text-muted);
  }
  .ws-id {
    font-size: 0.7rem; color: var(--accent);
    font-family: "Geist Mono", monospace;
  }
  .channel-list { list-style: none; }
  .channel-item {
    padding: 10px 12px; border-radius: 8px;
    margin-bottom: 4px; transition: background 0.15s;
  }
  .channel-item:hover { background: var(--surface); }
  .channel-name {
    font-size: 0.85rem; color: var(--text); font-weight: 500;
    margin-bottom: 4px;
  }
  .channel-url-row {
    display: flex; align-items: center; gap: 6px;
  }
  .channel-url {
    font-size: 0.7rem; color: var(--text-dim);
    font-family: "Geist Mono", monospace;
    overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
    flex: 1;
  }
  .copy-btn {
    background: none; border: none; cursor: pointer;
    color: var(--text-dim); padding: 2px;
    display: flex; align-items: center;
    transition: color 0.15s; flex-shrink: 0;
  }
  .copy-btn:hover { color: var(--text); }
  .copy-btn svg { width: 13px; height: 13px; }
  .copy-btn.copied { color: var(--green); }
  .disconnect-btn {
    width: 100%; margin-top: 16px; padding: 8px;
    background: none; border: 1px solid var(--border);
    border-radius: 8px; color: var(--text-muted);
    font-size: 0.8rem; cursor: pointer;
    font-family: "Geist", sans-serif;
    transition: border-color 0.15s, color 0.15s;
  }
  .disconnect-btn:hover { border-color: var(--rose); color: var(--rose); }

  /* MAIN AREA */
  .main-area {
    flex: 1; min-width: 0;
    display: flex; flex-direction: column;
  }

  /* TOOLBAR */
  .toolbar {
    padding: 12px 20px;
    border-bottom: 1px solid var(--border);
    display: flex; align-items: center; gap: 12px;
    position: sticky; top: 57px; z-index: 10;
    background: var(--bg);
  }
  .toolbar-status {
    display: flex; align-items: center; gap: 8px;
    font-size: 0.8rem; color: var(--text-muted);
  }
  .status-dot {
    width: 7px; height: 7px; border-radius: 50%;
    background: var(--text-dim);
  }
  .status-dot.connected { background: var(--green); }
  .health-indicator {
    display: flex; align-items: center; gap: 10px;
    font-size: 0.75rem; font-family: var(--mono);
  }
  .health-indicator .hi-success { color: var(--green); }
  .health-indicator .hi-failure { color: var(--rose); }
  .health-indicator .hi-neutral { color: var(--text-dim); }
  .filter-input {
    flex: 1; padding: 8px 12px;
    background: var(--surface); border: 1px solid var(--border);
    border-radius: 6px; color: var(--text);
    font-family: "Geist", sans-serif; font-size: 0.8rem;
    outline: none; transition: border-color 0.15s;
  }
  .filter-input::placeholder { color: var(--text-dim); }
  .filter-input:focus { border-color: var(--accent); }
  .event-count {
    font-size: 0.75rem; color: var(--text-dim);
    white-space: nowrap;
    font-family: "Geist Mono", monospace;
  }

  /* EVENT TABLE */
  .events-container {
    flex: 1; overflow-y: auto;
  }
  .events-table {
    width: 100%; border-collapse: collapse;
  }
  .events-table th {
    position: sticky; top: 0;
    background: var(--bg);
    padding: 10px 16px; text-align: left;
    font-size: 0.7rem; text-transform: uppercase;
    letter-spacing: 0.08em; color: var(--text-dim);
    font-weight: 500; border-bottom: 1px solid var(--border);
    white-space: nowrap;
  }
  .events-table td {
    padding: 10px 16px; border-bottom: 1px solid var(--border-subtle);
    font-size: 0.8rem; vertical-align: top;
  }
  .events-table tr { cursor: pointer; transition: background 0.1s; }
  .events-table tbody tr:hover { background: var(--surface); }
  .events-table tr.new-event { animation: flash 1s ease-out; }
  @keyframes flash {
    0% { background: var(--accent-glow); }
    100% { background: transparent; }
  }
  .col-time {
    white-space: nowrap; color: var(--text-dim);
    font-family: "Geist Mono", monospace; font-size: 0.75rem;
    width: 140px;
  }
  .col-source {
    font-weight: 500; width: 100px;
  }
  .col-type {
    color: var(--violet); width: 160px;
    font-family: "Geist Mono", monospace; font-size: 0.75rem;
  }
  .col-summary {
    color: var(--text-secondary);
    max-width: 0; overflow: hidden;
    text-overflow: ellipsis; white-space: nowrap;
  }

  /* EXPANDED ROW */
  .event-detail {
    display: none;
  }
  .event-detail.open { display: table-row; }
  .event-detail td {
    padding: 0 16px 16px; border-bottom: 1px solid var(--border);
    background: var(--surface);
  }
  .json-viewer-wrap { position: relative; }
  .json-viewer-wrap .copy-json {
    position: absolute; top: 8px; right: 8px;
    background: var(--surface); border: 1px solid var(--border);
    color: var(--text-muted); border-radius: 6px; padding: 4px 10px;
    font-size: 0.7rem; cursor: pointer; opacity: 0; transition: opacity 0.15s;
    font-family: "Geist", sans-serif;
  }
  .json-viewer-wrap:hover .copy-json { opacity: 1; }
  .json-viewer-wrap .copy-json:hover { color: var(--text-primary); border-color: var(--text-muted); }
  .json-viewer {
    background: var(--bg); border: 1px solid var(--border);
    border-radius: 8px; padding: 16px;
    overflow-x: auto; font-size: 0.75rem;
    font-family: "Geist Mono", monospace;
    line-height: 1.5; max-height: 400px;
    overflow-y: auto; white-space: pre-wrap;
    word-break: break-all;
  }
  .json-key { color: var(--cyan); }
  .json-string { color: var(--amber); }
  .json-number { color: var(--violet); }
  .json-bool { color: var(--rose); }
  .json-null { color: var(--text-dim); }

  /* EMPTY STATE */
  .empty-state {
    display: flex; flex-direction: column;
    align-items: center; justify-content: center;
    padding: 80px 24px; color: var(--text-dim);
    text-align: center;
  }
  .empty-state svg { width: 48px; height: 48px; margin-bottom: 16px; opacity: 0.3; }
  .empty-state p { font-size: 0.9rem; }

  /* MOBILE */
  @media (max-width: 768px) {
    .sidebar {
      display: none; position: fixed; top: 57px; left: 0;
      width: 280px; height: calc(100vh - 57px);
      z-index: 50; background: var(--bg);
      border-right: 1px solid var(--border);
    }
    .sidebar.open { display: block; }
    .mobile-sidebar-btn { display: flex !important; }
    .col-type { display: none; }
    .col-source { width: 80px; }
    .toolbar { flex-wrap: wrap; }
    .filter-input { min-width: 100%; order: 10; }
  }
  @media (min-width: 769px) {
    .mobile-sidebar-btn { display: none !important; }
    .sidebar-overlay { display: none !important; }
  }

  .mobile-sidebar-btn {
    display: none; background: none; border: none;
    color: var(--text-muted); cursor: pointer; padding: 4px;
  }
  .mobile-sidebar-btn svg { width: 18px; height: 18px; }
  .sidebar-overlay {
    display: none; position: fixed; inset: 0;
    background: oklch(0% 0 0 / 0.5); z-index: 49;
  }
  .sidebar-overlay.open { display: block; }

  /* LOAD MORE */
  .load-more {
    display: none; text-align: center; padding: 16px;
  }
  .load-more.active { display: block; }
  .load-more button {
    padding: 8px 20px; background: var(--surface);
    border: 1px solid var(--border); border-radius: 6px;
    color: var(--text-muted); font-size: 0.8rem; cursor: pointer;
    font-family: "Geist", sans-serif; transition: border-color 0.15s, color 0.15s;
  }
  .load-more button:hover { border-color: var(--text-muted); color: var(--text); }
  .load-more button:disabled { opacity: 0.5; cursor: default; }

  /* PAUSE BUTTON */
  .pause-btn {
    background: none; border: 1px solid var(--border);
    border-radius: 6px; cursor: pointer; padding: 5px 10px;
    color: var(--text-muted); display: flex; align-items: center; gap: 6px;
    font-size: 0.75rem; font-family: "Geist", sans-serif;
    transition: border-color 0.15s, color 0.15s; white-space: nowrap;
  }
  .pause-btn:hover { border-color: var(--text-muted); color: var(--text); }
  .pause-btn svg { width: 14px; height: 14px; }
  .pause-btn.paused { border-color: var(--amber); color: var(--amber); }
  .pause-badge {
    display: none; background: var(--amber); color: oklch(15% 0.003 256);
    font-size: 0.65rem; font-weight: 600; padding: 1px 5px;
    border-radius: 8px; margin-left: 2px;
  }
  .pause-btn.paused .pause-badge { display: inline; }

  /* SOUND SELECTOR */
  .sound-section {
    margin-top: 20px; padding-top: 16px;
    border-top: 1px solid var(--border-subtle);
  }
  .sound-section-label {
    font-size: 0.75rem; text-transform: uppercase;
    letter-spacing: 0.1em; color: var(--text-muted);
    margin-bottom: 8px;
  }
  .sound-select {
    width: 100%; padding: 8px 12px;
    background: var(--surface); border: 1px solid var(--border);
    border-radius: 6px; color: var(--text);
    font-family: "Geist", sans-serif; font-size: 0.8rem;
    outline: none; cursor: pointer;
    appearance: none; -webkit-appearance: none;
    background-image: url("data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' width='12' height='12' viewBox='0 0 24 24' fill='none' stroke='%23888' stroke-width='2'%3E%3Cpath d='m6 9 6 6 6-6'/%3E%3C/svg%3E");
    background-repeat: no-repeat;
    background-position: right 10px center;
  }
  .sound-select:focus { border-color: var(--accent); }
  .sound-select option { background: var(--surface); color: var(--text); }
  .sound-saved {
    font-size: 0.7rem; color: var(--green);
    margin-top: 6px; opacity: 0;
    transition: opacity 0.2s;
  }
  .sound-saved.show { opacity: 1; }

  /* CHANNEL COLUMN */
  .col-channel {
    color: var(--orange); width: 110px; font-size: 0.8rem;
  }
  @media (max-width: 768px) {
    .col-channel { display: none; }
  }

  /* BOOKMARK STAR */
  .bookmark-btn {
    background: none; border: none; cursor: pointer; padding: 2px;
    color: var(--text-dim); font-size: 0.85rem; transition: color 0.15s;
  }
  .bookmark-btn:hover { color: var(--amber); }
  .bookmark-btn.active { color: var(--amber); }

  /* STATS PANEL */
  .stats-panel {
    display: none; padding: 20px; border-bottom: 1px solid var(--border);
    background: var(--surface);
  }
  .stats-panel.active { display: block; }
  .stats-panel h3 {
    font-size: 0.75rem; text-transform: uppercase;
    letter-spacing: 0.08em; color: var(--text-muted);
    margin-bottom: 12px;
  }
  .stats-row {
    display: flex; align-items: center; gap: 8px;
    margin-bottom: 6px; font-size: 0.8rem;
  }
  .stats-label { width: 100px; color: var(--text-secondary); font-weight: 500; }
  .stats-bar-bg {
    flex: 1; height: 14px; background: var(--border-subtle);
    border-radius: 3px; overflow: hidden; max-width: 200px;
  }
  .stats-bar-fill {
    height: 100%; border-radius: 3px;
    transition: width 0.3s;
  }
  .stats-count { font-size: 0.75rem; color: var(--text-dim); font-family: "Geist Mono", monospace; }
  .stats-grid { display: flex; gap: 32px; flex-wrap: wrap; }
  .stats-section { min-width: 280px; flex: 1; }

  /* SWIMLANE */
  .swimlane-wrap { margin-top: 12px; }
  .swimlane-row {
    display: flex; align-items: center; gap: 8px;
    margin-bottom: 3px; font-size: 0.75rem;
  }
  .swimlane-label { width: 100px; font-weight: 500; }
  .swimlane-track { display: flex; gap: 1px; }
  .swimlane-cell {
    width: 6px; height: 14px; border-radius: 1px;
    background: var(--border-subtle);
  }
  .swimlane-cell.active { background: var(--accent); }
  .swimlane-time-labels {
    display: flex; justify-content: space-between;
    font-size: 0.65rem; color: var(--text-dim);
    margin-left: 108px; max-width: 366px;
  }

  /* DIFF VIEW */
  .diff-view {
    background: var(--bg); border: 1px solid var(--border);
    border-radius: 8px; padding: 16px; margin-top: 8px;
    overflow-x: auto; font-size: 0.75rem;
    font-family: "Geist Mono", monospace;
    line-height: 1.5; max-height: 400px;
    overflow-y: auto; white-space: pre-wrap;
  }
  .diff-add { color: var(--green); }
  .diff-rem { color: var(--rose); }
  .diff-ctx { color: var(--text-dim); }
  .diff-header { color: var(--text-muted); margin-bottom: 8px; }

  /* KEYBOARD HELP OVERLAY */
  .kb-overlay {
    display: none; position: fixed; inset: 0;
    background: oklch(0% 0 0 / 0.6); z-index: 100;
    justify-content: center; align-items: center;
  }
  .kb-overlay.open { display: flex; }
  .kb-panel {
    background: var(--surface); border: 1px solid var(--border);
    border-radius: 12px; padding: 24px 32px;
    max-width: 420px; width: 90%;
  }
  .kb-panel h3 {
    font-size: 1rem; color: var(--text); margin-bottom: 16px;
    font-weight: 600;
  }
  .kb-row {
    display: flex; align-items: center; gap: 12px;
    margin-bottom: 8px; font-size: 0.85rem;
  }
  .kb-key {
    display: inline-block; min-width: 28px; text-align: center;
    padding: 2px 8px; background: var(--bg);
    border: 1px solid var(--border); border-radius: 4px;
    font-family: "Geist Mono", monospace; font-size: 0.75rem;
    color: var(--accent); font-weight: 600;
  }
  .kb-desc { color: var(--text-secondary); }

  /* FILTER MODE BADGE */
  .filter-mode {
    font-size: 0.65rem; color: var(--accent);
    font-family: "Geist Mono", monospace;
    white-space: nowrap;
  }

  /* TOOLBAR EXTRAS */
  .toolbar-btn {
    background: none; border: 1px solid var(--border);
    border-radius: 6px; cursor: pointer; padding: 5px 10px;
    color: var(--text-muted); display: flex; align-items: center; gap: 6px;
    font-size: 0.75rem; font-family: "Geist", sans-serif;
    transition: border-color 0.15s, color 0.15s; white-space: nowrap;
  }
  .toolbar-btn:hover { border-color: var(--text-muted); color: var(--text); }
  .toolbar-btn.active { border-color: var(--accent); color: var(--accent); }
  .toolbar-btn svg { width: 14px; height: 14px; }

  /* ACTIVE ROW HIGHLIGHT */
  .events-table tbody tr.kb-selected {
    outline: 2px solid var(--accent); outline-offset: -2px;
  }

  /* DETAIL ACTION BUTTONS */
  .detail-actions {
    display: flex; gap: 8px; margin-top: 8px;
  }
  .detail-btn {
    padding: 4px 12px; background: var(--bg);
    border: 1px solid var(--border); border-radius: 6px;
    color: var(--text-muted); font-size: 0.7rem; cursor: pointer;
    font-family: "Geist", sans-serif;
    transition: border-color 0.15s, color 0.15s;
  }
  .detail-btn:hover { border-color: var(--text-muted); color: var(--text); }
  .detail-btn.active { border-color: var(--accent); color: var(--accent); }
</style>
</head>
<body>

<!-- NAV_HTML -->

<!-- CONNECT SCREEN -->
<div class="connect-screen" id="connect-screen">
  <h1>Dashboard</h1>
  <p>Enter your workspace ID to view channels and live events.</p>
  <div class="connect-form">
    <input type="text" id="ws-input" placeholder="ws_230a2bc06cb0" autocomplete="off" spellcheck="false">
    <button onclick="connectWorkspace()">Connect</button>
  </div>
  <div class="connect-error" id="connect-error"></div>
</div>

<!-- DASHBOARD -->
<div class="dashboard" id="dashboard">
  <div class="sidebar-overlay" id="sidebar-overlay" onclick="toggleSidebar()"></div>
  <aside class="sidebar" id="sidebar">
    <div class="sidebar-header">
      <span class="sidebar-title">Channels</span>
      <span class="ws-id" id="sidebar-ws-id"></span>
    </div>
    <ul class="channel-list" id="channel-list"></ul>
    <div class="sound-section">
      <div class="sound-section-label">Notification Sound</div>
      <select class="sound-select" id="sound-select" onchange="changeSound(this.value)">
        <option value="">Default (Sosumi)</option>
        <option value="Basso">Basso</option>
        <option value="Blow">Blow</option>
        <option value="Bottle">Bottle</option>
        <option value="Frog">Frog</option>
        <option value="Funk">Funk</option>
        <option value="Glass">Glass</option>
        <option value="Hero">Hero</option>
        <option value="Morse">Morse</option>
        <option value="Ping">Ping</option>
        <option value="Pop">Pop</option>
        <option value="Purr">Purr</option>
        <option value="Sosumi">Sosumi</option>
        <option value="Submarine">Submarine</option>
        <option value="Tink">Tink</option>
      </select>
      <div class="sound-saved" id="sound-saved">Saved</div>
    </div>
    <button class="disconnect-btn" onclick="disconnect()">Disconnect</button>
  </aside>

  <div class="main-area">
    <div class="toolbar">
      <button class="mobile-sidebar-btn" onclick="toggleSidebar()" aria-label="Channels"><i data-lucide="menu"></i></button>
      <div class="toolbar-status">
        <span class="status-dot" id="status-dot"></span>
        <span id="status-text">Connecting...</span>
      </div>
      <button class="pause-btn" id="pause-btn" onclick="togglePause()"><i data-lucide="pause"></i><span id="pause-label">Pause</span><span class="pause-badge" id="pause-badge"></span></button>
      <button class="toolbar-btn" id="stats-btn" onclick="toggleStats()"><i data-lucide="bar-chart-2"></i>Stats</button>
      <button class="toolbar-btn" id="bookmarks-btn" onclick="toggleBookmarkFilter()"><i data-lucide="star"></i><span id="bookmarks-label">Bookmarks</span></button>
      <button class="pause-btn" onclick="exportEvents('json')">Export JSON</button>
      <button class="pause-btn" onclick="exportEvents('csv')">Export CSV</button>
      <button class="toolbar-btn" onclick="exportHTML()"><i data-lucide="file-text"></i>HTML</button>
      <button class="toolbar-btn" onclick="toggleKbHelp()"><i data-lucide="keyboard"></i>?</button>
      <input type="text" class="filter-input" id="filter-input" placeholder="Filter: text, source:name, !exclude">
      <span class="filter-mode" id="filter-mode"></span>
      <div class="health-indicator" id="health-indicator"></div>
      <span class="event-count" id="event-count"></span>
    </div>
    <div class="stats-panel" id="stats-panel">
      <div class="stats-grid">
        <div class="stats-section">
          <h3>Events by Source</h3>
          <div id="stats-sources"></div>
        </div>
        <div class="stats-section">
          <h3>Status Breakdown</h3>
          <div id="stats-status"></div>
        </div>
        <div class="stats-section">
          <h3>Swimlane Timeline (last 60 min)</h3>
          <div class="swimlane-time-labels"><span>-60m</span><span>-30m</span><span>now</span></div>
          <div class="swimlane-wrap" id="stats-swimlane"></div>
        </div>
      </div>
    </div>
    <div class="events-container" id="events-container">
      <table class="events-table">
        <thead>
          <tr>
            <th style="width:32px"></th>
            <th style="width:28px"></th>
            <th>Time</th>
            <th>Channel</th>
            <th>Source</th>
            <th class="col-type">Type</th>
            <th>Summary</th>
          </tr>
        </thead>
        <tbody id="events-body"></tbody>
      </table>
      <div class="load-more" id="load-more"><button onclick="loadMore()" id="load-more-btn">Load older events</button></div>
      <div class="empty-state" id="empty-state">
        <i data-lucide="inbox"></i>
        <p>No events yet. Send a webhook to see it here.</p>
      </div>
    </div>
  </div>
</div>

<!-- KEYBOARD HELP OVERLAY -->
<div class="kb-overlay" id="kb-overlay" onclick="if(event.target===this)toggleKbHelp()">
  <div class="kb-panel">
    <h3>Keyboard Shortcuts</h3>
    <div class="kb-row"><span class="kb-key">j</span><span class="kb-key">k</span> <span class="kb-desc">Navigate down / up</span></div>
    <div class="kb-row"><span class="kb-key">Enter</span> <span class="kb-desc">Toggle event detail</span></div>
    <div class="kb-row"><span class="kb-key">/</span> <span class="kb-desc">Focus filter input</span></div>
    <div class="kb-row"><span class="kb-key">f</span> <span class="kb-desc">Bookmark selected event</span></div>
    <div class="kb-row"><span class="kb-key">F</span> <span class="kb-desc">Toggle bookmarks view</span></div>
    <div class="kb-row"><span class="kb-key">d</span> <span class="kb-desc">Diff with previous same-source event</span></div>
    <div class="kb-row"><span class="kb-key">s</span> <span class="kb-desc">Toggle stats panel</span></div>
    <div class="kb-row"><span class="kb-key">p</span> <span class="kb-desc">Pause / resume live feed</span></div>
    <div class="kb-row"><span class="kb-key">c</span> <span class="kb-desc">Copy payload of selected event</span></div>
    <div class="kb-row"><span class="kb-key">?</span> <span class="kb-desc">Toggle this help</span></div>
    <div class="kb-row"><span class="kb-key">Esc</span> <span class="kb-desc">Close overlay / clear filter</span></div>
    <div style="margin-top:12px;font-size:0.75rem;color:var(--text-dim)">Supports source:name, type:name, !exclude filter syntax</div>
  </div>
</div>

<script>
lucide.createIcons();

var state = {
  ws: null,
  channels: [],
  channelNames: {},
  webhookURLs: {},
  events: [],
  workspaceId: '',
  filter: '',
  paused: false,
  pauseBuffer: [],
  hasMore: false,
  loadingMore: false,
  unreadCount: 0,
  bookmarks: JSON.parse(localStorage.getItem('dread_bookmarks') || '{}'),
  bookmarkFilter: false,
  kbCursor: -1,
  showStats: false,
  showKbHelp: false
};

// Theme
function toggleTheme() {
  var root = document.documentElement;
  var isLight = root.classList.toggle('light');
  localStorage.setItem('theme', isLight ? 'light' : 'dark');
  var icon = document.getElementById('theme-icon');
  if (icon) {
    icon.setAttribute('data-lucide', isLight ? 'sun' : 'moon');
    lucide.createIcons({attrs:{class:'lucide'},nameAttr:'data-lucide'});
  }
}
(function() {
  if (localStorage.getItem('theme') === 'light') {
    document.documentElement.classList.add('light');
    var i = document.getElementById('theme-icon');
    if (i) i.setAttribute('data-lucide', 'sun');
    lucide.createIcons({attrs:{class:'lucide'},nameAttr:'data-lucide'});
  }
})();

// Restore last workspace or deep link
(function() {
  var params = new URLSearchParams(window.location.search);
  var wsParam = params.get('ws');
  if (wsParam) {
    document.getElementById('ws-input').value = wsParam;
    // Auto-connect from URL param after icons init
    setTimeout(function() { connectWorkspace(); }, 0);
  } else {
    var saved = localStorage.getItem('dread_workspace_id');
    if (saved) {
      document.getElementById('ws-input').value = saved;
    }
  }
})();

function connectWorkspace() {
  var input = document.getElementById('ws-input');
  var id = input.value.trim();
  if (!id) return;
  var errEl = document.getElementById('connect-error');
  errEl.style.display = 'none';

  fetch('/api/workspaces/' + encodeURIComponent(id))
    .then(function(res) {
      if (!res.ok) throw new Error('Workspace not found');
      return res.json();
    })
    .then(function(data) {
      state.workspaceId = id;
      state.channels = data.channels || [];
      state.sound = data.sound || '';
      state.channelNames = {};
      state.channels.forEach(function(ch) {
        state.channelNames[ch.id] = ch.name || ch.id;
      });
      localStorage.setItem('dread_workspace_id', id);
      // Update URL with workspace ID for deep linking
      var url = new URL(window.location);
      url.searchParams.set('ws', id);
      history.replaceState(null, '', url);
      showDashboard();
    })
    .catch(function(err) {
      errEl.textContent = err.message || 'Failed to load workspace';
      errEl.style.display = 'block';
    });
}

// Enter key on input
document.getElementById('ws-input').addEventListener('keydown', function(e) {
  if (e.key === 'Enter') connectWorkspace();
});

function showDashboard() {
  document.getElementById('connect-screen').style.display = 'none';
  document.getElementById('dashboard').classList.add('active');
  document.getElementById('sidebar-ws-id').textContent = state.workspaceId;
  document.getElementById('sound-select').value = state.sound;
  renderChannels();
  connectWS();
  fetchHistory();
  // Refresh relative timestamps every 30s
  if (state.timeInterval) clearInterval(state.timeInterval);
  state.timeInterval = setInterval(refreshTimes, 30000);
}

function disconnect() {
  if (state.ws) { state.ws.close(); state.ws = null; }
  if (state.timeInterval) { clearInterval(state.timeInterval); state.timeInterval = null; }
  state.events = [];
  state.channels = [];
  state.channelNames = {};
  state.webhookURLs = {};
  state.paused = false;
  state.pauseBuffer = [];
  state.hasMore = false;
  state.unreadCount = 0;
  document.title = 'Dashboard | dread.sh';
  document.getElementById('events-body').innerHTML = '';
  document.getElementById('load-more').classList.remove('active');
  document.getElementById('dashboard').classList.remove('active');
  document.getElementById('connect-screen').style.display = '';
  var url = new URL(window.location);
  url.searchParams.delete('ws');
  history.replaceState(null, '', url);
  updateStatus(false);
  updateEventCount();
  updatePauseUI();
}

function renderChannels() {
  var list = document.getElementById('channel-list');
  list.innerHTML = '';
  state.channels.forEach(function(ch) {
    var li = document.createElement('li');
    li.className = 'channel-item';
    var url = state.webhookURLs[ch.id] || '...';
    li.innerHTML = '<div class="channel-name">' + esc(ch.name || ch.id) + '</div>' +
      '<div class="channel-url-row">' +
        '<span class="channel-url" title="' + esc(url) + '">' + esc(url) + '</span>' +
        '<button class="copy-btn" onclick="copyUrl(this, \'' + esc(ch.id) + '\')" title="Copy webhook URL"><i data-lucide="copy"></i></button>' +
      '</div>';
    list.appendChild(li);
  });
  lucide.createIcons({attrs:{class:'lucide'},nameAttr:'data-lucide'});
}

function copyUrl(btn, channelId) {
  var url = state.webhookURLs[channelId];
  if (!url) return;
  navigator.clipboard.writeText(url).then(function() {
    btn.classList.add('copied');
    var svg = btn.querySelector('svg');
    if (svg) svg.setAttribute('data-lucide', 'check');
    lucide.createIcons({attrs:{class:'lucide'},nameAttr:'data-lucide'});
    setTimeout(function() {
      btn.classList.remove('copied');
      if (svg) svg.setAttribute('data-lucide', 'copy');
      lucide.createIcons({attrs:{class:'lucide'},nameAttr:'data-lucide'});
    }, 1500);
  });
}

function channelIds() {
  return state.channels.map(function(ch) { return ch.id; });
}

function connectWS() {
  var ids = channelIds();
  if (ids.length === 0) return;
  var proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  var url = proto + '//' + location.host + '/ws?channels=' + ids.join(',');
  var ws = new WebSocket(url);
  state.ws = ws;

  ws.onopen = function() { updateStatus(true); };
  ws.onclose = function() {
    updateStatus(false);
    // Reconnect after 3s
    setTimeout(function() {
      if (state.channels.length > 0) connectWS();
    }, 3000);
  };
  ws.onmessage = function(e) {
    var msg;
    try { msg = JSON.parse(e.data); } catch(_) { return; }
    if (msg.type === 'registered' && msg.webhook_urls) {
      state.webhookURLs = msg.webhook_urls;
      renderChannels();
    } else if (msg.type === 'event' && msg.event) {
      if (state.paused) {
        state.pauseBuffer.push(msg.event);
        updatePauseUI();
      } else {
        addEvent(msg.event, true);
      }
      // Track unread when tab is hidden
      if (document.hidden) {
        state.unreadCount++;
        document.title = '(' + state.unreadCount + ') Dashboard | dread.sh';
      }
    }
  };
}

function updateStatus(connected) {
  var dot = document.getElementById('status-dot');
  var text = document.getElementById('status-text');
  if (connected) {
    dot.classList.add('connected');
    text.textContent = 'Connected';
  } else {
    dot.classList.remove('connected');
    text.textContent = 'Disconnected';
  }
}

function fetchHistory(before) {
  var ids = channelIds();
  if (ids.length === 0) return;
  var url = '/api/events?channels=' + ids.join(',') + '&limit=50';
  if (before) url += '&before=' + encodeURIComponent(before);
  fetch(url)
    .then(function(res) { return res.json(); })
    .then(function(data) {
      if (data.events) {
        data.events.forEach(function(ev) { addEvent(ev, false); });
      }
      state.hasMore = !!data.has_more;
      var loadMoreEl = document.getElementById('load-more');
      if (state.hasMore) {
        loadMoreEl.classList.add('active');
      } else {
        loadMoreEl.classList.remove('active');
      }
      state.loadingMore = false;
      updateEventCount();
    })
    .catch(function() { state.loadingMore = false; });
}

function loadMore() {
  if (state.loadingMore || !state.hasMore) return;
  state.loadingMore = true;
  var btn = document.getElementById('load-more-btn');
  btn.disabled = true;
  btn.textContent = 'Loading...';
  // Find oldest event timestamp
  var oldest = state.events[state.events.length - 1];
  var before = oldest ? oldest.timestamp : '';
  fetchHistory(before);
  btn.disabled = false;
  btn.textContent = 'Load older events';
}

function addEvent(ev, isLive) {
  // Deduplicate
  for (var i = 0; i < state.events.length; i++) {
    if (state.events[i].id === ev.id) return;
  }

  if (isLive) {
    state.events.unshift(ev);
  } else {
    state.events.push(ev);
  }

  var empty = document.getElementById('empty-state');
  if (empty) empty.style.display = 'none';

  var tbody = document.getElementById('events-body');
  var row = createEventRow(ev, isLive);
  var detail = createDetailRow(ev);

  if (isLive) {
    tbody.insertBefore(detail, tbody.firstChild);
    tbody.insertBefore(row, tbody.firstChild);
  } else {
    tbody.appendChild(row);
    tbody.appendChild(detail);
  }

  applyFilter();
  updateEventCount();
}

function createEventRow(ev, isLive) {
  var tr = document.createElement('tr');
  tr.setAttribute('data-event-id', ev.id);
  tr.setAttribute('data-ts', ev.timestamp);
  tr.className = 'event-row' + (isLive ? ' new-event' : '');
  tr.onclick = function() { toggleDetail(ev.id); };
  var chName = state.channelNames[ev.channel] || ev.channel || '';
  var status = classifyEvent(ev.type, ev.summary);
  var dotColor = status === 'success' ? 'var(--green)' : status === 'failure' ? 'var(--rose)' : 'var(--text-dim)';
  var starred = state.bookmarks[ev.id] ? ' active' : '';
  tr.innerHTML =
    '<td style="width:32px;text-align:center"><span style="color:' + dotColor + ';font-size:0.6rem">&#9679;</span></td>' +
    '<td style="width:28px;text-align:center"><button class="bookmark-btn' + starred + '" onclick="toggleBookmark(\'' + esc(ev.id) + '\', event)" title="Bookmark">&#9733;</button></td>' +
    '<td class="col-time">' + formatTime(ev.timestamp) + '</td>' +
    '<td class="col-channel">' + esc(chName) + '</td>' +
    '<td class="col-source" style="color:' + sourceColour(ev.source) + '">' + esc(ev.source) + '</td>' +
    '<td class="col-type">' + esc(ev.type) + '</td>' +
    '<td class="col-summary">' + esc(ev.summary) + '</td>';
  return tr;
}

function createDetailRow(ev) {
  var tr = document.createElement('tr');
  tr.className = 'event-detail';
  tr.setAttribute('data-detail-for', ev.id);
  var td = document.createElement('td');
  td.setAttribute('colspan', '7');
  var json = '';
  try {
    var parsed = typeof ev.raw_json === 'string' ? JSON.parse(ev.raw_json) : ev.raw_json;
    json = syntaxHighlight(JSON.stringify(parsed, null, 2));
  } catch(_) {
    json = esc(ev.raw_json || '{}');
  }
  var rawStr = '';
  try {
    var p = typeof ev.raw_json === 'string' ? JSON.parse(ev.raw_json) : ev.raw_json;
    rawStr = JSON.stringify(p, null, 2);
  } catch(_) { rawStr = ev.raw_json || '{}'; }
  td.innerHTML = '<div class="json-viewer-wrap"><button class="copy-json" onclick="copyPayload(this, event)">Copy</button><div class="json-viewer" id="json-' + esc(ev.id) + '">' + json + '</div></div>' +
    '<div class="detail-actions">' +
      '<button class="detail-btn" onclick="showDiff(\'' + esc(ev.id) + '\', event)">Diff with previous</button>' +
      '<button class="detail-btn" onclick="replayEvent(\'' + esc(ev.id) + '\')">Replay</button>' +
    '</div>' +
    '<div id="diff-' + esc(ev.id) + '"></div>';
  td._rawJson = rawStr;
  tr.appendChild(td);
  return tr;
}

function copyPayload(btn, e) {
  e.stopPropagation();
  var td = btn.closest('td');
  var text = td._rawJson || btn.closest('.json-viewer-wrap').querySelector('.json-viewer').textContent;
  navigator.clipboard.writeText(text).then(function() {
    btn.textContent = 'Copied!';
    setTimeout(function() { btn.textContent = 'Copy'; }, 1500);
  });
}

function toggleDetail(id) {
  var detail = document.querySelector('[data-detail-for="' + id + '"]');
  if (detail) detail.classList.toggle('open');
}

function formatTime(ts) {
  var d = new Date(ts);
  var now = new Date();
  var diff = (now - d) / 1000;
  if (diff < 60) return Math.floor(diff) + 's ago';
  if (diff < 3600) return Math.floor(diff / 60) + 'm ago';
  if (diff < 86400) return Math.floor(diff / 3600) + 'h ago';
  return d.toLocaleDateString() + ' ' + d.toLocaleTimeString([], {hour:'2-digit',minute:'2-digit'});
}

function syntaxHighlight(json) {
  json = esc(json);
  return json.replace(/("(\\u[a-fA-F0-9]{4}|\\[^u]|[^\\"])*"(\s*:)?|\b(true|false|null)\b|-?\d+(?:\.\d*)?(?:[eE][+\-]?\d+)?)/g, function(match) {
    var cls = 'json-number';
    if (/^"/.test(match)) {
      if (/:$/.test(match)) {
        cls = 'json-key';
        return '<span class="' + cls + '">' + match.slice(0, -1) + '</span>:';
      } else {
        cls = 'json-string';
      }
    } else if (/true|false/.test(match)) {
      cls = 'json-bool';
    } else if (/null/.test(match)) {
      cls = 'json-null';
    }
    return '<span class="' + cls + '">' + match + '</span>';
  });
}

var SOURCE_COLOURS = {
  stripe: 'oklch(65% 0.18 293)',
  github: 'oklch(72% 0.17 142)',
  gitlab: 'oklch(72% 0.18 30)',
  vercel: 'oklch(75% 0.0 0)',
  sentry: 'oklch(70% 0.18 25)',
  linear: 'oklch(65% 0.18 270)',
  jira: 'oklch(65% 0.17 255)',
  slack: 'oklch(70% 0.15 155)',
  discord: 'oklch(68% 0.18 280)',
  paypal: 'oklch(65% 0.17 250)',
  shopify: 'oklch(70% 0.18 145)',
  twilio: 'oklch(65% 0.15 15)',
  sendgrid: 'oklch(65% 0.17 240)',
  pagerduty: 'oklch(70% 0.18 140)',
  hubspot: 'oklch(70% 0.18 30)',
  typeform: 'oklch(65% 0.15 320)',
  paddle: 'oklch(65% 0.15 230)',
  supabase: 'oklch(70% 0.18 155)',
  aws: 'oklch(72% 0.18 55)',
  cloudflare: 'oklch(72% 0.18 55)',
  grafana: 'oklch(72% 0.18 55)',
  datadog: 'oklch(65% 0.18 290)',
  mailchimp: 'oklch(72% 0.18 80)',
  zendesk: 'oklch(68% 0.15 160)',
  intercom: 'oklch(65% 0.17 240)',
  postmark: 'oklch(72% 0.18 80)',
  telegram: 'oklch(65% 0.17 230)',
  figma: 'oklch(65% 0.18 340)',
  zapier: 'oklch(72% 0.18 40)',
  'trigger.dev': 'oklch(70% 0.18 165)',
  test: 'oklch(70% 0.15 200)',
  webhook: 'oklch(70.7% 0.165 254.62)'
};
var SOURCE_COLOUR_POOL = [
  'oklch(70.7% 0.165 254.62)',
  'oklch(70.2% 0.183 293.54)',
  'oklch(75% 0.18 55)',
  'oklch(78.9% 0.154 211.53)',
  'oklch(71.2% 0.194 13.43)',
  'oklch(72% 0.17 142)',
  'oklch(82.8% 0.189 84.43)',
  'oklch(65% 0.18 320)'
];
var sourceColourCache = {};
function classifyEvent(typ, summary) {
  var lower = ((typ || '') + ' ' + (summary || '')).toLowerCase();
  var okWords = ['succeed','success','completed','paid','captured','created','active','resolved','delivered','merged','approved','ready'];
  for (var i = 0; i < okWords.length; i++) { if (lower.indexOf(okWords[i]) !== -1) return 'success'; }
  var failWords = ['fail','error','denied','declined','expired','canceled','cancelled','refused','rejected','dispute','incident','critical','warning','overdue'];
  for (var i = 0; i < failWords.length; i++) { if (lower.indexOf(failWords[i]) !== -1) return 'failure'; }
  return 'neutral';
}

function sourceColour(src) {
  if (!src) return 'var(--text-muted)';
  var lower = src.toLowerCase();
  if (SOURCE_COLOURS[lower]) return SOURCE_COLOURS[lower];
  if (sourceColourCache[lower]) return sourceColourCache[lower];
  var hash = 0;
  for (var i = 0; i < lower.length; i++) hash = ((hash << 5) - hash) + lower.charCodeAt(i);
  sourceColourCache[lower] = SOURCE_COLOUR_POOL[Math.abs(hash) % SOURCE_COLOUR_POOL.length];
  return sourceColourCache[lower];
}

function esc(s) {
  if (!s) return '';
  var d = document.createElement('div');
  d.appendChild(document.createTextNode(s));
  return d.innerHTML;
}

// Filter
document.getElementById('filter-input').addEventListener('input', function() {
  state.filter = this.value.toLowerCase();
  applyFilter();
});

function applyFilter() {
  var f = state.filter;
  var modeEl = document.getElementById('filter-mode');
  var exclude = false;
  var fieldFilter = '';
  var valueFilter = '';
  var searchTerm = f;

  if (f && f.charAt(0) === '!') {
    exclude = true;
    searchTerm = f.substring(1);
    modeEl.textContent = 'exclude';
  } else if (f && f.indexOf(':') > 0) {
    var parts = f.split(':');
    var cand = parts[0].toLowerCase();
    if (cand === 'source' || cand === 'type' || cand === 'channel') {
      fieldFilter = cand;
      valueFilter = parts.slice(1).join(':').toLowerCase();
      modeEl.textContent = fieldFilter;
    } else {
      modeEl.textContent = '';
    }
  } else {
    modeEl.textContent = f ? 'search' : '';
  }

  var rows = document.querySelectorAll('.event-row');
  var visible = 0;
  rows.forEach(function(row) {
    var id = row.getAttribute('data-event-id');
    var ev = null;
    for (var i = 0; i < state.events.length; i++) {
      if (state.events[i].id === id) { ev = state.events[i]; break; }
    }

    // Bookmark filter
    if (state.bookmarkFilter && !state.bookmarks[id]) {
      row.style.display = 'none';
      var detail = document.querySelector('[data-detail-for="' + id + '"]');
      if (detail) { detail.style.display = 'none'; detail.classList.remove('open'); }
      return;
    }

    var show = true;
    if (f && ev) {
      var match = false;
      if (fieldFilter && ev) {
        var val = '';
        if (fieldFilter === 'source') val = (ev.source || '').toLowerCase();
        else if (fieldFilter === 'type') val = (ev.type || '').toLowerCase();
        else if (fieldFilter === 'channel') val = (ev.channel || '').toLowerCase();
        match = val.indexOf(valueFilter) !== -1;
      } else {
        var text = row.textContent.toLowerCase();
        match = text.indexOf(searchTerm.toLowerCase()) !== -1;
      }
      show = exclude ? !match : match;
    }

    row.style.display = show ? '' : 'none';
    var detail = document.querySelector('[data-detail-for="' + id + '"]');
    if (detail && !show) {
      detail.style.display = 'none';
      detail.classList.remove('open');
    } else if (detail && show) {
      detail.style.display = '';
    }
    if (show) visible++;
  });
  var countEl = document.getElementById('event-count');
  if (f || state.bookmarkFilter) {
    countEl.textContent = visible + ' / ' + state.events.length;
  } else {
    updateEventCount();
  }
}

function updateEventCount() {
  var countEl = document.getElementById('event-count');
  countEl.textContent = state.events.length + ' event' + (state.events.length !== 1 ? 's' : '');
  updateHealthIndicator();
}

function updateHealthIndicator() {
  var s = 0, f = 0, n = 0;
  for (var i = 0; i < state.events.length; i++) {
    var c = classifyEvent(state.events[i].type, state.events[i].summary);
    if (c === 'success') s++;
    else if (c === 'failure') f++;
    else n++;
  }
  var el = document.getElementById('health-indicator');
  el.innerHTML = '<span class="hi-success">✓ ' + s + '</span>' +
    '<span class="hi-failure">✗ ' + f + '</span>' +
    '<span class="hi-neutral">○ ' + n + '</span>';
}

// Refresh relative timestamps
function refreshTimes() {
  var rows = document.querySelectorAll('.event-row');
  rows.forEach(function(row) {
    var ts = row.getAttribute('data-ts');
    if (ts) {
      var cell = row.querySelector('.col-time');
      if (cell) cell.textContent = formatTime(ts);
    }
  });
}

// Pause / Resume
function togglePause() {
  state.paused = !state.paused;
  if (!state.paused) {
    // Flush buffered events
    state.pauseBuffer.forEach(function(ev) { addEvent(ev, true); });
    state.pauseBuffer = [];
  }
  updatePauseUI();
}

function updatePauseUI() {
  var btn = document.getElementById('pause-btn');
  var label = document.getElementById('pause-label');
  var badge = document.getElementById('pause-badge');
  var icon = btn.querySelector('i');
  if (state.paused) {
    btn.classList.add('paused');
    label.textContent = 'Resume';
    if (icon) icon.setAttribute('data-lucide', 'play');
    badge.textContent = state.pauseBuffer.length;
  } else {
    btn.classList.remove('paused');
    label.textContent = 'Pause';
    if (icon) icon.setAttribute('data-lucide', 'pause');
    badge.textContent = '';
  }
  lucide.createIcons({attrs:{class:'lucide'},nameAttr:'data-lucide'});
}

// Tab visibility — reset unread count when tab becomes visible
document.addEventListener('visibilitychange', function() {
  if (!document.hidden) {
    state.unreadCount = 0;
    document.title = 'Dashboard | dread.sh';
  }
});

// Sound selector
function changeSound(sound) {
  state.sound = sound;
  previewSound(sound || 'Sosumi');
  // Save to workspace via API
  var channels = state.channels;
  fetch('/api/workspaces/' + encodeURIComponent(state.workspaceId), {
    method: 'PUT',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({channels: channels, sound: sound})
  }).then(function(res) {
    if (res.ok) {
      var el = document.getElementById('sound-saved');
      el.classList.add('show');
      setTimeout(function() { el.classList.remove('show'); }, 1500);
    }
  }).catch(function() {});
}

// Synthesize a short preview tone for each sound name using Web Audio API.
// Each sound gets a distinct frequency/envelope so the user can hear a difference.
var soundProfiles = {
  Basso:     {freq: 130, dur: 0.25, type: 'sine'},
  Blow:      {freq: 440, dur: 0.4,  type: 'sine'},
  Bottle:    {freq: 880, dur: 0.3,  type: 'sine'},
  Frog:      {freq: 220, dur: 0.2,  type: 'square'},
  Funk:      {freq: 330, dur: 0.15, type: 'square'},
  Glass:     {freq: 1200, dur: 0.15, type: 'sine'},
  Hero:      {freq: 523, dur: 0.5,  type: 'triangle'},
  Morse:     {freq: 800, dur: 0.08, type: 'square'},
  Ping:      {freq: 1000, dur: 0.12, type: 'sine'},
  Pop:       {freq: 600, dur: 0.06, type: 'sine'},
  Purr:      {freq: 180, dur: 0.35, type: 'sine'},
  Sosumi:    {freq: 740, dur: 0.3,  type: 'triangle'},
  Submarine: {freq: 260, dur: 0.5,  type: 'sine'},
  Tink:      {freq: 1400, dur: 0.05, type: 'sine'}
};

function previewSound(name) {
  var p = soundProfiles[name];
  if (!p) return;
  try {
    var ctx = new (window.AudioContext || window.webkitAudioContext)();
    var osc = ctx.createOscillator();
    var gain = ctx.createGain();
    osc.type = p.type;
    osc.frequency.value = p.freq;
    gain.gain.setValueAtTime(0.3, ctx.currentTime);
    gain.gain.exponentialRampToValueAtTime(0.001, ctx.currentTime + p.dur);
    osc.connect(gain);
    gain.connect(ctx.destination);
    osc.start(ctx.currentTime);
    osc.stop(ctx.currentTime + p.dur);
    setTimeout(function() { ctx.close(); }, (p.dur + 0.1) * 1000);
  } catch(_) {}
}

// Mobile sidebar
function toggleSidebar() {
  document.getElementById('sidebar').classList.toggle('open');
  document.getElementById('sidebar-overlay').classList.toggle('open');
}

// Export events
function exportEvents(format) {
  if (!state.channelIds || state.channelIds.length === 0) { alert('No channels connected'); return; }
  window.open('/api/export?channels=' + state.channelIds.join(',') + '&format=' + format, '_blank');
}

// Channel muting (localStorage)
function getMutedChannels() {
  try { return JSON.parse(localStorage.getItem('dread_muted') || '[]'); } catch(e) { return []; }
}
function setMutedChannels(list) {
  localStorage.setItem('dread_muted', JSON.stringify(list));
}
function isChannelMuted(chId) {
  return getMutedChannels().indexOf(chId) >= 0;
}
function toggleMuteChannel(chId) {
  var list = getMutedChannels();
  var idx = list.indexOf(chId);
  if (idx >= 0) { list.splice(idx, 1); } else { list.push(chId); }
  setMutedChannels(list);
}

// Replay event from dashboard
function replayEvent(eventId) {
  var url = prompt('Enter the URL to forward this event to:');
  if (!url) return;
  fetch('/api/replay', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({event_id: eventId, url: url})
  }).then(function(res) { return res.json(); }).then(function(data) {
    if (data.ok) { alert('Replayed successfully (status ' + data.status + ')'); }
    else { alert('Replay failed: ' + (data.error || 'unknown error')); }
  }).catch(function(e) { alert('Error: ' + e); });
}

// Bookmarks
function toggleBookmark(id, e) {
  if (e) e.stopPropagation();
  if (state.bookmarks[id]) {
    delete state.bookmarks[id];
  } else {
    state.bookmarks[id] = true;
  }
  localStorage.setItem('dread_bookmarks', JSON.stringify(state.bookmarks));
  // Update star button
  var btn = document.querySelector('[data-event-id="' + id + '"] .bookmark-btn');
  if (btn) btn.classList.toggle('active');
  updateBookmarkLabel();
  if (state.bookmarkFilter) applyFilter();
}

function toggleBookmarkFilter() {
  state.bookmarkFilter = !state.bookmarkFilter;
  var btn = document.getElementById('bookmarks-btn');
  btn.classList.toggle('active', state.bookmarkFilter);
  applyFilter();
}

function updateBookmarkLabel() {
  var count = Object.keys(state.bookmarks).length;
  var label = document.getElementById('bookmarks-label');
  label.textContent = count > 0 ? 'Bookmarks (' + count + ')' : 'Bookmarks';
}

// Stats panel
function toggleStats() {
  state.showStats = !state.showStats;
  var panel = document.getElementById('stats-panel');
  var btn = document.getElementById('stats-btn');
  panel.classList.toggle('active', state.showStats);
  btn.classList.toggle('active', state.showStats);
  if (state.showStats) renderStats();
}

function renderStats() {
  // Source breakdown
  var srcCounts = {};
  var statusCounts = {success: 0, failure: 0, neutral: 0};
  var maxSrc = 0;
  state.events.forEach(function(ev) {
    srcCounts[ev.source] = (srcCounts[ev.source] || 0) + 1;
    if (srcCounts[ev.source] > maxSrc) maxSrc = srcCounts[ev.source];
    var s = classifyEvent(ev.type, ev.summary);
    statusCounts[s]++;
  });

  var srcEl = document.getElementById('stats-sources');
  var sorted = Object.entries(srcCounts).sort(function(a, b) { return b[1] - a[1]; });
  var html = '';
  sorted.slice(0, 10).forEach(function(pair) {
    var pct = maxSrc > 0 ? (pair[1] / maxSrc * 100) : 0;
    html += '<div class="stats-row">' +
      '<span class="stats-label" style="color:' + sourceColour(pair[0]) + '">' + esc(pair[0]) + '</span>' +
      '<div class="stats-bar-bg"><div class="stats-bar-fill" style="width:' + pct + '%;background:' + sourceColour(pair[0]) + '"></div></div>' +
      '<span class="stats-count">' + pair[1] + '</span></div>';
  });
  srcEl.innerHTML = html;

  // Status breakdown
  var total = statusCounts.success + statusCounts.failure + statusCounts.neutral;
  var statusEl = document.getElementById('stats-status');
  var statusHTML = '';
  [{label:'success',count:statusCounts.success,color:'var(--green)'},{label:'failure',count:statusCounts.failure,color:'var(--rose)'},{label:'neutral',count:statusCounts.neutral,color:'var(--text-dim)'}].forEach(function(item) {
    var pct = total > 0 ? (item.count / total * 100) : 0;
    statusHTML += '<div class="stats-row">' +
      '<span class="stats-label" style="color:' + item.color + '">' + item.label + '</span>' +
      '<div class="stats-bar-bg"><div class="stats-bar-fill" style="width:' + pct + '%;background:' + item.color + '"></div></div>' +
      '<span class="stats-count">' + item.count + ' (' + Math.round(pct) + '%)</span></div>';
  });
  statusEl.innerHTML = statusHTML;

  // Swimlane
  renderSwimlane();
}

function renderSwimlane() {
  var now = Date.now();
  var sources = {};
  state.events.forEach(function(ev) {
    if (!sources[ev.source]) sources[ev.source] = [];
    sources[ev.source].push(new Date(ev.timestamp).getTime());
  });

  var swimEl = document.getElementById('stats-swimlane');
  var html = '';
  var sortedSrc = Object.keys(sources).sort();
  sortedSrc.forEach(function(src) {
    var buckets = new Array(60).fill(0);
    sources[src].forEach(function(ts) {
      var age = (now - ts) / 60000;
      if (age >= 0 && age < 60) {
        var idx = 59 - Math.floor(age);
        buckets[idx]++;
      }
    });
    html += '<div class="swimlane-row">' +
      '<span class="swimlane-label" style="color:' + sourceColour(src) + '">' + esc(src) + '</span>' +
      '<div class="swimlane-track">';
    buckets.forEach(function(c) {
      html += '<div class="swimlane-cell' + (c > 0 ? ' active' : '') + '" style="' + (c > 0 ? 'background:' + sourceColour(src) : '') + '"></div>';
    });
    html += '</div></div>';
  });
  swimEl.innerHTML = html;
}

// Diff view
function showDiff(id, e) {
  if (e) e.stopPropagation();
  var diffEl = document.getElementById('diff-' + id);
  if (!diffEl) return;
  if (diffEl.innerHTML) { diffEl.innerHTML = ''; return; }

  var current = null;
  for (var i = 0; i < state.events.length; i++) {
    if (state.events[i].id === id) { current = state.events[i]; break; }
  }
  if (!current) return;

  // Find previous event from same source
  var prev = null;
  var foundCurrent = false;
  for (var i = 0; i < state.events.length; i++) {
    if (state.events[i].id === id) { foundCurrent = true; continue; }
    if (foundCurrent && state.events[i].source === current.source) {
      prev = state.events[i]; break;
    }
  }
  if (!prev) {
    diffEl.innerHTML = '<div class="diff-view"><span class="diff-ctx">No previous event from this source to diff against.</span></div>';
    return;
  }

  var oldLines = prettyJSON(prev.raw_json).split('\n');
  var newLines = prettyJSON(current.raw_json).split('\n');
  var maxLen = Math.max(oldLines.length, newLines.length);
  var html = '<div class="diff-view">';
  html += '<div class="diff-header">--- ' + esc(prev.type) + ' (' + formatTime(prev.timestamp) + ')</div>';
  html += '<div class="diff-header">+++ ' + esc(current.type) + ' (' + formatTime(current.timestamp) + ')</div>';
  for (var i = 0; i < maxLen; i++) {
    var oldL = i < oldLines.length ? oldLines[i] : '';
    var newL = i < newLines.length ? newLines[i] : '';
    if (oldL === newL) {
      html += '<div class="diff-ctx">  ' + esc(newL) + '</div>';
    } else {
      if (oldL) html += '<div class="diff-rem">- ' + esc(oldL) + '</div>';
      if (newL) html += '<div class="diff-add">+ ' + esc(newL) + '</div>';
    }
  }
  html += '</div>';
  diffEl.innerHTML = html;
}

function prettyJSON(raw) {
  try {
    var parsed = typeof raw === 'string' ? JSON.parse(raw) : raw;
    return JSON.stringify(parsed, null, 2);
  } catch(_) { return raw || '{}'; }
}

// Keyboard help
function toggleKbHelp() {
  state.showKbHelp = !state.showKbHelp;
  document.getElementById('kb-overlay').classList.toggle('open', state.showKbHelp);
}

// Keyboard navigation
document.addEventListener('keydown', function(e) {
  // Skip if typing in input
  if (e.target.tagName === 'INPUT' || e.target.tagName === 'TEXTAREA' || e.target.tagName === 'SELECT') {
    if (e.key === 'Escape') {
      e.target.blur();
      e.target.value = '';
      state.filter = '';
      applyFilter();
    }
    return;
  }

  if (state.showKbHelp) {
    if (e.key === 'Escape' || e.key === '?') toggleKbHelp();
    return;
  }

  switch(e.key) {
    case '?':
      toggleKbHelp();
      e.preventDefault();
      break;
    case '/':
      document.getElementById('filter-input').focus();
      e.preventDefault();
      break;
    case 'j':
      kbNavigate(1);
      e.preventDefault();
      break;
    case 'k':
      kbNavigate(-1);
      e.preventDefault();
      break;
    case 'Enter':
      if (state.kbCursor >= 0) {
        var rows = getVisibleRows();
        if (state.kbCursor < rows.length) {
          var id = rows[state.kbCursor].getAttribute('data-event-id');
          toggleDetail(id);
        }
      }
      e.preventDefault();
      break;
    case 'f':
      if (state.kbCursor >= 0) {
        var rows = getVisibleRows();
        if (state.kbCursor < rows.length) {
          var id = rows[state.kbCursor].getAttribute('data-event-id');
          toggleBookmark(id);
        }
      }
      e.preventDefault();
      break;
    case 'F':
      toggleBookmarkFilter();
      e.preventDefault();
      break;
    case 'd':
      if (state.kbCursor >= 0) {
        var rows = getVisibleRows();
        if (state.kbCursor < rows.length) {
          var id = rows[state.kbCursor].getAttribute('data-event-id');
          // Ensure detail is open first
          var detail = document.querySelector('[data-detail-for="' + id + '"]');
          if (detail && !detail.classList.contains('open')) toggleDetail(id);
          showDiff(id);
        }
      }
      e.preventDefault();
      break;
    case 's':
      toggleStats();
      e.preventDefault();
      break;
    case 'p':
      togglePause();
      e.preventDefault();
      break;
    case 'c':
      if (state.kbCursor >= 0) {
        var rows = getVisibleRows();
        if (state.kbCursor < rows.length) {
          var id = rows[state.kbCursor].getAttribute('data-event-id');
          var ev = state.events.find(function(e) { return e.id === id; });
          if (ev) navigator.clipboard.writeText(prettyJSON(ev.raw_json));
        }
      }
      e.preventDefault();
      break;
    case 'Escape':
      state.kbCursor = -1;
      clearKbSelection();
      break;
  }
});

function getVisibleRows() {
  return Array.from(document.querySelectorAll('.event-row')).filter(function(r) {
    return r.style.display !== 'none';
  });
}

function kbNavigate(dir) {
  var rows = getVisibleRows();
  if (rows.length === 0) return;
  clearKbSelection();
  state.kbCursor += dir;
  if (state.kbCursor < 0) state.kbCursor = 0;
  if (state.kbCursor >= rows.length) state.kbCursor = rows.length - 1;
  rows[state.kbCursor].classList.add('kb-selected');
  rows[state.kbCursor].scrollIntoView({block: 'nearest'});
}

function clearKbSelection() {
  document.querySelectorAll('.kb-selected').forEach(function(el) {
    el.classList.remove('kb-selected');
  });
}

// HTML export
function exportHTML() {
  var events = state.events;
  if (state.bookmarkFilter) {
    events = events.filter(function(ev) { return state.bookmarks[ev.id]; });
  }
  var html = '<!DOCTYPE html><html><head><meta charset="utf-8"><title>Dread Session Export</title>' +
    '<style>body{font-family:monospace;background:#1e1e1e;color:#abb2bf;padding:2em}' +
    'h1{color:#b5835a}table{border-collapse:collapse;width:100%}' +
    'th,td{text-align:left;padding:8px 12px;border-bottom:1px solid #333}' +
    'th{color:#c678dd;border-bottom:2px solid #555}' +
    '.success{color:#98c379}.failure{color:#e06c75}.neutral{color:#666}' +
    '.source{color:#e5c07b;font-weight:bold}' +
    'pre{background:#282c34;padding:1em;border-radius:4px;overflow-x:auto;font-size:12px}' +
    'details{margin:4px 0}summary{cursor:pointer;color:#61afef}</style></head><body>' +
    '<h1>Dread Session Export</h1>' +
    '<p>Generated: ' + new Date().toISOString() + ' — ' + events.length + ' events</p>' +
    '<table><tr><th>Time</th><th>Source</th><th>Type</th><th>Summary</th><th>Status</th><th>Payload</th></tr>';
  events.forEach(function(ev) {
    var status = classifyEvent(ev.type, ev.summary);
    var cls = status === 'success' ? 'success' : status === 'failure' ? 'failure' : 'neutral';
    html += '<tr><td>' + esc(new Date(ev.timestamp).toLocaleTimeString()) + '</td>' +
      '<td class="source">' + esc(ev.source) + '</td>' +
      '<td>' + esc(ev.type) + '</td>' +
      '<td>' + esc(ev.summary) + '</td>' +
      '<td class="' + cls + '">' + status + '</td>' +
      '<td><details><summary>payload</summary><pre>' + esc(prettyJSON(ev.raw_json)) + '</pre></details></td></tr>';
  });
  html += '</table></body></html>';
  var blob = new Blob([html], {type: 'text/html'});
  var a = document.createElement('a');
  a.href = URL.createObjectURL(blob);
  a.download = 'dread-export-' + new Date().toISOString().replace(/[:.]/g, '-').slice(0, 19) + '.html';
  a.click();
  URL.revokeObjectURL(a.href);
}

// Update stats when events change
var _origAddEvent = addEvent;
addEvent = function(ev, isLive) {
  _origAddEvent(ev, isLive);
  if (state.showStats) renderStats();
  updateBookmarkLabel();
};
</script>
</body>
</html>`

const statusPage = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<script>if(localStorage.getItem('theme')==='light')document.documentElement.classList.add('light')</script>
<link rel="icon" href="data:image/svg+xml,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 100 100'><circle cx='50' cy='50' r='40' fill='%23c37960'/></svg>">
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Press+Start+2P&display=swap">
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/geist@1.3.1/dist/fonts/geist-sans/style.min.css">
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/geist@1.3.1/dist/fonts/geist-mono/style.min.css">
<title>Webhook Status - dread.sh</title>
<script src="https://unpkg.com/lucide@0.469.0/dist/umd/lucide.min.js"></script>
<style>
  :root {
    --bg: oklch(10% 0.003 256);
    --surface: oklch(16% 0.003 256);
    --surface-hover: oklch(20% 0.003 256);
    --border: oklch(23% 0.003 256);
    --border-subtle: oklch(18% 0.003 256);
    --text: oklch(98.5% 0.003 256);
    --text-secondary: oklch(70.5% 0.003 256);
    --text-muted: oklch(55.2% 0.003 256);
    --text-dim: oklch(40% 0.003 256);
    --accent: oklch(65% 0.1 40);
    --nav-bg: oklch(10% 0.003 256 / 0.85);
    --green: oklch(72% 0.19 145);
    --yellow: oklch(82% 0.18 90);
    --red: oklch(65% 0.2 25);
    --grey: oklch(50% 0.003 256);
  }
  :root.light {
    --bg: oklch(98% 0.003 256); --surface: oklch(97% 0.003 256);
    --surface-hover: oklch(94% 0.003 256); --border: oklch(85% 0.003 256);
    --border-subtle: oklch(90% 0.003 256); --text: oklch(15% 0.003 256);
    --text-secondary: oklch(35% 0.003 256); --text-muted: oklch(50% 0.003 256);
    --text-dim: oklch(65% 0.003 256); --accent: oklch(50% 0.12 40);
    --nav-bg: oklch(98% 0.003 256 / 0.85);
    --green: oklch(45% 0.2 145); --yellow: oklch(55% 0.18 90);
    --red: oklch(50% 0.2 25); --grey: oklch(60% 0.003 256);
  }
  * { margin: 0; padding: 0; box-sizing: border-box; }
  html { font-size: 18px; }
  body {
    font-family: "Geist", -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
    background: var(--bg); color: var(--text-secondary);
    line-height: 1.6; -webkit-font-smoothing: antialiased;
  }
  code, pre { font-family: "Geist Mono", ui-monospace, monospace; }
  /*! NAV_CSS */
  .status-wrap { max-width: 920px; margin: 0 auto; padding: 80px 24px 120px; }
  .status-wrap h1 { font-size: 1.5rem; color: var(--text); font-weight: 700; margin-bottom: 4px; }
  .status-wrap .sub { font-size: 0.85rem; color: var(--text-muted); margin-bottom: 32px; }
  .status-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(280px, 1fr)); gap: 16px; }
  .status-card {
    background: var(--surface); border: 1px solid var(--border);
    border-radius: 10px; padding: 20px; transition: border-color 0.2s;
  }
  .status-card:hover { border-color: var(--accent); }
  .card-top { display: flex; align-items: center; justify-content: space-between; margin-bottom: 8px; }
  .card-name { font-size: 0.95rem; font-weight: 600; color: var(--text); }
  .card-dot { width: 10px; height: 10px; border-radius: 50%; }
  .card-dot.green { background: var(--green); box-shadow: 0 0 6px var(--green); }
  .card-dot.yellow { background: var(--yellow); box-shadow: 0 0 6px var(--yellow); }
  .card-dot.red { background: var(--red); box-shadow: 0 0 6px var(--red); }
  .card-dot.grey { background: var(--grey); }
  .card-detail { font-size: 0.78rem; color: var(--text-muted); }
  .card-summary { font-size: 0.8rem; color: var(--text-secondary); margin-top: 4px; }
  .refresh-note { font-size: 0.75rem; color: var(--text-dim); text-align: center; margin-top: 24px; }
</style>
</head>
<body>
<!-- NAV_HTML -->
<div class="status-wrap">
  <h1>Channel Status</h1>
  <p class="sub">Workspace <code>{{WORKSPACE_ID}}</code> &mdash; auto-refreshes every 30s</p>
  <div class="status-grid" id="grid"></div>
  <p class="refresh-note" id="note"></p>
</div>
<script>
const WS_ID = '{{WORKSPACE_ID}}';
function ago(ts) {
  const d = Date.now() - new Date(ts).getTime();
  if (d < 60000) return Math.floor(d/1000) + 's ago';
  if (d < 3600000) return Math.floor(d/60000) + 'm ago';
  if (d < 86400000) return Math.floor(d/3600000) + 'h ago';
  return Math.floor(d/86400000) + 'd ago';
}
function colour(ts) {
  if (!ts) return 'grey';
  const d = Date.now() - new Date(ts).getTime();
  if (d < 300000) return 'green';
  if (d < 1800000) return 'yellow';
  return 'red';
}
async function load() {
  try {
    const r = await fetch('/api/status/' + WS_ID);
    if (!r.ok) { document.getElementById('grid').innerHTML = '<p style="color:var(--text-muted)">Workspace not found</p>'; return; }
    const channels = await r.json();
    const grid = document.getElementById('grid');
    grid.innerHTML = '';
    channels.forEach(function(ch) {
      const le = ch.last_event;
      const ts = le ? le.timestamp : null;
      const c = colour(ts);
      const card = document.createElement('div');
      card.className = 'status-card';
      card.innerHTML = '<div class="card-top"><span class="card-name">' + ch.name + '</span><span class="card-dot ' + c + '"></span></div>' +
        '<div class="card-detail">' + (ts ? ago(ts) + ' &mdash; ' + (le.source || '') : 'No events yet') + '</div>' +
        (le ? '<div class="card-summary">' + le.summary + '</div>' : '');
      grid.appendChild(card);
    });
    document.getElementById('note').textContent = 'Last checked: ' + new Date().toLocaleTimeString();
  } catch(e) {
    document.getElementById('note').textContent = 'Error loading status';
  }
}
load();
setInterval(load, 30000);
function toggleTheme() {
  document.documentElement.classList.toggle('light');
  localStorage.setItem('theme', document.documentElement.classList.contains('light') ? 'light' : 'dark');
}
lucide.createIcons();
</script>
</body>
</html>`

const howToPage = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="description" content="Step-by-step guides for setting up dread with Stripe, GitHub, Sentry, and other webhook sources. Learn to forward webhooks to Slack and Discord.">
<link rel="canonical" href="https://dread.sh/howto">
<meta property="og:type" content="website">
<meta property="og:site_name" content="dread.sh">
<meta property="og:title" content="How to Set Up Webhooks - dread.sh Guides">
<meta property="og:description" content="Step-by-step guides for setting up dread with Stripe, GitHub, Sentry, and other webhook sources.">
<meta property="og:url" content="https://dread.sh/howto">
<meta property="og:image" content="https://dread.sh/og.png">
<meta name="twitter:card" content="summary_large_image">
<meta name="twitter:title" content="How to Set Up Webhooks - dread.sh Guides">
<meta name="twitter:description" content="Step-by-step guides for setting up dread with Stripe, GitHub, Sentry, and other webhook sources.">
<meta name="twitter:image" content="https://dread.sh/og.png">
<script>if(localStorage.getItem('theme')==='light')document.documentElement.classList.add('light')</script>
<link rel="icon" href="data:image/svg+xml,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 100 100'><circle cx='50' cy='50' r='40' fill='%23c37960'/></svg>">
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Press+Start+2P&display=swap">
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/geist@1.3.1/dist/fonts/geist-sans/style.min.css">
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/geist@1.3.1/dist/fonts/geist-mono/style.min.css">
<title>How to Set Up Webhooks - dread.sh Guides</title>
<script src="https://unpkg.com/lucide@0.469.0/dist/umd/lucide.min.js"></script>
<style>
  :root {
    --bg: oklch(10% 0.003 256);
    --surface: oklch(16% 0.003 256);
    --surface-hover: oklch(20% 0.003 256);
    --border: oklch(23% 0.003 256);
    --border-subtle: oklch(18% 0.003 256);
    --text: oklch(98.5% 0.003 256);
    --text-secondary: oklch(70.5% 0.003 256);
    --text-muted: oklch(55.2% 0.003 256);
    --text-dim: oklch(40% 0.003 256);
    --accent: oklch(65% 0.1 40);
    --accent-dim: oklch(47% 0.09 36);
    --accent-glow: oklch(55% 0.1 38 / 0.15);
    --accent-glow-strong: oklch(55% 0.1 38 / 0.3);
    --orange: oklch(75% 0.18 55);
    --violet: oklch(70.2% 0.183 293.54);
    --nav-bg: oklch(10% 0.003 256 / 0.85);
  }
  :root.light {
    --bg: oklch(98% 0.003 256); --surface: oklch(97% 0.003 256);
    --surface-hover: oklch(94% 0.003 256); --border: oklch(85% 0.003 256);
    --border-subtle: oklch(90% 0.003 256); --text: oklch(15% 0.003 256);
    --text-secondary: oklch(35% 0.003 256); --text-muted: oklch(50% 0.003 256);
    --text-dim: oklch(65% 0.003 256); --accent: oklch(50% 0.12 40);
    --accent-dim: oklch(40% 0.1 36); --accent-glow: oklch(50% 0.12 40 / 0.1);
    --accent-glow-strong: oklch(50% 0.12 40 / 0.2);
    --orange: oklch(55% 0.18 55); --violet: oklch(50% 0.183 293.54);
    --nav-bg: oklch(98% 0.003 256 / 0.85);
  }
  * { margin: 0; padding: 0; box-sizing: border-box; }
  html, body { overscroll-behavior: none; }
  html { font-size: 18px; }
  body {
    font-family: "Geist", -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
    background: var(--bg); color: var(--text-secondary);
    line-height: 1.6; -webkit-font-smoothing: antialiased;
  }
  code, pre, kbd { font-family: "Geist Mono", ui-monospace, "Cascadia Code", "Source Code Pro", Menlo, Consolas, monospace; }
  /*! NAV_CSS */

  .docs-layout { display: flex; min-height: 100vh; padding-top: 56px; }
  .docs-sidebar {
    width: 260px; position: fixed; top: 56px; bottom: 0; left: 0;
    border-right: 1px solid var(--border); background: var(--bg);
    overflow-y: auto; padding: 24px 0; z-index: 50;
  }
  .docs-sidebar::-webkit-scrollbar { width: 4px; }
  .docs-sidebar::-webkit-scrollbar-track { background: transparent; }
  .docs-sidebar::-webkit-scrollbar-thumb { background: var(--border); border-radius: 4px; }
  .docs-sidebar-group { margin-bottom: 8px; }
  .docs-sidebar-label {
    font-size: 0.7rem; text-transform: uppercase;
    letter-spacing: 0.08em; color: var(--text-muted);
    padding: 8px 24px 4px; font-weight: 600;
  }
  .docs-sidebar a {
    display: block; padding: 5px 24px 5px 28px;
    font-size: 0.8rem; color: var(--text-muted);
    text-decoration: none; transition: color 0.15s, background 0.15s;
    border-left: 2px solid transparent; margin-left: -1px;
  }
  .docs-sidebar a:hover { color: var(--text); }
  .docs-sidebar a.active { color: var(--accent); background: var(--accent-glow); border-left-color: var(--accent); }

  .docs-content {
    margin-left: 260px; flex: 1;
    max-width: 920px; padding: 48px 48px 120px;
  }
  .docs-section { margin-bottom: 64px; scroll-margin-top: 80px; }
  .docs-section h2 { font-size: 1.5rem; color: var(--text); font-weight: 700; letter-spacing: -0.02em; margin-bottom: 8px; }
  .docs-section h3 { font-size: 1.1rem; color: var(--text); font-weight: 600; letter-spacing: -0.01em; margin: 32px 0 12px; }
  .docs-section h3:first-child { margin-top: 0; }
  .docs-section p { font-size: 0.85rem; color: var(--text-secondary); line-height: 1.7; margin-bottom: 16px; }
  .docs-section ol, .docs-section ul { font-size: 0.85rem; color: var(--text-secondary); line-height: 1.7; margin: 0 0 16px 20px; }
  .docs-section li { margin-bottom: 4px; }
  .docs-section code { font-size: 0.8rem; background: var(--surface); padding: 2px 6px; border-radius: 4px; color: var(--accent); }
  .code-block { position: relative; }
  .code-block pre {
    background: var(--surface); border: 1px solid var(--border);
    border-radius: 8px; padding: 16px 20px; overflow-x: auto;
    font-size: 0.8rem; margin-bottom: 16px; line-height: 1.7;
  }
  .code-block pre code { background: none; padding: 0; color: var(--text); border-radius: 0; }
  .copy-btn {
    position: absolute; top: 8px; right: 8px;
    background: var(--surface-hover); border: 1px solid var(--border);
    border-radius: 6px; padding: 4px 8px; cursor: pointer;
    font-size: 0.7rem; color: var(--text-muted); transition: color 0.15s;
  }
  .copy-btn:hover { color: var(--text); }
  .docs-section .section-divider { border: none; border-top: 1px solid var(--border-subtle); margin: 32px 0; }
  .expect { background: var(--accent-glow); border: 1px solid var(--accent-glow-strong); border-radius: 8px; padding: 12px 16px; margin-bottom: 16px; font-size: 0.83rem; }

  .sidebar-overlay { display: none; }
  @media (max-width: 768px) {
    .docs-sidebar { display: none; }
    .docs-sidebar.open { display: block; width: 260px; z-index: 200; }
    .sidebar-overlay.open { display: block; position: fixed; inset: 0; z-index: 199; background: oklch(0% 0 0 / 0.5); }
    .docs-content { margin-left: 0; padding: 24px 16px 80px; }
    .docs-menu-btn { display: flex !important; }
  }
</style>
</head>
<body>
<!-- NAV_HTML -->
<div class="sidebar-overlay" id="sidebar-overlay" onclick="toggleSidebar()"></div>
<div class="docs-layout">
<aside class="docs-sidebar" id="sidebar">
  <div class="docs-sidebar-group">
    <div class="docs-sidebar-label">Getting Started</div>
    <a href="#quick-setup" class="active">Quick Setup</a>
    <a href="#first-webhook">Your First Webhook</a>
    <a href="#customise">Customise</a>
  </div>
  <div class="docs-sidebar-group">
    <div class="docs-sidebar-label">Payments</div>
    <a href="#stripe">Stripe</a>
    <a href="#paypal">PayPal</a>
    <a href="#paddle">Paddle</a>
    <a href="#shopify">Shopify</a>
    <a href="#square">Square</a>
    <a href="#razorpay">Razorpay</a>
    <a href="#recurly">Recurly</a>
    <a href="#chargebee">Chargebee</a>
    <a href="#coinbase">Coinbase Commerce</a>
    <a href="#lemonsqueezy">LemonSqueezy</a>
  </div>
  <div class="docs-sidebar-group">
    <div class="docs-sidebar-label">Developer Tools</div>
    <a href="#github">GitHub</a>
    <a href="#gitlab">GitLab</a>
    <a href="#vercel">Vercel</a>
    <a href="#sentry">Sentry</a>
    <a href="#linear">Linear</a>
    <a href="#jira">Jira</a>
    <a href="#bitbucket">Bitbucket</a>
    <a href="#circleci">CircleCI</a>
    <a href="#buildkite">Buildkite</a>
    <a href="#netlify">Netlify</a>
  </div>
  <div class="docs-sidebar-group">
    <div class="docs-sidebar-label">Infrastructure</div>
    <a href="#supabase">Supabase</a>
    <a href="#aws-sns">AWS SNS</a>
    <a href="#pagerduty">PagerDuty</a>
    <a href="#grafana">Grafana</a>
    <a href="#datadog">Datadog</a>
    <a href="#cloudflare">Cloudflare</a>
    <a href="#heroku">Heroku</a>
    <a href="#pingdom">Pingdom</a>
    <a href="#uptimerobot">UptimeRobot</a>
    <a href="#newrelic">New Relic</a>
  </div>
  <div class="docs-sidebar-group">
    <div class="docs-sidebar-label">Communication</div>
    <a href="#slack-source">Slack</a>
    <a href="#discord-source">Discord</a>
    <a href="#twilio">Twilio</a>
    <a href="#sendgrid">SendGrid</a>
    <a href="#mailchimp">Mailchimp</a>
    <a href="#zendesk">Zendesk</a>
    <a href="#intercom">Intercom</a>
    <a href="#postmark">Postmark</a>
    <a href="#mailgun">Mailgun</a>
    <a href="#telegram">Telegram</a>
  </div>
  <div class="docs-sidebar-group">
    <div class="docs-sidebar-label">SaaS</div>
    <a href="#hubspot">HubSpot</a>
    <a href="#typeform">Typeform</a>
    <a href="#clerk">Clerk</a>
    <a href="#twitch">Twitch</a>
    <a href="#zapier">Zapier</a>
    <a href="#calendly">Calendly</a>
    <a href="#docusign">DocuSign</a>
    <a href="#auth0">Auth0</a>
    <a href="#launchdarkly">LaunchDarkly</a>
    <a href="#figma">Figma</a>
    <a href="#salesforce">Salesforce</a>
    <a href="#notion">Notion</a>
    <a href="#airtable">Airtable</a>
    <a href="#asana">Asana</a>
    <a href="#webflow">Webflow</a>
    <a href="#custom-source">Custom / Other</a>
  </div>
</aside>

<main class="docs-content">

<!-- GETTING STARTED -->
<section id="quick-setup" class="docs-section">
<h2>Quick Setup</h2>
<p>Get dread running in under a minute.</p>

<h3>1. Install dread</h3>
<div class="code-block"><pre><code>curl -fsSL https://dread.sh/install | sh</code></pre><button class="copy-btn" onclick="copyCode(this)">Copy</button></div>

<h3>2. Create a channel</h3>
<p>A channel is where webhooks get sent. Create one for each service (e.g. Stripe, GitHub):</p>
<div class="code-block"><pre><code>dread new "Stripe Prod"</code></pre><button class="copy-btn" onclick="copyCode(this)">Copy</button></div>
<p>This prints your webhook URL. <strong>This is the URL you paste into other services:</strong></p>
<div class="code-block"><pre><code>https://dread.sh/wh/ch_stripe-prod_a1b2c3</code></pre><button class="copy-btn" onclick="copyCode(this)">Copy</button></div>
<p>Every channel gets its own unique URL. Copy it &mdash; you'll paste it in the next step.</p>

<h3>3. Paste the URL into your service</h3>
<p>Go to whatever service you want to monitor (Stripe, GitHub, Vercel, etc.), find its webhook settings, and paste the URL from step 2. Each guide below shows you exactly where.</p>

<h3>4. Start watching</h3>
<div class="code-block"><pre><code>dread</code></pre><button class="copy-btn" onclick="copyCode(this)">Copy</button></div>
<div class="expect">You'll see the dread TUI with your channel listed, waiting for events. When the service sends a webhook, it appears here instantly with a desktop notification.</div>
</section>

<section id="first-webhook" class="docs-section">
<h2>Your First Webhook</h2>
<p>Don't have a service ready yet? Send a test event to make sure everything works:</p>
<div class="code-block"><pre><code>dread test ch_stripe-prod_a1b2c3</code></pre><button class="copy-btn" onclick="copyCode(this)">Copy</button></div>
<p>Or send a real HTTP request from anywhere:</p>
<div class="code-block"><pre><code>curl -X POST https://dread.sh/wh/ch_stripe-prod_a1b2c3 \
  -H "Content-Type: application/json" \
  -d '{"event":"test","message":"Hello from curl"}'</code></pre><button class="copy-btn" onclick="copyCode(this)">Copy</button></div>
<div class="expect">You'll see the event appear in the TUI and get a desktop notification. If this works, you're ready to connect a real service below.</div>
</section>

<section id="customise" class="docs-section">
<h2>Customise Channel Name, Source &amp; Summary</h2>

<h3>Change the channel name</h3>
<p>The channel name is set when you create it with <code>dread new</code>. To rename it, remove and re-add with the new name:</p>
<div class="code-block"><pre><code>dread remove ch_old-name_abc123
dread add ch_old-name_abc123 "New Display Name"</code></pre><button class="copy-btn" onclick="copyCode(this)">Copy</button></div>
<p>The channel ID and webhook URL stay the same &mdash; only the display name changes in the TUI, notifications, and dashboard.</p>

<h3>Change the source label</h3>
<p>dread auto-detects the source from HTTP headers (Stripe-Signature, X-GitHub-Event, etc.). For services that aren't auto-detected, add <code>?source=name</code> to your webhook URL:</p>
<div class="code-block"><pre><code>https://dread.sh/wh/ch_xxx?source=trigger.dev</code></pre><button class="copy-btn" onclick="copyCode(this)">Copy</button></div>
<p>Just paste the URL with <code>?source=</code> into your service's webhook settings &mdash; no custom headers needed. You can also use the <code>X-Dread-Source</code> header for programmatic control.</p>
<p>The source label controls how events are grouped in the dashboard and which colour appears in the TUI. Auto-detected sources (stripe, github, vercel, sentry, etc.) always take priority.</p>

<h3>Change the summary text</h3>
<p>The summary is extracted automatically from known payload formats. For custom services, dread looks for these JSON fields in order:</p>
<ol>
<li><code>type</code>, <code>event</code>, <code>event_type</code>, <code>action</code>, <code>topic</code> &mdash; used as the event type</li>
<li><code>message</code>, <code>description</code>, <code>summary</code>, <code>text</code>, <code>status</code> &mdash; used as the summary text</li>
</ol>
<p>To control what appears in notifications, structure your JSON payload with these fields:</p>
<div class="code-block"><pre><code>curl -X POST "https://dread.sh/wh/ch_xxx?source=deploy-bot" \
  -H "Content-Type: application/json" \
  -d '{"type":"deploy.success","message":"v2.1.0 deployed to production"}'</code></pre><button class="copy-btn" onclick="copyCode(this)">Copy</button></div>
<div class="expect">This will show as: source <strong>deploy-bot</strong>, type <strong>deploy.success</strong>, summary <strong>deploy-bot &mdash; v2.1.0 deployed to production</strong></div>
</section>

<hr class="section-divider">

<!-- PAYMENTS -->
<section id="stripe" class="docs-section">
<h2>Stripe</h2>
<ol>
<li>Open <strong>Stripe Dashboard</strong> &rarr; click <strong>Developers</strong> in the top nav</li>
<li>Select <strong>Webhooks</strong> from the submenu</li>
<li>Click <strong>Create an event destination</strong></li>
<li>Select <strong>Account</strong> as the event source, then choose <strong>Webhook endpoint</strong></li>
<li>Paste your dread webhook URL in <strong>Endpoint URL</strong></li>
<li>Select the events you want (e.g. <code>payment_intent.succeeded</code>, <code>charge.failed</code>) or choose all events for testing</li>
<li>Click <strong>Add endpoint</strong></li>
</ol>
<div class="expect">Stripe events will show parsed summaries with amounts (e.g. <strong>payment_intent.succeeded &mdash; $49.99 USD</strong>), subscription statuses, and customer emails.</div>
</section>

<section id="paypal" class="docs-section">
<h2>PayPal</h2>
<ol>
<li>Log in to <strong>developer.paypal.com/dashboard</strong></li>
<li>Go to <strong>Apps &amp; Credentials</strong> and select your app (or create one)</li>
<li>Open the <strong>Webhooks</strong> section</li>
<li>Click <strong>Add Webhook</strong></li>
<li>Paste your dread webhook URL (must be HTTPS)</li>
<li>Select event types (e.g. <code>PAYMENT.CAPTURE.COMPLETED</code>, <code>BILLING.SUBSCRIPTION.ACTIVATED</code>)</li>
<li>Click <strong>Save</strong></li>
</ol>
<div class="expect">Payment events will show amounts and statuses (e.g. <strong>PAYMENT.CAPTURE.COMPLETED &mdash; 29.99 USD</strong>).</div>
</section>

<section id="paddle" class="docs-section">
<h2>Paddle</h2>
<ol>
<li>Open <strong>Paddle Dashboard</strong> &rarr; <strong>Developer Tools</strong> &rarr; <strong>Notifications</strong></li>
<li>Click <strong>+ New destination</strong></li>
<li>Select <strong>Webhook</strong> as the type</li>
<li>Paste your dread webhook URL</li>
<li>Select events (subscriptions, transactions, customers, adjustments)</li>
<li>Click <strong>Save destination</strong></li>
</ol>
<div class="expect">Paddle events will show the event type and status (e.g. <strong>subscription.created &mdash; active</strong>).</div>
</section>

<section id="shopify" class="docs-section">
<h2>Shopify</h2>
<ol>
<li>Open <strong>Shopify Admin</strong> &rarr; <strong>Settings</strong> (bottom left) &rarr; <strong>Notifications</strong></li>
<li>Scroll down to <strong>Webhooks</strong> and click <strong>Create webhook</strong></li>
<li>Select an event topic from the dropdown (e.g. <code>orders/create</code>)</li>
<li>Set format to <strong>JSON</strong></li>
<li>Paste your dread webhook URL</li>
<li>Click <strong>Save</strong></li>
</ol>
<p>Note: you can't change the event topic after creation &mdash; create a new webhook for each topic you need.</p>
<div class="expect">Order events will show order numbers and totals (e.g. <strong>orders/create &mdash; order #1042 ($79.00)</strong>).</div>
</section>

<section id="square" class="docs-section">
<h2>Square</h2>
<ol>
<li>Log in to <strong>developer.squareup.com</strong> &rarr; open your application</li>
<li>Click <strong>Webhooks</strong> in the left sidebar</li>
<li>Click <strong>Add subscription</strong></li>
<li>Enter a name for the subscription</li>
<li>Paste your dread webhook URL in the <strong>URL</strong> field</li>
<li>Select event types: <code>payment.completed</code>, <code>order.created</code>, <code>refund.created</code>, <code>invoice.published</code>, etc.</li>
<li>Click <strong>Save</strong></li>
</ol>
<p>Use the <strong>Send test event</strong> button to verify. Square signs all webhooks with a signature in the <code>x-square-hmacsha256-signature</code> header.</p>
<div class="expect">Payment events show the amount and status (e.g. <strong>payment.completed &mdash; $45.00 GBP</strong>).</div>
</section>

<section id="razorpay" class="docs-section">
<h2>Razorpay</h2>
<ol>
<li>Log in to the <strong>Razorpay Dashboard</strong></li>
<li>Go to <strong>Account &amp; Settings</strong> &rarr; <strong>Webhooks</strong></li>
<li>Click <strong>Add New Webhook</strong></li>
<li>Paste your dread webhook URL</li>
<li>Enter a <strong>Secret</strong> (used to verify webhook signatures)</li>
<li>Select events: <code>payment.captured</code>, <code>payment.failed</code>, <code>order.paid</code>, <code>subscription.activated</code>, <code>refund.processed</code>, etc.</li>
<li>Click <strong>Create Webhook</strong></li>
</ol>
<p>Razorpay sends events in both test and live modes. Toggle between modes in the dashboard to configure webhooks for each.</p>
<div class="expect">Payment events show the event type and amount (e.g. <strong>payment.captured &mdash; ₹2,499 INR</strong>).</div>
</section>

<section id="recurly" class="docs-section">
<h2>Recurly</h2>
<ol>
<li>Log in to the <strong>Recurly Dashboard</strong></li>
<li>Go to <strong>Integrations</strong> &rarr; <strong>Webhooks</strong></li>
<li>Click <strong>Configure</strong></li>
<li>Click <strong>New Endpoint</strong></li>
<li>Paste your dread webhook URL</li>
<li>Select notification types: <code>new_subscription</code>, <code>successful_payment</code>, <code>failed_payment</code>, <code>canceled_subscription</code>, etc.</li>
<li>Click <strong>Save</strong></li>
</ol>
<p>Recurly sends XML payloads by default. dread will detect the source via the <code>X-Recurly-Notification-Type</code> header.</p>
<div class="expect">Subscription events show the notification type (e.g. <strong>successful_payment &mdash; invoice 1234</strong>).</div>
</section>

<section id="chargebee" class="docs-section">
<h2>Chargebee</h2>
<ol>
<li>Log in to <strong>Chargebee</strong> &rarr; go to <strong>Settings</strong> &rarr; <strong>Configure Chargebee</strong></li>
<li>Select <strong>Webhooks</strong> under API &amp; Webhooks</li>
<li>Click <strong>Add Webhook</strong></li>
<li>Paste your dread webhook URL</li>
<li>Select <strong>V2</strong> API version</li>
<li>Choose event types: <code>subscription_created</code>, <code>payment_succeeded</code>, <code>payment_failed</code>, <code>invoice_generated</code>, etc.</li>
<li>Click <strong>Create</strong></li>
</ol>
<p>Use the <strong>Test Webhook</strong> button to send a sample event. Chargebee retries failed deliveries automatically.</p>
<div class="expect">Chargebee events show the event type and object (e.g. <strong>subscription_created &mdash; plan Enterprise</strong>).</div>
</section>

<section id="coinbase" class="docs-section">
<h2>Coinbase Commerce</h2>
<ol>
<li>Log in to <strong>commerce.coinbase.com</strong></li>
<li>Go to <strong>Settings</strong> &rarr; <strong>Webhook subscriptions</strong></li>
<li>Click <strong>Add an endpoint</strong></li>
<li>Paste your dread webhook URL</li>
<li>Click <strong>Save</strong></li>
<li>Copy the <strong>Shared Secret</strong> displayed (used for signature verification)</li>
</ol>
<p>Coinbase Commerce sends events for charges: <code>charge:created</code>, <code>charge:confirmed</code>, <code>charge:failed</code>, <code>charge:pending</code>, <code>charge:resolved</code>.</p>
<div class="expect">Charge events show the event type and amount (e.g. <strong>charge:confirmed &mdash; 0.005 BTC</strong>).</div>
</section>

<section id="lemonsqueezy" class="docs-section">
<h2>LemonSqueezy</h2>
<ol>
<li>Log in to <strong>app.lemonsqueezy.com</strong></li>
<li>Go to <strong>Settings</strong> &rarr; <strong>Webhooks</strong></li>
<li>Click <strong>Add webhook</strong> (the <strong>+</strong> button)</li>
<li>Paste your dread webhook URL in the <strong>Callback URL</strong> field</li>
<li>Enter a <strong>Signing secret</strong></li>
<li>Select events: <code>order_created</code>, <code>subscription_created</code>, <code>subscription_payment_success</code>, <code>license_key_created</code>, etc.</li>
<li>Click <strong>Save webhook</strong></li>
</ol>
<p>LemonSqueezy signs payloads with HMAC-SHA256 using the signing secret in the <code>X-Signature</code> header.</p>
<div class="expect">Order events show the event type and product (e.g. <strong>order_created &mdash; Pro Plan ($49.00)</strong>).</div>
</section>

<hr class="section-divider">

<!-- DEVELOPER TOOLS -->
<section id="github" class="docs-section">
<h2>GitHub</h2>
<ol>
<li>Go to your repository &rarr; <strong>Settings</strong> tab &rarr; <strong>Webhooks</strong> in the left sidebar</li>
<li>Click <strong>Add webhook</strong></li>
<li>Paste your dread webhook URL in <strong>Payload URL</strong></li>
<li>Set <strong>Content type</strong> to <code>application/json</code></li>
<li>Under "Which events would you like to trigger this webhook?" select <strong>Let me select individual events</strong> and check the ones you want, or choose <strong>Send me everything</strong></li>
<li>Make sure <strong>Active</strong> is checked and click <strong>Add webhook</strong></li>
</ol>
<p>GitHub sends a <code>ping</code> event to confirm the setup.</p>
<div class="expect">Push events show branch and commit message. PRs show number and title. Stars show who starred. (e.g. <strong>push to main by alice &mdash; fix login bug</strong>)</div>
</section>

<section id="gitlab" class="docs-section">
<h2>GitLab</h2>
<ol>
<li>Go to your project &rarr; <strong>Settings</strong> &rarr; <strong>Webhooks</strong></li>
<li>Click <strong>Add new webhook</strong></li>
<li>Paste your dread webhook URL in the <strong>URL</strong> field</li>
<li>Select trigger events: <strong>Push events</strong>, <strong>Merge request events</strong>, <strong>Issues events</strong>, <strong>Pipeline events</strong>, etc.</li>
<li>Click <strong>Add webhook</strong></li>
</ol>
<p>Requires Maintainer or Owner role. Use the <strong>Test</strong> dropdown to send a test event.</p>
<div class="expect">Push events show branch and author. MRs show title and action. Pipelines show status and ref. (e.g. <strong>MR opened &mdash; Add dark mode support</strong>)</div>
</section>

<section id="vercel" class="docs-section">
<h2>Vercel</h2>
<p>Requires Pro or Enterprise plan. Webhooks are configured at the team level, not per project.</p>
<ol>
<li>Go to your <strong>Team Settings</strong> &rarr; <strong>Webhooks</strong></li>
<li>Click <strong>Create Webhook</strong></li>
<li>Select events: Deployment (created, succeeded, error), Project (created, removed), etc.</li>
<li>Choose target projects (all or specific ones)</li>
<li>Paste your dread webhook URL</li>
<li>Click <strong>Create Webhook</strong> and copy the secret key shown (it's only displayed once)</li>
</ol>
<div class="expect">Deployment events show the project name (e.g. <strong>deployment.succeeded &mdash; my-app</strong>).</div>
</section>

<section id="sentry" class="docs-section">
<h2>Sentry</h2>
<ol>
<li>Go to <strong>Settings</strong> &rarr; <strong>Developer Settings</strong></li>
<li>Click <strong>Create New Integration</strong> &rarr; select <strong>Internal Integration</strong></li>
<li>Enter a name for the integration</li>
<li>Paste your dread webhook URL in the <strong>Webhook URL</strong> field</li>
<li>Set Permissions: <strong>Issue &amp; Event</strong> to Read</li>
<li>Under Webhooks, check: <strong>Issue</strong>, <strong>Error</strong>, and any other events you want</li>
<li>Click <strong>Save Changes</strong> &mdash; the integration installs automatically on your org</li>
</ol>
<div class="expect">Error and issue events show the issue title (e.g. <strong>issue.created &mdash; TypeError: Cannot read property 'map' of undefined</strong>).</div>
</section>

<section id="linear" class="docs-section">
<h2>Linear</h2>
<ol>
<li>Click your profile icon &rarr; <strong>Settings</strong></li>
<li>Go to <strong>API</strong> (under Administration)</li>
<li>Click <strong>New webhook</strong></li>
<li>Paste your dread webhook URL (must be publicly accessible HTTPS)</li>
<li>Add a label and select resources: Issues, Comments, Projects, Cycles, etc.</li>
<li>Save the webhook</li>
</ol>
<p>Only workspace admins can create webhooks.</p>
<div class="expect">Issue events show the identifier and title (e.g. <strong>Issue updated &mdash; ENG-142 Fix onboarding flow</strong>).</div>
</section>

<section id="jira" class="docs-section">
<h2>Jira</h2>
<p>Requires the <strong>Administer Jira</strong> global permission.</p>
<ol>
<li>Click the gear icon &rarr; <strong>System</strong></li>
<li>Go to <strong>Advanced</strong> &rarr; <strong>WebHooks</strong></li>
<li>Click <strong>Create a WebHook</strong></li>
<li>Enter a name and paste your dread webhook URL</li>
<li>Select events (e.g. <code>jira:issue_created</code>, <code>jira:issue_updated</code>, <code>comment_created</code>)</li>
<li>Optionally add a JQL filter to limit which issues trigger the webhook (e.g. <code>project = PROD</code>)</li>
<li>Click <strong>Save</strong></li>
</ol>
<div class="expect">Issue events show the key and summary (e.g. <strong>jira:issue_updated &mdash; PROD-245 Update payment timeout</strong>).</div>
</section>

<section id="bitbucket" class="docs-section">
<h2>Bitbucket</h2>
<ol>
<li>Open your repository in Bitbucket</li>
<li>Go to <strong>Repository settings</strong> &rarr; <strong>Webhooks</strong></li>
<li>Click <strong>Add webhook</strong></li>
<li>Enter a title and paste your dread webhook URL</li>
<li>Under <strong>Triggers</strong>, select events: push, pull request (created, updated, merged, declined), issue events, etc.</li>
<li>Click <strong>Save</strong></li>
</ol>
<p>Bitbucket sends a unique <code>X-Hook-UUID</code> header with each delivery. Use <strong>View requests</strong> on the webhook to see delivery history.</p>
<div class="expect">Push events show the branch and author. PRs show the title and action (e.g. <strong>pullrequest:created &mdash; Add dark mode</strong>).</div>
</section>

<section id="circleci" class="docs-section">
<h2>CircleCI</h2>
<ol>
<li>Open your project in <strong>CircleCI</strong></li>
<li>Go to <strong>Project Settings</strong> &rarr; <strong>Webhooks</strong></li>
<li>Click <strong>Add Webhook</strong></li>
<li>Enter a name for the webhook</li>
<li>Paste your dread webhook URL in the <strong>Receiver URL</strong> field</li>
<li>Select events: <code>workflow-completed</code> and/or <code>job-completed</code></li>
<li>Click <strong>Save</strong></li>
</ol>
<p>CircleCI includes a <code>circleci-signature</code> header for verification. Payloads include pipeline, workflow, and job details.</p>
<div class="expect">Workflow events show the status and name (e.g. <strong>workflow-completed &mdash; build-and-deploy (success)</strong>).</div>
</section>

<section id="buildkite" class="docs-section">
<h2>Buildkite</h2>
<ol>
<li>Go to your <strong>Organisation Settings</strong> &rarr; <strong>Notification Services</strong></li>
<li>Click <strong>Add</strong> next to <strong>Webhook</strong></li>
<li>Paste your dread webhook URL</li>
<li>Select events: <code>build.scheduled</code>, <code>build.running</code>, <code>build.finished</code>, <code>job.started</code>, <code>agent.connected</code>, etc.</li>
<li>Optionally filter by pipeline</li>
<li>Click <strong>Add Webhook Notification</strong></li>
</ol>
<p>Buildkite sends a <code>X-Buildkite-Event</code> header with every delivery. Use the <strong>Send test</strong> button to verify.</p>
<div class="expect">Build events show the pipeline and status (e.g. <strong>build.finished &mdash; deploy-prod (passed)</strong>).</div>
</section>

<section id="netlify" class="docs-section">
<h2>Netlify</h2>
<ol>
<li>Open your site in <strong>Netlify</strong></li>
<li>Go to <strong>Site configuration</strong> &rarr; <strong>Notifications</strong></li>
<li>Click <strong>Add notification</strong> &rarr; <strong>Outgoing webhook</strong></li>
<li>Select an event: <code>Deploy started</code>, <code>Deploy succeeded</code>, <code>Deploy failed</code>, <code>Form submission</code>, etc.</li>
<li>Paste your dread webhook URL</li>
<li>Click <strong>Save</strong></li>
</ol>
<p>Note: Netlify creates one notification per event type. Repeat the steps for each event you want to receive.</p>
<div class="expect">Deploy events show the site name and status (e.g. <strong>deploy succeeded &mdash; my-app.netlify.app</strong>).</div>
</section>

<hr class="section-divider">

<!-- INFRASTRUCTURE -->
<section id="aws-sns" class="docs-section">
<h2>AWS SNS</h2>
<ol>
<li>Open the <strong>SNS console</strong> &rarr; <strong>Topics</strong> &rarr; <strong>Create topic</strong></li>
<li>Choose <strong>Standard</strong>, enter a name, and click <strong>Create topic</strong></li>
<li>Select your topic, then click <strong>Create subscription</strong></li>
<li>Set Protocol to <strong>HTTPS</strong></li>
<li>Paste your dread webhook URL as the endpoint</li>
<li>Click <strong>Create subscription</strong></li>
<li>dread will receive a <code>SubscriptionConfirmation</code> message &mdash; the subscription auto-confirms when dread responds with 200</li>
</ol>
<p>Check the SNS console Subscriptions page &mdash; status should change from PendingConfirmation to Confirmed.</p>
<div class="expect">Notifications show the subject or message content (e.g. <strong>notification &mdash; Auto Scaling: instance i-0abc123 terminated</strong>).</div>
</section>

<section id="pagerduty" class="docs-section">
<h2>PagerDuty</h2>
<ol>
<li>Go to <strong>Integrations</strong> &rarr; <strong>Generic Webhooks (v3)</strong></li>
<li>Click <strong>New Webhook</strong></li>
<li>Paste your dread webhook URL</li>
<li>Select event subscriptions: incident events (triggered, acknowledged, resolved, escalated) and/or service events</li>
<li>Optionally add a description and custom headers</li>
<li>Click <strong>Add Webhook</strong> and save the generated webhook secret</li>
</ol>
<p>Requires Manager, Admin, or Account Owner role. Max 10 webhook subscriptions per scope.</p>
<div class="expect">Incident events show the incident title (e.g. <strong>incident.triggered &mdash; High CPU on web-prod-3</strong>).</div>
</section>

<section id="supabase" class="docs-section">
<h2>Supabase</h2>
<p>Supabase Database Webhooks fire when rows are inserted, updated, or deleted in your tables.</p>
<ol>
<li>Open your project in the <strong>Supabase Dashboard</strong></li>
<li>Go to <strong>Database</strong> &rarr; <strong>Webhooks</strong> in the sidebar</li>
<li>Click <strong>Create a new hook</strong></li>
<li>Name your hook (e.g. <code>new-booking</code>) and select the <strong>table</strong> to watch</li>
<li>Tick the events to fire on: <strong>Insert</strong>, <strong>Update</strong>, and/or <strong>Delete</strong></li>
<li>Under <strong>Hook type</strong>, select <strong>HTTP Request</strong></li>
<li>Set Method to <strong>POST</strong> and paste your dread webhook URL</li>
<li>Add the header <code>Content-Type: application/json</code></li>
<li>Click <strong>Create webhook</strong></li>
</ol>
<p>The payload includes the <code>type</code> (INSERT/UPDATE/DELETE), the <code>table</code> name, <code>record</code> (new row data), and <code>old_record</code> (previous row data for updates/deletes).</p>
<div class="expect">Supabase events are detected via the <code>X-Supabase-Event-Signature</code> header and show the table name and operation type.</div>
</section>

<section id="grafana" class="docs-section">
<h2>Grafana</h2>
<ol>
<li>Open <strong>Grafana</strong> &rarr; go to <strong>Alerting</strong> &rarr; <strong>Contact points</strong></li>
<li>Click <strong>Add contact point</strong></li>
<li>Enter a name and select <strong>Webhook</strong> as the integration type</li>
<li>Paste your dread webhook URL</li>
<li>Set HTTP method to <strong>POST</strong></li>
<li>Optionally add a <code>username</code> and <code>password</code> for basic auth</li>
<li>Click <strong>Save contact point</strong></li>
<li>Go to <strong>Notification policies</strong> and route alerts to your new contact point</li>
</ol>
<p>Grafana sends alerts with labels, annotations, and the alert state (firing or resolved).</p>
<div class="expect">Alert events show the alert name and status (e.g. <strong>alerting &mdash; High CPU Usage [FIRING]</strong>).</div>
</section>

<section id="datadog" class="docs-section">
<h2>Datadog</h2>
<ol>
<li>Log in to <strong>Datadog</strong></li>
<li>Go to <strong>Integrations</strong> &rarr; <strong>Webhooks</strong></li>
<li>Click <strong>New</strong> (or <strong>+ New</strong>)</li>
<li>Enter a name for the webhook</li>
<li>Paste your dread webhook URL</li>
<li>Optionally customise the <strong>Payload</strong> JSON template using Datadog variables (<code>$EVENT_TITLE</code>, <code>$ALERT_STATUS</code>, etc.)</li>
<li>Click <strong>Save</strong></li>
<li>Use the webhook in monitor notifications by adding <code>@webhook-your-name</code> to the alert message</li>
</ol>
<p>The webhook only fires when referenced in a monitor &mdash; it won't send events on its own.</p>
<div class="expect">Monitor events show the alert title and status (e.g. <strong>webhook &mdash; CPU above 90% on web-prod [Triggered]</strong>).</div>
</section>

<section id="cloudflare" class="docs-section">
<h2>Cloudflare</h2>
<ol>
<li>Log in to the <strong>Cloudflare Dashboard</strong></li>
<li>Go to <strong>Notifications</strong> in the left sidebar</li>
<li>Click the <strong>Destinations</strong> tab</li>
<li>Under <strong>Webhooks</strong>, click <strong>Create</strong></li>
<li>Enter a name and paste your dread webhook URL</li>
<li>Click <strong>Save and Test</strong> &mdash; Cloudflare sends a verification request</li>
<li>Go to the <strong>Notifications</strong> tab and create a notification</li>
<li>Select an alert type (e.g. <code>Origin Error Rate Alert</code>, <code>DDoS Attack Alert</code>, <code>SSL Certificate Expiration</code>)</li>
<li>Choose the webhook as the delivery destination</li>
<li>Click <strong>Save</strong></li>
</ol>
<div class="expect">Alert events show the notification type and zone (e.g. <strong>cloudflare &mdash; Origin 5xx Error Rate Alert for example.com</strong>).</div>
</section>

<section id="heroku" class="docs-section">
<h2>Heroku</h2>
<p>Heroku uses the <strong>app-webhooks</strong> add-on for webhook notifications.</p>
<ol>
<li>Install the CLI plugin: <code>heroku plugins:install @heroku-cli/plugin-webhooks</code></li>
<li>Create a webhook:</li>
</ol>
<div class="code-block"><pre><code>heroku webhooks:add \
  --app your-app-name \
  --url https://dread.sh/wh/ch_xxx \
  --include api:release api:build api:dyno \
  --level notify</code></pre><button class="copy-btn" onclick="copyCode(this)">Copy</button></div>
<ol start="3">
<li>Verify with <code>heroku webhooks --app your-app-name</code></li>
</ol>
<p>Events include <code>api:release</code>, <code>api:build</code>, <code>api:dyno</code>, <code>api:formation</code>, and <code>api:addon</code>. Set <code>--level sync</code> for guaranteed delivery (slower) or <code>--level notify</code> for best-effort (faster).</p>
<div class="expect">Release events show the action and version (e.g. <strong>api:release &mdash; v42 deployed</strong>).</div>
</section>

<section id="pingdom" class="docs-section">
<h2>Pingdom</h2>
<ol>
<li>Log in to <strong>my.pingdom.com</strong></li>
<li>Go to <strong>Settings</strong> &rarr; <strong>Integrations</strong></li>
<li>Click <strong>Add integration</strong></li>
<li>Select <strong>Webhook</strong> as the type</li>
<li>Enter a name and paste your dread webhook URL</li>
<li>Click <strong>Save integration</strong></li>
<li>Go to <strong>Monitoring</strong> &rarr; select a check &rarr; <strong>Edit</strong></li>
<li>Under <strong>Connect integrations</strong>, select your webhook</li>
<li>Click <strong>Save</strong></li>
</ol>
<p>Pingdom sends alerts when checks go up or down, including the check name, status, and response time.</p>
<div class="expect">Uptime events show the check name and status (e.g. <strong>pingdom &mdash; api.example.com is DOWN</strong>).</div>
</section>

<section id="uptimerobot" class="docs-section">
<h2>UptimeRobot</h2>
<ol>
<li>Log in to <strong>uptimerobot.com</strong></li>
<li>Go to <strong>My Settings</strong> (top right)</li>
<li>Scroll to <strong>Alert Contacts</strong> and click <strong>Add Alert Contact</strong></li>
<li>Select <strong>Webhook</strong> as the alert contact type</li>
<li>Paste your dread webhook URL</li>
<li>Select <strong>POST</strong> as the method</li>
<li>Optionally enable <strong>Send as JSON</strong> (recommended)</li>
<li>Click <strong>Create Alert Contact</strong></li>
<li>Go to your monitor and add the new alert contact under <strong>Alert Contacts To Notify</strong></li>
</ol>
<div class="expect">Monitor events show the monitor name and status (e.g. <strong>webhook &mdash; api.example.com is down</strong>).</div>
</section>

<section id="newrelic" class="docs-section">
<h2>New Relic</h2>
<ol>
<li>Log in to <strong>New Relic</strong></li>
<li>Go to <strong>Alerts &amp; AI</strong> &rarr; <strong>Destinations</strong></li>
<li>Click <strong>Add a destination</strong> &rarr; select <strong>Webhook</strong></li>
<li>Enter a name and paste your dread webhook URL</li>
<li>Click <strong>Save destination</strong></li>
<li>Go to <strong>Workflows</strong> &rarr; <strong>Add a workflow</strong></li>
<li>Set your filter (e.g. policy name, priority, condition)</li>
<li>Under <strong>Notify</strong>, select <strong>Webhook</strong> and choose your destination</li>
<li>Customise the payload template if needed</li>
<li>Click <strong>Activate workflow</strong></li>
</ol>
<p>New Relic sends alert notifications with issue details, condition name, and violation details.</p>
<div class="expect">Alert events show the condition and status (e.g. <strong>webhook &mdash; High Error Rate [ACTIVATED]</strong>).</div>
</section>

<hr class="section-divider">

<!-- COMMUNICATION -->
<section id="slack-source" class="docs-section">
<h2>Slack (as event source)</h2>
<p>This receives events <em>from</em> Slack into dread. (For forwarding dread events <em>to</em> Slack, see <a href="/docs#slack-discord-fwd" style="color:var(--violet)">Docs &rarr; Slack/Discord Forwarding</a>.)</p>
<ol>
<li>Go to <strong>api.slack.com/apps</strong> &rarr; <strong>Create New App</strong> &rarr; <strong>From scratch</strong></li>
<li>Name your app and select your workspace</li>
<li>In the sidebar, click <strong>Event Subscriptions</strong> and toggle it on</li>
<li>Paste your dread webhook URL in <strong>Request URL</strong> &mdash; Slack sends a verification challenge that dread handles automatically</li>
<li>Under <strong>Subscribe to bot events</strong>, add events like <code>message.channels</code>, <code>app_mention</code>, etc.</li>
<li>Go to <strong>OAuth &amp; Permissions</strong> and add the required scopes</li>
<li>Click <strong>Install App</strong> to install to your workspace</li>
</ol>
<div class="expect">Slack events show the event type and message text (e.g. <strong>message &mdash; Hey team, deploy is live</strong>).</div>
</section>

<section id="discord-source" class="docs-section">
<h2>Discord (as event source)</h2>
<p>This receives interactions from Discord into dread.</p>
<ol>
<li>Go to <strong>discord.com/developers/applications</strong> &rarr; <strong>New Application</strong></li>
<li>Navigate to the <strong>General Information</strong> tab</li>
<li>Paste your dread webhook URL in <strong>Interactions Endpoint URL</strong></li>
<li>Click <strong>Save Changes</strong> &mdash; Discord will send test PING requests to verify your endpoint</li>
</ol>
<p>Your endpoint must handle the PING verification (dread does this automatically). Discord sends all interactions (slash commands, buttons) as HTTP POST requests.</p>
<div class="expect">Discord interactions show the type (e.g. <strong>command &mdash; /deploy</strong>).</div>
</section>

<section id="twilio" class="docs-section">
<h2>Twilio</h2>
<ol>
<li>Log in to <strong>console.twilio.com</strong></li>
<li>Go to <strong>Phone Numbers</strong> &rarr; <strong>Active Numbers</strong></li>
<li>Click on the phone number you want to configure</li>
<li>Scroll to the <strong>Messaging</strong> section</li>
<li>Under "A Message Comes In", select <strong>Webhook</strong></li>
<li>Paste your dread webhook URL and set the method to <strong>POST</strong></li>
<li>Click <strong>Save</strong></li>
</ol>
<p>For Messaging Services: go to <strong>Messaging Services</strong> &rarr; select your service &rarr; <strong>Integration</strong> tab &rarr; enter the Inbound Request URL.</p>
<div class="expect">Twilio events show as generic webhook events with the message body from the SMS/call.</div>
</section>

<section id="sendgrid" class="docs-section">
<h2>SendGrid</h2>
<ol>
<li>Log in to <strong>SendGrid</strong> &rarr; go to <strong>Settings</strong> &rarr; <strong>Mail Settings</strong></li>
<li>Select <strong>Event Webhooks</strong></li>
<li>Click <strong>Create new webhook</strong></li>
<li>Paste your dread webhook URL in the <strong>Post URL</strong> field</li>
<li>Select event types: <strong>processed</strong>, <strong>delivered</strong>, <strong>bounced</strong>, <strong>open</strong>, <strong>click</strong>, <strong>spam report</strong>, etc.</li>
<li>Toggle <strong>Enabled</strong> on</li>
<li>Click <strong>Save</strong></li>
</ol>
<p>Use <strong>Test Your Integration</strong> to send a sample payload and verify.</p>
<div class="expect">SendGrid events show as generic webhook events with delivery status information.</div>
</section>

<section id="mailchimp" class="docs-section">
<h2>Mailchimp</h2>
<ol>
<li>Log in to <strong>Mailchimp</strong></li>
<li>Go to your <strong>Audience</strong> &rarr; <strong>Settings</strong> &rarr; <strong>Webhooks</strong></li>
<li>Click <strong>Create New Webhook</strong></li>
<li>Paste your dread webhook URL in the <strong>Callback URL</strong> field</li>
<li>Select events: <code>subscribes</code>, <code>unsubscribes</code>, <code>profile updates</code>, <code>cleaned</code>, <code>email changed</code>, <code>campaign sent</code></li>
<li>Choose whether to fire for API-initiated changes, admin changes, or subscriber-initiated changes</li>
<li>Click <strong>Save</strong></li>
</ol>
<p>Mailchimp sends form-encoded POST data (not JSON). dread handles both formats automatically.</p>
<div class="expect">Audience events show the event type and email (e.g. <strong>mailchimp &mdash; subscribe user@example.com</strong>).</div>
</section>

<section id="zendesk" class="docs-section">
<h2>Zendesk</h2>
<p>Requires Admin access. Zendesk uses <strong>Webhooks</strong> (destination) paired with <strong>Triggers</strong> (rules) to send events.</p>
<ol>
<li>Go to <strong>Admin Centre</strong> &rarr; <strong>Apps and integrations</strong> &rarr; <strong>Webhooks</strong></li>
<li>Click <strong>Create webhook</strong></li>
<li>Select <strong>Trigger or automation</strong> as the connection method</li>
<li>Enter a name and paste your dread webhook URL</li>
<li>Set request method to <strong>POST</strong> and format to <strong>JSON</strong></li>
<li>Click <strong>Create webhook</strong></li>
<li>Go to <strong>Business rules</strong> &rarr; <strong>Triggers</strong> &rarr; <strong>Add trigger</strong></li>
<li>Set conditions (e.g. ticket created, status changed)</li>
<li>Under <strong>Actions</strong>, select <strong>Notify webhook</strong> and choose your webhook</li>
<li>Define the JSON body using placeholders (<code>{{ticket.id}}</code>, <code>{{ticket.title}}</code>)</li>
</ol>
<div class="expect">Ticket events show the trigger action and ticket info (e.g. <strong>zendesk &mdash; Ticket #4521 created: Cannot log in</strong>).</div>
</section>

<section id="intercom" class="docs-section">
<h2>Intercom</h2>
<ol>
<li>Log in to <strong>Intercom</strong></li>
<li>Go to <strong>Settings</strong> &rarr; <strong>Integrations</strong> &rarr; <strong>Developer Hub</strong></li>
<li>Select your app (or create one) &rarr; click <strong>Webhooks</strong></li>
<li>Paste your dread webhook URL in the <strong>Webhook URL</strong> field</li>
<li>Select topics: <code>conversation.created</code>, <code>conversation.user.replied</code>, <code>contact.created</code>, <code>ticket.created</code>, etc.</li>
<li>Click <strong>Save</strong></li>
</ol>
<p>Intercom signs webhooks with HMAC-SHA1 using your app's client secret. The signature is in the <code>X-Hub-Signature</code> header.</p>
<div class="expect">Conversation events show the topic and user (e.g. <strong>intercom &mdash; conversation.user.replied by alice@example.com</strong>).</div>
</section>

<section id="postmark" class="docs-section">
<h2>Postmark</h2>
<ol>
<li>Log in to <strong>account.postmarkapp.com</strong></li>
<li>Select your <strong>Server</strong> &rarr; go to <strong>Settings</strong> &rarr; <strong>Webhooks</strong></li>
<li>Click <strong>Add webhook</strong></li>
<li>Paste your dread webhook URL</li>
<li>Select message events: <code>Delivery</code>, <code>Bounce</code>, <code>Spam Complaint</code>, <code>Open</code>, <code>Click</code>, <code>Subscription Change</code></li>
<li>Optionally enable <strong>Include message content</strong></li>
<li>Click <strong>Save webhook</strong></li>
</ol>
<p>Use the <strong>Send test</strong> button on each event type to verify delivery. Postmark sends a <code>X-Postmark-Webhooks-Auth</code> header if you set an HTTP Auth username/password.</p>
<div class="expect">Email events show the event type and recipient (e.g. <strong>postmark &mdash; Bounce: user@example.com (hard bounce)</strong>).</div>
</section>

<section id="mailgun" class="docs-section">
<h2>Mailgun</h2>
<ol>
<li>Log in to <strong>app.mailgun.com</strong></li>
<li>Go to <strong>Sending</strong> &rarr; <strong>Webhooks</strong></li>
<li>Select your domain from the dropdown</li>
<li>Click on an event type: <code>Delivered</code>, <code>Opened</code>, <code>Clicked</code>, <code>Bounced</code>, <code>Complained</code>, <code>Unsubscribed</code>, or <code>Failed</code></li>
<li>Paste your dread webhook URL and click <strong>Create webhook</strong></li>
<li>Repeat for each event type you want</li>
</ol>
<p>Mailgun signs webhooks using your domain's API key. The signature is in the POST body as <code>signature.token</code>, <code>signature.timestamp</code>, and <code>signature.signature</code>.</p>
<div class="expect">Email events show the event type and recipient (e.g. <strong>mailgun &mdash; delivered to user@example.com</strong>).</div>
</section>

<section id="telegram" class="docs-section">
<h2>Telegram</h2>
<p>Telegram uses <strong>Bot API webhooks</strong> to push updates to your URL instead of polling.</p>
<ol>
<li>Create a bot via <strong>@BotFather</strong> on Telegram and note the bot token</li>
<li>Set the webhook URL using the Telegram API:</li>
</ol>
<div class="code-block"><pre><code>curl -X POST "https://api.telegram.org/botYOUR_BOT_TOKEN/setWebhook" \
  -d "url=https://dread.sh/wh/ch_xxx"</code></pre><button class="copy-btn" onclick="copyCode(this)">Copy</button></div>
<ol start="3">
<li>Verify the webhook is set:</li>
</ol>
<div class="code-block"><pre><code>curl "https://api.telegram.org/botYOUR_BOT_TOKEN/getWebhookInfo"</code></pre><button class="copy-btn" onclick="copyCode(this)">Copy</button></div>
<p>Telegram sends updates for messages, edited messages, channel posts, callback queries, and inline queries.</p>
<div class="expect">Message events show the sender and text (e.g. <strong>telegram &mdash; message from alice: Deploy ready?</strong>).</div>
</section>

<hr class="section-divider">

<!-- SaaS -->
<section id="hubspot" class="docs-section">
<h2>HubSpot</h2>
<p>Requires a HubSpot developer account and a registered app.</p>
<ol>
<li>Log in to your <strong>HubSpot developer account</strong></li>
<li>Go to your app dashboard and select your app (or create one)</li>
<li>Click <strong>Webhooks</strong> in the left sidebar</li>
<li>Enter your dread webhook URL as the <strong>Target URL</strong></li>
<li>Click <strong>Create subscription</strong></li>
<li>Select the <strong>Object type</strong> (Contacts, Companies, Deals, Tickets)</li>
<li>Select event types (creation, deletion, property changes)</li>
<li>Click <strong>Subscribe</strong> &mdash; subscriptions start paused</li>
<li>Hover over the subscription, check it, and click <strong>Activate</strong></li>
</ol>
<div class="expect">HubSpot events show the subscription type and object ID (e.g. <strong>contact.creation &mdash; object 12345</strong>).</div>
</section>

<section id="typeform" class="docs-section">
<h2>Typeform</h2>
<ol>
<li>Open your form in Typeform</li>
<li>Click <strong>Connect</strong> in the top menu</li>
<li>Click the <strong>Webhooks</strong> tab</li>
<li>Click <strong>Add a webhook</strong></li>
<li>Paste your dread webhook URL</li>
<li>Toggle the webhook <strong>ON</strong> (off by default)</li>
<li>Optionally click <strong>Edit</strong> to add a secret for signature verification</li>
</ol>
<p>Use <strong>View deliveries</strong> &rarr; <strong>Send test request</strong> to verify.</p>
<div class="expect">Typeform events show the form title (e.g. <strong>form_response &mdash; Customer Feedback Survey</strong>).</div>
</section>

<section id="clerk" class="docs-section">
<h2>Clerk</h2>
<p>Clerk uses Svix for webhook delivery.</p>
<ol>
<li>Log in to your <strong>Clerk Dashboard</strong></li>
<li>Go to <strong>Webhooks</strong> in the left sidebar</li>
<li>Click <strong>Add Endpoint</strong></li>
<li>Paste your dread webhook URL</li>
<li>Select events to receive: <code>user.created</code>, <code>user.updated</code>, <code>user.deleted</code>, organization events, session events, etc. (or leave unselected for all)</li>
<li>Click <strong>Create</strong></li>
<li>Copy the <strong>Webhook Signing Secret</strong> from the endpoint settings page</li>
</ol>
<p>Use the <strong>Testing</strong> tab on your endpoint page to send example events.</p>
<div class="expect">Clerk events are detected via the <code>Svix-Id</code> header and show as svix events with the event type.</div>
</section>

<section id="twitch" class="docs-section">
<h2>Twitch EventSub</h2>
<p>Twitch EventSub requires API calls to create subscriptions (no dashboard UI).</p>
<ol>
<li>Register an app at <strong>dev.twitch.tv/console</strong> &rarr; <strong>Applications</strong> &rarr; <strong>Register Your Application</strong></li>
<li>Note your <strong>Client ID</strong> and generate a <strong>Client Secret</strong></li>
<li>Get an access token via the Client Credentials flow:</li>
</ol>
<div class="code-block"><pre><code>curl -X POST "https://id.twitch.tv/oauth2/token" \
  -d "client_id=YOUR_CLIENT_ID" \
  -d "client_secret=YOUR_CLIENT_SECRET" \
  -d "grant_type=client_credentials"</code></pre><button class="copy-btn" onclick="copyCode(this)">Copy</button></div>
<ol start="4">
<li>Create a webhook subscription:</li>
</ol>
<div class="code-block"><pre><code>curl -X POST "https://api.twitch.tv/helix/eventsub/subscriptions" \
  -H "Authorization: Bearer YOUR_ACCESS_TOKEN" \
  -H "Client-Id: YOUR_CLIENT_ID" \
  -H "Content-Type: application/json" \
  -d '{
    "type": "channel.follow",
    "version": "2",
    "condition": {"broadcaster_user_id": "USER_ID", "moderator_user_id": "USER_ID"},
    "transport": {"method": "webhook", "callback": "https://dread.sh/wh/ch_xxx", "secret": "your-secret"}
  }'</code></pre><button class="copy-btn" onclick="copyCode(this)">Copy</button></div>
<ol start="5">
<li>Twitch sends a verification challenge &mdash; dread responds automatically</li>
</ol>
<div class="expect">Twitch events show the subscription type and broadcaster name (e.g. <strong>channel.follow &mdash; ninja</strong>).</div>
</section>

<section id="zapier" class="docs-section">
<h2>Zapier</h2>
<p>Zapier can connect <strong>7,000+ apps</strong> to dread &mdash; even services that don't have native webhooks. Use a Zap to trigger from any supported service and POST to your dread channel.</p>
<ol>
<li>Log in to <strong>zapier.com</strong> &rarr; click <strong>Create</strong> &rarr; <strong>Zaps</strong></li>
<li>Choose your <strong>Trigger</strong> app (e.g. Google Sheets, Notion, Airtable, QuickBooks, Monday.com &mdash; anything Zapier supports)</li>
<li>Configure the trigger event (e.g. "New Row in Spreadsheet", "New Database Item")</li>
<li>Connect your account and test the trigger</li>
<li>Add an <strong>Action</strong> step &rarr; search for <strong>Webhooks by Zapier</strong></li>
<li>Select <strong>POST</strong> as the action event</li>
<li>Set the URL to your dread webhook URL</li>
<li>Set <strong>Payload Type</strong> to <strong>Json</strong></li>
<li>Map fields from the trigger into the payload data (e.g. <code>type</code> = "row.added", <code>message</code> = the row data)</li>
<li>Add <code>?source=google-sheets</code> to the end of your webhook URL to label events</li>
<li>Test the action and turn on the Zap</li>
</ol>
<p>This is the best way to connect services that don't support webhooks natively &mdash; Notion databases, Google Sheets, Airtable, Monday.com, Trello, and thousands more.</p>
<div class="expect">Events show the source you set and the mapped message (e.g. <strong>google-sheets &mdash; New row: Order #1042 from alice@example.com</strong>).</div>
</section>

<section id="calendly" class="docs-section">
<h2>Calendly</h2>
<ol>
<li>Log in to <strong>calendly.com</strong></li>
<li>Go to <strong>Integrations &amp; apps</strong></li>
<li>Scroll to <strong>Webhooks</strong> (under API &amp; connectors) and click <strong>Learn more</strong></li>
<li>You'll need to use the <strong>Calendly API</strong> to create a webhook subscription:</li>
</ol>
<div class="code-block"><pre><code>curl -X POST "https://api.calendly.com/webhook_subscriptions" \
  -H "Authorization: Bearer YOUR_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "url": "https://dread.sh/wh/ch_xxx",
    "events": ["invitee.created", "invitee.canceled"],
    "organization": "https://api.calendly.com/organizations/YOUR_ORG_UUID",
    "scope": "organization"
  }'</code></pre><button class="copy-btn" onclick="copyCode(this)">Copy</button></div>
<ol start="5">
<li>Get your API token from <strong>Integrations &amp; apps</strong> &rarr; <strong>API &amp; connectors</strong> &rarr; <strong>Personal access tokens</strong></li>
<li>Get your organisation UUID from <code>GET /users/me</code></li>
</ol>
<p>Events: <code>invitee.created</code> (new booking), <code>invitee.canceled</code> (cancelled booking), <code>routing_form_submission.created</code>.</p>
<div class="expect">Booking events show the event type and invitee (e.g. <strong>calendly &mdash; invitee.created: alice@example.com booked 30-Min Meeting</strong>).</div>
</section>

<section id="docusign" class="docs-section">
<h2>DocuSign</h2>
<p>DocuSign Connect sends real-time envelope status updates.</p>
<ol>
<li>Log in to <strong>DocuSign Admin</strong></li>
<li>Go to <strong>Settings</strong> &rarr; <strong>Connect</strong></li>
<li>Click <strong>Add Configuration</strong> &rarr; <strong>Custom</strong></li>
<li>Enter a name for the configuration</li>
<li>Paste your dread webhook URL in the <strong>URL to Publish</strong> field</li>
<li>Under <strong>Trigger events</strong>, select envelope events: <code>Sent</code>, <code>Delivered</code>, <code>Completed</code>, <code>Declined</code>, <code>Voided</code></li>
<li>Set data format to <strong>JSON</strong></li>
<li>Click <strong>Save</strong></li>
</ol>
<p>Requires DocuSign Admin or Account Administrator role. Use <strong>Logs</strong> in the Connect settings to debug delivery issues.</p>
<div class="expect">Envelope events show the status and subject (e.g. <strong>docusign &mdash; Completed: NDA Agreement with Acme Corp</strong>).</div>
</section>

<section id="auth0" class="docs-section">
<h2>Auth0</h2>
<p>Auth0 uses <strong>Log Streams</strong> to send authentication events via webhook.</p>
<ol>
<li>Log in to the <strong>Auth0 Dashboard</strong></li>
<li>Go to <strong>Monitoring</strong> &rarr; <strong>Streams</strong></li>
<li>Click <strong>Create Log Stream</strong></li>
<li>Select <strong>Custom Webhook</strong></li>
<li>Enter a name and paste your dread webhook URL</li>
<li>Set <strong>Content Type</strong> to <code>application/json</code></li>
<li>Optionally set an <strong>Authorization Token</strong> (sent as a Bearer token)</li>
<li>Under <strong>Filter by Event Category</strong>, select categories: login success, login failure, signup, password change, etc.</li>
<li>Click <strong>Save</strong></li>
</ol>
<p>Auth0 sends events in batches. Each delivery may contain multiple log entries.</p>
<div class="expect">Auth events show the event type and user (e.g. <strong>auth0 &mdash; Successful login: alice@example.com</strong>).</div>
</section>

<section id="launchdarkly" class="docs-section">
<h2>LaunchDarkly</h2>
<ol>
<li>Log in to <strong>LaunchDarkly</strong></li>
<li>Go to <strong>Integrations</strong> in the left sidebar</li>
<li>Find <strong>Webhooks</strong> and click <strong>Add integration</strong></li>
<li>Paste your dread webhook URL</li>
<li>Optionally check <strong>Sign this webhook</strong> and note the generated secret</li>
<li>Select a <strong>Policy</strong> to filter which events trigger the webhook (or leave blank for all events)</li>
<li>Toggle the webhook <strong>On</strong></li>
<li>Click <strong>Save</strong></li>
</ol>
<p>LaunchDarkly sends events for flag changes, project updates, environment changes, and member actions. Use policy filters to narrow to specific projects or flags.</p>
<div class="expect">Flag events show the action and flag key (e.g. <strong>launchdarkly &mdash; flag.updated: enable-new-checkout</strong>).</div>
</section>

<section id="figma" class="docs-section">
<h2>Figma</h2>
<p>Figma webhooks are created via the <strong>Figma API</strong> (no dashboard UI).</p>
<ol>
<li>Generate a <strong>Personal Access Token</strong> at <strong>figma.com/developers</strong> &rarr; <strong>Manage personal access tokens</strong></li>
<li>Find your <strong>Team ID</strong> from the URL when viewing a team page (e.g. <code>figma.com/files/team/123456</code>)</li>
<li>Create a webhook subscription:</li>
</ol>
<div class="code-block"><pre><code>curl -X POST "https://api.figma.com/v2/webhooks" \
  -H "Authorization: Bearer YOUR_FIGMA_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "event_type": "FILE_UPDATE",
    "team_id": "123456",
    "endpoint": "https://dread.sh/wh/ch_xxx",
    "passcode": "your-secret"
  }'</code></pre><button class="copy-btn" onclick="copyCode(this)">Copy</button></div>
<ol start="4">
<li>Available event types: <code>FILE_UPDATE</code>, <code>FILE_DELETE</code>, <code>FILE_VERSION_UPDATE</code>, <code>FILE_COMMENT</code>, <code>LIBRARY_PUBLISH</code></li>
</ol>
<p>Figma sends events for all files within the specified team. The passcode is included in every payload for verification.</p>
<div class="expect">File events show the event type and file name (e.g. <strong>figma &mdash; FILE_COMMENT on Homepage Redesign v2</strong>).</div>
</section>

<section id="salesforce" class="docs-section">
<h2>Salesforce</h2>
<p>Salesforce sends webhook-style notifications via <strong>Outbound Messages</strong> in Workflow Rules or Flow.</p>
<ol>
<li>Go to <strong>Setup</strong> &rarr; search <strong>Outbound Messages</strong> &rarr; <strong>New Outbound Message</strong></li>
<li>Select the <strong>Object</strong> (e.g. Lead, Opportunity, Case)</li>
<li>Set <strong>Endpoint URL</strong> to your dread channel URL: <code>https://dread.sh/wh/ch_xxx?source=salesforce</code></li>
<li>Select the fields to include in the payload</li>
<li>Create a <strong>Workflow Rule</strong> or <strong>Flow</strong> that triggers the outbound message on record create/update</li>
</ol>
<p>Alternatively, use Salesforce <strong>Platform Events</strong> with a custom Apex trigger to POST JSON to your dread URL for more flexibility.</p>
<div class="expect">Events show the object type and action (e.g. <strong>salesforce &mdash; Opportunity updated: Acme Corp Deal</strong>).</div>
</section>

<section id="notion" class="docs-section">
<h2>Notion</h2>
<p>Notion doesn't have native outgoing webhooks, but you can use <strong>Notion automations</strong> with a webhook action or connect via Zapier/Make.</p>
<ol>
<li>Open a Notion database &rarr; click <strong>Automations</strong> (lightning bolt icon)</li>
<li>Set a trigger (e.g. <strong>When a page is added</strong> or <strong>When a property changes</strong>)</li>
<li>Add action &rarr; <strong>Send webhook</strong></li>
<li>Set the URL to your dread channel: <code>https://dread.sh/wh/ch_xxx?source=notion</code></li>
</ol>
<p>Alternatively, use the <strong>Notion API</strong> with a polling script or connect Notion to dread via <strong>Zapier</strong> using a Webhooks by Zapier action.</p>
<div class="expect">Events show the database and page info (e.g. <strong>notion &mdash; Page added: Q1 Planning Notes</strong>).</div>
</section>

<section id="airtable" class="docs-section">
<h2>Airtable</h2>
<p>Airtable supports webhooks via <strong>Airtable Automations</strong>.</p>
<ol>
<li>Open your base &rarr; click <strong>Automations</strong> in the top bar</li>
<li>Create a new automation with a trigger (e.g. <strong>When a record is created</strong>, <strong>When a record is updated</strong>)</li>
<li>Add action &rarr; <strong>Send a webhook</strong></li>
<li>Set the URL to: <code>https://dread.sh/wh/ch_xxx?source=airtable</code></li>
<li>Configure the body to include the fields you want to track</li>
<li>Test and enable the automation</li>
</ol>
<div class="expect">Events show the table and record info (e.g. <strong>airtable &mdash; Record created in Projects: New landing page</strong>).</div>
</section>

<section id="asana" class="docs-section">
<h2>Asana</h2>
<p>Asana sends webhooks via its <strong>API</strong>. Create a webhook subscription using the Asana API.</p>
<ol>
<li>Generate a <strong>Personal Access Token</strong> at <strong>app.asana.com/0/developer-console</strong></li>
<li>Find your <strong>Project GID</strong> from the project URL</li>
<li>Create a webhook subscription:</li>
</ol>
<div class="code-block"><pre><code>curl -X POST "https://app.asana.com/api/1.0/webhooks" \
  -H "Authorization: Bearer YOUR_ASANA_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "data": {
      "resource": "PROJECT_GID",
      "target": "https://dread.sh/wh/ch_xxx?source=asana"
    }
  }'</code></pre><button class="copy-btn" onclick="copyCode(this)">Copy</button></div>
<ol start="4">
<li>Asana will send a handshake request &mdash; dread handles this automatically</li>
<li>Events fire for task creation, completion, assignment, comments, and more</li>
</ol>
<div class="expect">Events show the action and task (e.g. <strong>asana &mdash; Task completed: Design review for homepage</strong>).</div>
</section>

<section id="webflow" class="docs-section">
<h2>Webflow</h2>
<p>Webflow supports webhooks for site, form, and e-commerce events.</p>
<ol>
<li>Go to <strong>Site Settings</strong> &rarr; <strong>Integrations</strong> &rarr; <strong>Webhooks</strong></li>
<li>Click <strong>Add Webhook</strong></li>
<li>Select a <strong>Trigger Type</strong> (e.g. Form submission, Site publish, Ecommerce order)</li>
<li>Set the <strong>Webhook URL</strong> to: <code>https://dread.sh/wh/ch_xxx?source=webflow</code></li>
<li>Click <strong>Add Webhook</strong> to save</li>
</ol>
<p>Available triggers include: <code>form_submission</code>, <code>site_publish</code>, <code>page_created</code>, <code>ecomm_new_order</code>, <code>ecomm_order_changed</code>, <code>memberships_user_account_added</code>, and more.</p>
<div class="expect">Events show the trigger type (e.g. <strong>webflow &mdash; form_submission on Contact Form</strong>).</div>
</section>

<section id="custom-source" class="docs-section">
<h2>Custom / Any Service</h2>
<p>Any service that sends JSON webhooks works with dread. Just point it at your channel URL:</p>
<div class="code-block"><pre><code>curl -X POST "https://dread.sh/wh/ch_xxx?source=myapp" \
  -H "Content-Type: application/json" \
  -d '{"type":"deploy.success","message":"v1.2.3 deployed to production"}'</code></pre><button class="copy-btn" onclick="copyCode(this)">Copy</button></div>
<p>Add <code>?source=myapp</code> to the URL to control the source label. Use <code>type</code> or <code>event</code> in your JSON for the event type, and <code>message</code> or <code>description</code> for the summary text. You can also use the <code>X-Dread-Source</code> header instead.</p>
<p>For all other features (muting, alerts, export, digest, forwarding, status page, dashboard), see the <a href="/docs" style="color:var(--violet)">Documentation</a>.</p>
</section>

</main>
</div>

<script>
function copyCode(btn) {
  const code = btn.previousElementSibling.querySelector('code');
  navigator.clipboard.writeText(code.textContent);
  btn.textContent = 'Copied!';
  setTimeout(function() { btn.textContent = 'Copy'; }, 1500);
}

function toggleTheme() {
  document.documentElement.classList.toggle('light');
  localStorage.setItem('theme', document.documentElement.classList.contains('light') ? 'light' : 'dark');
}

// Sidebar active link tracking
var links = document.querySelectorAll('.docs-sidebar a');
var sections = [];
links.forEach(function(a) {
  var id = a.getAttribute('href');
  if (id && id.startsWith('#')) {
    var el = document.getElementById(id.slice(1));
    if (el) sections.push({ el: el, link: a });
  }
});
function updateActive() {
  var scrollY = window.scrollY + 100;
  var active = sections[0];
  sections.forEach(function(s) { if (s.el.offsetTop <= scrollY) active = s; });
  links.forEach(function(a) { a.classList.remove('active'); });
  if (active) active.link.classList.add('active');
}
window.addEventListener('scroll', updateActive);
updateActive();

function toggleSidebar() {
  document.getElementById('sidebar').classList.toggle('open');
  document.getElementById('sidebar-overlay').classList.toggle('open');
}

document.getElementById('menu-btn').addEventListener('click', toggleSidebar);
lucide.createIcons();
</script>
</body>
</html>`

const downloadPage = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="description" content="Download dread — install with one command on macOS or Linux. Supports Intel, Apple Silicon, and ARM64.">
<link rel="canonical" href="https://dread.sh/download">
<meta property="og:type" content="website">
<meta property="og:site_name" content="dread.sh">
<meta property="og:title" content="Download and Install dread - Webhook CLI Tool">
<meta property="og:description" content="Install dread with one command on macOS or Linux. Supports Intel, Apple Silicon, and ARM64.">
<meta property="og:url" content="https://dread.sh/download">
<meta property="og:image" content="https://dread.sh/og.png">
<meta name="twitter:card" content="summary_large_image">
<meta name="twitter:title" content="Download and Install dread - Webhook CLI Tool">
<meta name="twitter:description" content="Install dread with one command on macOS or Linux. Supports Intel, Apple Silicon, and ARM64.">
<meta name="twitter:image" content="https://dread.sh/og.png">
<script>if(localStorage.getItem('theme')==='light')document.documentElement.classList.add('light')</script>
<link rel="icon" href="data:image/svg+xml,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 100 100'><circle cx='50' cy='50' r='40' fill='%23c37960'/></svg>">
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Press+Start+2P&display=swap">
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/geist@1.3.1/dist/fonts/geist-sans/style.min.css">
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/geist@1.3.1/dist/fonts/geist-mono/style.min.css">
<title>Download and Install dread - Webhook CLI Tool</title>
<script src="https://unpkg.com/lucide@0.469.0/dist/umd/lucide.min.js"></script>
<style>
  :root {
    --bg: oklch(10% 0.003 256);
    --surface: oklch(16% 0.003 256);
    --surface-hover: oklch(20% 0.003 256);
    --border: oklch(23% 0.003 256);
    --border-subtle: oklch(18% 0.003 256);
    --text: oklch(98.5% 0.003 256);
    --text-secondary: oklch(70.5% 0.003 256);
    --text-muted: oklch(55.2% 0.003 256);
    --text-dim: oklch(40% 0.003 256);
    --accent: oklch(65% 0.1 40);
    --accent-dim: oklch(47% 0.09 36);
    --accent-glow: oklch(55% 0.1 38 / 0.15);
    --accent-glow-strong: oklch(55% 0.1 38 / 0.3);
    --nav-bg: oklch(10% 0.003 256 / 0.85);
  }
  :root.light {
    --bg: oklch(98% 0.003 256); --surface: oklch(97% 0.003 256);
    --surface-hover: oklch(94% 0.003 256); --border: oklch(85% 0.003 256);
    --border-subtle: oklch(90% 0.003 256); --text: oklch(15% 0.003 256);
    --text-secondary: oklch(35% 0.003 256); --text-muted: oklch(50% 0.003 256);
    --text-dim: oklch(65% 0.003 256); --accent: oklch(50% 0.12 40);
    --accent-dim: oklch(40% 0.1 36); --accent-glow: oklch(50% 0.12 40 / 0.1);
    --accent-glow-strong: oklch(50% 0.12 40 / 0.2);
    --nav-bg: oklch(98% 0.003 256 / 0.85);
  }
  * { margin: 0; padding: 0; box-sizing: border-box; }
  html, body { overscroll-behavior: none; }
  html { font-size: 18px; }
  body {
    font-family: "Geist", -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
    background: var(--bg); color: var(--text-secondary);
    line-height: 1.6; -webkit-font-smoothing: antialiased;
  }
  code, pre, kbd { font-family: "Geist Mono", ui-monospace, "Cascadia Code", "Source Code Pro", Menlo, Consolas, monospace; }
  /*! NAV_CSS */

  .download-page {
    max-width: 640px; margin: 0 auto;
    padding: 100px 24px 120px; text-align: center;
  }
  .download-page h1 {
    font-size: 2.4rem; font-weight: 700; color: var(--text);
    letter-spacing: -0.03em; margin-bottom: 12px;
  }
  .download-page .subtitle {
    font-size: 1rem; color: var(--text-muted); margin-bottom: 48px;
    line-height: 1.7;
  }

  .stats-row {
    display: flex; gap: 24px; justify-content: center;
    margin-bottom: 48px; flex-wrap: wrap;
  }
  .stat-card {
    background: var(--surface); border: 1px solid var(--border);
    border-radius: 12px; padding: 24px 32px; min-width: 160px;
  }
  .stat-number {
    font-size: 2.4rem; font-weight: 700; color: var(--accent);
    letter-spacing: -0.03em; line-height: 1;
  }
  .stat-label {
    font-size: 0.75rem; color: var(--text-muted);
    text-transform: uppercase; letter-spacing: 0.06em;
    margin-top: 8px;
  }

  .install-block {
    background: var(--surface); border: 1px solid var(--border);
    border-radius: 12px; padding: 32px; text-align: left;
    margin-bottom: 24px; position: relative;
  }
  .install-block h3 {
    font-size: 0.85rem; color: var(--text); font-weight: 600;
    margin-bottom: 12px;
  }
  .install-block pre {
    font-size: 0.85rem; color: var(--text); line-height: 1.7;
    overflow-x: auto;
  }
  .install-block .copy-btn {
    position: absolute; top: 12px; right: 12px;
    background: var(--surface-hover); border: 1px solid var(--border);
    border-radius: 6px; padding: 4px 10px; cursor: pointer;
    font-size: 0.7rem; color: var(--text-muted); transition: color 0.15s;
  }
  .install-block .copy-btn:hover { color: var(--text); }

  .or-divider {
    font-size: 0.8rem; color: var(--text-dim); margin: 16px 0;
    text-transform: uppercase; letter-spacing: 0.06em;
  }
</style>
</head>
<body>
<!-- NAV_HTML -->

<div class="download-page">
  <h1>Download dread</h1>

  <div class="stats-row">
    <div class="stat-card">
      <div class="stat-number">{{UNIQUE_COUNT}}</div>
      <div class="stat-label">Installs</div>
    </div>
  </div>
</div>

<script>
function copyText(text, btn) {
  navigator.clipboard.writeText(text);
  btn.textContent = 'Copied!';
  setTimeout(function() { btn.textContent = 'Copy'; }, 1500);
}
function toggleTheme() {
  document.documentElement.classList.toggle('light');
  localStorage.setItem('theme', document.documentElement.classList.contains('light') ? 'light' : 'dark');
}
lucide.createIcons();
</script>
</body>
</html>`

// ─── Blog shared styles ───

const blogCSS = `
  :root {
    --bg: oklch(10% 0.003 256);
    --surface: oklch(16% 0.003 256);
    --surface-hover: oklch(20% 0.003 256);
    --border: oklch(23% 0.003 256);
    --text: oklch(98.5% 0.003 256);
    --text-secondary: oklch(70.5% 0.003 256);
    --text-muted: oklch(55.2% 0.003 256);
    --text-dim: oklch(40% 0.003 256);
    --accent: oklch(65% 0.1 40);
    --accent-dim: oklch(47% 0.09 36);
    --accent-glow: oklch(55% 0.1 38 / 0.15);
    --nav-bg: oklch(10% 0.003 256 / 0.85);
  }
  html.light {
    --bg: oklch(98% 0.003 256);
    --surface: oklch(94% 0.003 256);
    --surface-hover: oklch(90% 0.003 256);
    --border: oklch(85% 0.003 256);
    --text: oklch(15% 0.003 256);
    --text-secondary: oklch(35% 0.003 256);
    --text-muted: oklch(50% 0.003 256);
    --text-dim: oklch(65% 0.003 256);
    --nav-bg: oklch(98% 0.003 256 / 0.85);
  }
  * { margin: 0; padding: 0; box-sizing: border-box; }
  body {
    font-family: "Geist", -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
    background: var(--bg); color: var(--text);
    line-height: 1.7; -webkit-font-smoothing: antialiased;
  }
  code, pre {
    font-family: "Geist Mono", ui-monospace, "Cascadia Code", Menlo, Consolas, monospace;
  }
  /*! NAV_CSS */
  .blog-container { max-width: 740px; margin: 0 auto; padding: 60px 24px 80px; }
  .blog-header { margin-bottom: 48px; }
  .blog-header h1 { font-size: 2.2rem; font-weight: 700; letter-spacing: -0.02em; line-height: 1.2; margin-bottom: 12px; }
  .blog-meta { color: var(--text-muted); font-size: 0.9rem; margin-bottom: 8px; }
  .blog-meta time { color: var(--text-secondary); }
  .blog-content h2 { font-size: 1.5rem; font-weight: 600; margin: 40px 0 16px; letter-spacing: -0.01em; }
  .blog-content h3 { font-size: 1.2rem; font-weight: 600; margin: 32px 0 12px; }
  .blog-content p { margin-bottom: 20px; color: var(--text-secondary); }
  .blog-content ul, .blog-content ol { margin-bottom: 20px; padding-left: 24px; color: var(--text-secondary); }
  .blog-content li { margin-bottom: 8px; }
  .blog-content pre {
    background: var(--surface); border: 1px solid var(--border); border-radius: 8px;
    padding: 16px 20px; overflow-x: auto; margin-bottom: 24px; font-size: 0.9rem;
    line-height: 1.6;
  }
  .blog-content code { background: var(--surface); padding: 2px 6px; border-radius: 4px; font-size: 0.88em; }
  .blog-content pre code { background: none; padding: 0; }
  .blog-content blockquote {
    border-left: 3px solid var(--accent); padding: 12px 20px; margin-bottom: 24px;
    background: var(--accent-glow); border-radius: 0 8px 8px 0;
  }
  .blog-content blockquote p { color: var(--text); margin-bottom: 0; }
  .blog-content a { color: var(--accent); text-decoration: underline; text-underline-offset: 3px; }
  .blog-content a:hover { opacity: 0.8; }
  .blog-content strong { color: var(--text); }
  .blog-cta {
    background: var(--surface); border: 1px solid var(--border); border-radius: 12px;
    padding: 28px 32px; margin-top: 48px; text-align: center;
  }
  .blog-cta h3 { margin: 0 0 8px; color: var(--text); }
  .blog-cta p { color: var(--text-muted); margin-bottom: 16px; }
  .blog-cta pre { background: var(--bg); display: inline-block; padding: 10px 20px; border-radius: 8px; margin: 0; border: 1px solid var(--border); }
  .blog-cards { display: grid; grid-template-columns: 1fr; gap: 20px; }
  .blog-card {
    background: var(--surface); border: 1px solid var(--border); border-radius: 12px;
    padding: 24px 28px; text-decoration: none; color: var(--text); transition: border-color 0.15s;
  }
  .blog-card:hover { border-color: var(--accent); }
  .blog-card h2 { font-size: 1.3rem; font-weight: 600; margin-bottom: 8px; letter-spacing: -0.01em; }
  .blog-card p { color: var(--text-muted); font-size: 0.95rem; margin: 0; }
  .blog-card .card-meta { color: var(--text-dim); font-size: 0.8rem; margin-top: 12px; }
`

// ─── Blog Index ───

const blogIndexPage = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="description" content="Technical articles about webhooks, API integrations, and developer tooling. Learn webhook best practices, testing strategies, and integration guides.">
<link rel="canonical" href="https://dread.sh/blog">
<meta property="og:type" content="website">
<meta property="og:site_name" content="dread.sh">
<meta property="og:title" content="Blog - dread.sh Webhook Engineering">
<meta property="og:description" content="Technical articles about webhooks, API integrations, and developer tooling.">
<meta property="og:url" content="https://dread.sh/blog">
<meta property="og:image" content="https://dread.sh/og.png">
<meta name="twitter:card" content="summary_large_image">
<meta name="twitter:title" content="Blog - dread.sh Webhook Engineering">
<meta name="twitter:description" content="Technical articles about webhooks, API integrations, and developer tooling.">
<meta name="twitter:image" content="https://dread.sh/og.png">
<script>if(localStorage.getItem('theme')==='light')document.documentElement.classList.add('light')</script>
<link rel="icon" href="data:image/svg+xml,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 100 100'><circle cx='50' cy='50' r='40' fill='%23c37960'/></svg>">
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Press+Start+2P&display=swap">
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/geist@1.3.1/dist/fonts/geist-sans/style.min.css">
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/geist@1.3.1/dist/fonts/geist-mono/style.min.css">
<title>Blog - dread.sh Webhook Engineering</title>
<script src="https://unpkg.com/lucide@0.469.0/dist/umd/lucide.min.js"></script>
<style>/*! BLOG_CSS */</style>
</head>
<body>
<!-- NAV_HTML -->
<main class="blog-container">
  <div class="blog-header">
    <h1>Blog</h1>
    <p style="color:var(--text-muted)">Technical articles about webhooks, integrations, and developer tooling.</p>
  </div>
  <div class="blog-cards">
    <a href="/blog/webhook-vs-polling" class="blog-card">
      <h2>Webhook vs Polling: When to Use Each</h2>
      <p>Why 98.5% of polling requests are wasted, and how webhooks deliver real-time data with less infrastructure overhead.</p>
      <div class="card-meta">March 2026 · 6 min read</div>
    </a>
    <a href="/blog/test-webhooks-locally" class="blog-card">
      <h2>How to Test Webhooks Locally</h2>
      <p>A practical guide to receiving, inspecting, and debugging webhooks in your local development environment.</p>
      <div class="card-meta">March 2026 · 5 min read</div>
    </a>
    <a href="/blog/stripe-webhook-setup" class="blog-card">
      <h2>How to Set Up Stripe Webhooks</h2>
      <p>Step-by-step guide to configuring Stripe webhooks, verifying signatures, and getting real-time payment notifications.</p>
      <div class="card-meta">March 2026 · 7 min read</div>
    </a>
  </div>
</main>
<script>
function toggleTheme() {
  document.documentElement.classList.toggle('light');
  localStorage.setItem('theme', document.documentElement.classList.contains('light') ? 'light' : 'dark');
}
lucide.createIcons();
</script>
</body>
</html>`

// ─── Blog Post: Webhook vs Polling ───

const blogWebhookVsPolling = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="description" content="Webhooks deliver data in real time when events happen. Polling checks repeatedly on a schedule. Learn when to use each approach and why webhooks are more efficient.">
<link rel="canonical" href="https://dread.sh/blog/webhook-vs-polling">
<meta property="og:type" content="article">
<meta property="og:site_name" content="dread.sh">
<meta property="og:title" content="Webhook vs Polling: When to Use Each - dread.sh">
<meta property="og:description" content="Why 98.5% of polling requests are wasted, and how webhooks deliver real-time data with less overhead.">
<meta property="og:url" content="https://dread.sh/blog/webhook-vs-polling">
<meta property="og:image" content="https://dread.sh/og.png">
<meta name="twitter:card" content="summary_large_image">
<meta name="twitter:title" content="Webhook vs Polling: When to Use Each - dread.sh">
<meta name="twitter:description" content="Why 98.5% of polling requests are wasted, and how webhooks deliver real-time data with less overhead.">
<meta name="twitter:image" content="https://dread.sh/og.png">
<script type="application/ld+json">
{
  "@context": "https://schema.org",
  "@type": "Article",
  "headline": "Webhook vs Polling: When to Use Each",
  "description": "Webhooks deliver data in real time. Polling checks on a schedule. Learn when to use each and why webhooks are more efficient.",
  "datePublished": "2026-03-07",
  "author": {"@type": "Organization", "name": "dread.sh", "url": "https://dread.sh"},
  "publisher": {"@type": "Organization", "name": "dread.sh", "url": "https://dread.sh"}
}
</script>
<script>if(localStorage.getItem('theme')==='light')document.documentElement.classList.add('light')</script>
<link rel="icon" href="data:image/svg+xml,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 100 100'><circle cx='50' cy='50' r='40' fill='%23c37960'/></svg>">
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Press+Start+2P&display=swap">
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/geist@1.3.1/dist/fonts/geist-sans/style.min.css">
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/geist@1.3.1/dist/fonts/geist-mono/style.min.css">
<title>Webhook vs Polling: When to Use Each - dread.sh</title>
<script src="https://unpkg.com/lucide@0.469.0/dist/umd/lucide.min.js"></script>
<style>/*! BLOG_CSS */</style>
</head>
<body>
<!-- NAV_HTML -->
<main class="blog-container">
  <article>
    <div class="blog-header">
      <h1>Webhook vs Polling: When to Use Each</h1>
      <div class="blog-meta"><time datetime="2026-03-07">March 7, 2026</time> · 6 min read</div>
    </div>
    <div class="blog-content">
      <p><strong>Webhooks push data to you when something happens. Polling asks for data on a schedule.</strong> Both approaches have trade-offs, but for most real-time use cases, webhooks are dramatically more efficient.</p>

      <h2>What is Polling?</h2>
      <p>Polling means your application repeatedly sends HTTP requests to an API at regular intervals to check for new data. A typical implementation looks like this:</p>
      <pre><code>// Poll every 30 seconds
setInterval(async () => {
  const response = await fetch('/api/orders?since=' + lastCheck);
  const newOrders = await response.json();
  if (newOrders.length > 0) {
    processOrders(newOrders);
  }
  lastCheck = Date.now();
}, 30000);</code></pre>
      <p>The problem? Most of those requests return nothing. A Zapier study found that <strong>98.5% of polling requests are wasted</strong> — they return empty responses because nothing has changed.</p>

      <h2>What is a Webhook?</h2>
      <p>A webhook is an HTTP callback. Instead of asking for data, you give a service a URL and it sends you an HTTP POST request whenever something happens:</p>
      <pre><code>// Your server receives events as they happen
app.post('/webhooks/stripe', (req, res) => {
  const event = req.body;
  if (event.type === 'payment_intent.succeeded') {
    notifyTeam(event.data.object);
  }
  res.status(200).send('ok');
});</code></pre>
      <p>No wasted requests. No delays. Data arrives within milliseconds of the event occurring.</p>

      <h2>When to Use Webhooks</h2>
      <ul>
        <li><strong>Real-time notifications</strong> — payment confirmations, deployment alerts, error tracking</li>
        <li><strong>Event-driven workflows</strong> — triggering actions when something happens in another system</li>
        <li><strong>High-frequency data</strong> — where polling would generate too many empty requests</li>
        <li><strong>Multi-service orchestration</strong> — connecting Stripe, GitHub, Sentry, and other services</li>
      </ul>

      <h2>When Polling Still Makes Sense</h2>
      <ul>
        <li><strong>The API does not support webhooks</strong> — some legacy APIs only offer polling</li>
        <li><strong>You need bulk data sync</strong> — periodic full syncs of large datasets</li>
        <li><strong>Unreliable network</strong> — if your server might be down, polling with retry is simpler</li>
        <li><strong>Rate-limited APIs</strong> — when you need to control exactly how often you hit an endpoint</li>
      </ul>

      <h2>Performance Comparison</h2>
      <p>For a service generating 100 events per day:</p>
      <ul>
        <li><strong>Polling every 30s:</strong> 2,880 API requests/day, 100 contain data (3.5% efficiency)</li>
        <li><strong>Webhooks:</strong> 100 HTTP callbacks/day (100% efficiency)</li>
      </ul>
      <p>That is a 28x reduction in network traffic. At scale, this difference translates directly to lower infrastructure costs and faster response times.</p>

      <h2>The Practical Challenge with Webhooks</h2>
      <p>Webhooks are more efficient, but they introduce operational complexity:</p>
      <ul>
        <li>You need a publicly reachable endpoint to receive them</li>
        <li>You must verify webhook signatures to prevent spoofing</li>
        <li>You need to handle retries and idempotency</li>
        <li>Debugging is harder — you cannot just re-run a request</li>
      </ul>
      <p>This is exactly why we built <strong>dread</strong>. It gives you a webhook endpoint instantly, delivers events as desktop notifications and a live terminal feed, and lets you inspect and replay payloads without writing any server code.</p>

      <div class="blog-cta">
        <h3>Try dread</h3>
        <p>Get webhook notifications in your terminal in 30 seconds.</p>
        <pre><code>curl -sSL dread.sh/install | sh</code></pre>
      </div>
    </div>
  </article>
</main>
<script>
function toggleTheme() {
  document.documentElement.classList.toggle('light');
  localStorage.setItem('theme', document.documentElement.classList.contains('light') ? 'light' : 'dark');
}
lucide.createIcons();
</script>
</body>
</html>`

// ─── Blog Post: Test Webhooks Locally ───

const blogTestWebhooksLocally = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="description" content="Learn how to receive, inspect, and debug webhooks in your local development environment. Test Stripe, GitHub, and any webhook source without deploying.">
<link rel="canonical" href="https://dread.sh/blog/test-webhooks-locally">
<meta property="og:type" content="article">
<meta property="og:site_name" content="dread.sh">
<meta property="og:title" content="How to Test Webhooks Locally - dread.sh">
<meta property="og:description" content="Receive, inspect, and debug webhooks in local development without deploying or tunneling.">
<meta property="og:url" content="https://dread.sh/blog/test-webhooks-locally">
<meta property="og:image" content="https://dread.sh/og.png">
<meta name="twitter:card" content="summary_large_image">
<meta name="twitter:title" content="How to Test Webhooks Locally - dread.sh">
<meta name="twitter:description" content="Receive, inspect, and debug webhooks in local development without deploying or tunneling.">
<meta name="twitter:image" content="https://dread.sh/og.png">
<script type="application/ld+json">
{
  "@context": "https://schema.org",
  "@type": "Article",
  "headline": "How to Test Webhooks Locally",
  "description": "A practical guide to receiving, inspecting, and debugging webhooks in your local development environment.",
  "datePublished": "2026-03-07",
  "author": {"@type": "Organization", "name": "dread.sh", "url": "https://dread.sh"},
  "publisher": {"@type": "Organization", "name": "dread.sh", "url": "https://dread.sh"}
}
</script>
<script>if(localStorage.getItem('theme')==='light')document.documentElement.classList.add('light')</script>
<link rel="icon" href="data:image/svg+xml,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 100 100'><circle cx='50' cy='50' r='40' fill='%23c37960'/></svg>">
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Press+Start+2P&display=swap">
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/geist@1.3.1/dist/fonts/geist-sans/style.min.css">
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/geist@1.3.1/dist/fonts/geist-mono/style.min.css">
<title>How to Test Webhooks Locally - dread.sh</title>
<script src="https://unpkg.com/lucide@0.469.0/dist/umd/lucide.min.js"></script>
<style>/*! BLOG_CSS */</style>
</head>
<body>
<!-- NAV_HTML -->
<main class="blog-container">
  <article>
    <div class="blog-header">
      <h1>How to Test Webhooks Locally</h1>
      <div class="blog-meta"><time datetime="2026-03-07">March 7, 2026</time> · 5 min read</div>
    </div>
    <div class="blog-content">
      <p><strong>Testing webhooks during development is frustrating.</strong> The service sending the webhook needs a public URL, but your dev server is on localhost. Here are the most practical approaches.</p>

      <h2>The Problem</h2>
      <p>When you configure a webhook in Stripe, GitHub, or any other service, it needs to send HTTP requests to a URL it can reach. Your local machine at <code>localhost:3000</code> is not reachable from the internet. You need a bridge.</p>

      <h2>Option 1: Use a Webhook Relay</h2>
      <p>The simplest approach is to use a hosted webhook endpoint that captures events and lets you view them in real time. With dread, this takes 30 seconds:</p>
      <pre><code># Install dread
curl -sSL dread.sh/install | sh

# Create a channel and get your webhook URL
dread init

# Output: Your webhook URL is https://dread.sh/wh/ch_abc123
# Paste this URL into Stripe/GitHub/etc.</code></pre>
      <p>Now run <code>dread</code> to see events in your terminal as they arrive:</p>
      <pre><code># Open the terminal UI
dread

# Or run in the background with desktop notifications
dread watch</code></pre>
      <p>Every webhook that hits your URL shows up instantly as a desktop notification and in the terminal UI where you can inspect the full payload, headers, and metadata.</p>

      <h2>Option 2: Use a Tunnel (ngrok, etc.)</h2>
      <p>Tunneling tools expose your local server to the internet:</p>
      <pre><code>ngrok http 3000
# Gives you a public URL like https://abc123.ngrok.io</code></pre>
      <p>This works, but has drawbacks:</p>
      <ul>
        <li>Free tier URLs change every session — you must reconfigure the webhook each time</li>
        <li>Your local server must be running to receive events</li>
        <li>No built-in payload inspection or event history</li>
        <li>Adds latency and a dependency on the tunnel service</li>
      </ul>

      <h2>Option 3: CLI-Specific Tools</h2>
      <p>Some services offer their own CLI for webhook testing:</p>
      <pre><code># Stripe CLI
stripe listen --forward-to localhost:3000/webhooks
stripe trigger payment_intent.succeeded

# GitHub CLI (limited)
gh webhook forward --repo=owner/repo --events=push</code></pre>
      <p>These work well for their specific service but do not help when you need to test webhooks from multiple sources simultaneously.</p>

      <h2>Best Practices for Webhook Testing</h2>
      <ol>
        <li><strong>Always verify signatures</strong> — Even in development, test that your signature verification code works correctly.</li>
        <li><strong>Log the raw payload</strong> — Do not just log parsed data. Keep the raw JSON so you can debug parsing issues.</li>
        <li><strong>Test error scenarios</strong> — What happens when your handler returns a 500? Most services will retry, which can cause duplicate processing.</li>
        <li><strong>Use idempotency keys</strong> — Design your webhook handler to safely process the same event twice.</li>
        <li><strong>Check the response time</strong> — Most services expect a response within 5-30 seconds. If your handler takes longer, move the work to a background queue.</li>
      </ol>

      <h2>Comparing Approaches</h2>
      <p>For most developers, a webhook relay like dread is the fastest path to testing webhooks. It works across all services, persists event history, and does not require your local server to be running.</p>

      <div class="blog-cta">
        <h3>Start testing webhooks now</h3>
        <p>Get a webhook URL and terminal UI in one command.</p>
        <pre><code>curl -sSL dread.sh/install | sh</code></pre>
      </div>
    </div>
  </article>
</main>
<script>
function toggleTheme() {
  document.documentElement.classList.toggle('light');
  localStorage.setItem('theme', document.documentElement.classList.contains('light') ? 'light' : 'dark');
}
lucide.createIcons();
</script>
</body>
</html>`

// ─── Blog Post: Stripe Webhook Setup ───

const blogStripeWebhooks = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="description" content="Step-by-step guide to setting up Stripe webhooks. Configure endpoints, verify signatures, handle events, and get real-time payment notifications on your desktop.">
<link rel="canonical" href="https://dread.sh/blog/stripe-webhook-setup">
<meta property="og:type" content="article">
<meta property="og:site_name" content="dread.sh">
<meta property="og:title" content="How to Set Up Stripe Webhooks - dread.sh">
<meta property="og:description" content="Configure Stripe webhook endpoints, verify signatures, and get real-time payment notifications.">
<meta property="og:url" content="https://dread.sh/blog/stripe-webhook-setup">
<meta property="og:image" content="https://dread.sh/og.png">
<meta name="twitter:card" content="summary_large_image">
<meta name="twitter:title" content="How to Set Up Stripe Webhooks - dread.sh">
<meta name="twitter:description" content="Configure Stripe webhook endpoints, verify signatures, and get real-time payment notifications.">
<meta name="twitter:image" content="https://dread.sh/og.png">
<script type="application/ld+json">
{
  "@context": "https://schema.org",
  "@type": "Article",
  "headline": "How to Set Up Stripe Webhooks",
  "description": "Step-by-step guide to configuring Stripe webhooks, verifying signatures, and getting real-time payment notifications.",
  "datePublished": "2026-03-07",
  "author": {"@type": "Organization", "name": "dread.sh", "url": "https://dread.sh"},
  "publisher": {"@type": "Organization", "name": "dread.sh", "url": "https://dread.sh"}
}
</script>
<script>if(localStorage.getItem('theme')==='light')document.documentElement.classList.add('light')</script>
<link rel="icon" href="data:image/svg+xml,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 100 100'><circle cx='50' cy='50' r='40' fill='%23c37960'/></svg>">
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Press+Start+2P&display=swap">
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/geist@1.3.1/dist/fonts/geist-sans/style.min.css">
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/geist@1.3.1/dist/fonts/geist-mono/style.min.css">
<title>How to Set Up Stripe Webhooks - dread.sh</title>
<script src="https://unpkg.com/lucide@0.469.0/dist/umd/lucide.min.js"></script>
<style>/*! BLOG_CSS */</style>
</head>
<body>
<!-- NAV_HTML -->
<main class="blog-container">
  <article>
    <div class="blog-header">
      <h1>How to Set Up Stripe Webhooks</h1>
      <div class="blog-meta"><time datetime="2026-03-07">March 7, 2026</time> · 7 min read</div>
    </div>
    <div class="blog-content">
      <p><strong>Stripe webhooks notify your application when events happen in your Stripe account</strong> — successful payments, failed charges, subscription changes, disputes, and more. Here is how to set them up from scratch.</p>

      <h2>Step 1: Get a Webhook Endpoint</h2>
      <p>You need a publicly reachable URL that Stripe can send events to. For development and monitoring, the fastest approach is dread:</p>
      <pre><code># Install dread
curl -sSL dread.sh/install | sh

# Create a channel
dread init

# You will get a URL like:
# https://dread.sh/wh/ch_abc123?source=stripe</code></pre>
      <p>For production, your webhook endpoint is a route in your application:</p>
      <pre><code>// Node.js / Express
app.post('/webhooks/stripe', express.raw({type: 'application/json'}), (req, res) => {
  const sig = req.headers['stripe-signature'];
  const endpointSecret = process.env.STRIPE_WEBHOOK_SECRET;

  let event;
  try {
    event = stripe.webhooks.constructEvent(req.body, sig, endpointSecret);
  } catch (err) {
    return res.status(400).send('Webhook signature verification failed');
  }

  switch (event.type) {
    case 'payment_intent.succeeded':
      console.log('Payment succeeded:', event.data.object.id);
      break;
    case 'payment_intent.payment_failed':
      console.log('Payment failed:', event.data.object.id);
      break;
    case 'customer.subscription.deleted':
      console.log('Subscription cancelled:', event.data.object.id);
      break;
  }

  res.status(200).json({received: true});
});</code></pre>

      <h2>Step 2: Configure in the Stripe Dashboard</h2>
      <ol>
        <li>Go to <strong>Developers &rarr; Webhooks</strong> in your Stripe Dashboard</li>
        <li>Click <strong>Add endpoint</strong></li>
        <li>Paste your webhook URL</li>
        <li>Select the events you want to receive (start with the essentials):</li>
      </ol>

      <h3>Essential Events to Monitor</h3>
      <ul>
        <li><code>payment_intent.succeeded</code> — a payment was completed</li>
        <li><code>payment_intent.payment_failed</code> — a payment attempt failed</li>
        <li><code>charge.refunded</code> — a refund was issued</li>
        <li><code>customer.subscription.created</code> — new subscription started</li>
        <li><code>customer.subscription.updated</code> — subscription plan changed</li>
        <li><code>customer.subscription.deleted</code> — subscription cancelled</li>
        <li><code>invoice.payment_failed</code> — recurring payment failed</li>
        <li><code>charge.dispute.created</code> — a chargeback was filed</li>
      </ul>

      <h2>Step 3: Verify Webhook Signatures</h2>
      <p>Always verify the <code>Stripe-Signature</code> header to confirm the event came from Stripe. This prevents attackers from sending fake events to your endpoint.</p>
      <pre><code># Python
import stripe

@app.route('/webhooks/stripe', methods=['POST'])
def stripe_webhook():
    payload = request.data
    sig = request.headers.get('Stripe-Signature')

    try:
        event = stripe.Webhook.construct_event(
            payload, sig, endpoint_secret
        )
    except stripe.error.SignatureVerificationError:
        return 'Invalid signature', 400

    # Process the event
    handle_event(event)
    return '', 200</code></pre>

      <blockquote><p>Never skip signature verification, even in development. It is the only way to confirm that Stripe — not an attacker — sent the event.</p></blockquote>

      <h2>Step 4: Handle Retries and Idempotency</h2>
      <p>Stripe retries failed webhook deliveries for up to 3 days with exponential backoff. Your handler must be idempotent — processing the same event twice should not cause problems.</p>
      <pre><code>// Store processed event IDs to prevent duplicates
const processedEvents = new Set();

function handleEvent(event) {
  if (processedEvents.has(event.id)) {
    return; // Already processed
  }
  processedEvents.add(event.id);
  // ... process the event
}</code></pre>
      <p>In production, use a database instead of an in-memory set.</p>

      <h2>Step 5: Monitor in Real Time</h2>
      <p>Once your webhooks are configured, use dread to monitor them live. You will get desktop notifications for every event and can inspect payloads in the terminal UI:</p>
      <pre><code># Watch for events with desktop notifications
dread watch

# Or open the full terminal UI
dread

# Forward to Slack for team visibility
dread config --slack https://hooks.slack.com/services/xxx</code></pre>

      <h2>Common Stripe Webhook Errors</h2>
      <ul>
        <li><strong>400 — Signature verification failed:</strong> Check that you are using the correct webhook signing secret (not your API key)</li>
        <li><strong>Timeout:</strong> Stripe expects a response within 20 seconds. Move heavy processing to a background job</li>
        <li><strong>Duplicate events:</strong> Stripe may send the same event more than once. Always check idempotency</li>
      </ul>

      <div class="blog-cta">
        <h3>Monitor Stripe webhooks from your terminal</h3>
        <p>Get real-time payment notifications with one command.</p>
        <pre><code>curl -sSL dread.sh/install | sh</code></pre>
      </div>
    </div>
  </article>
</main>
<script>
function toggleTheme() {
  document.documentElement.classList.toggle('light');
  localStorage.setItem('theme', document.documentElement.classList.contains('light') ? 'light' : 'dark');
}
lucide.createIcons();
</script>
</body>
</html>`
