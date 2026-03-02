package main

import (
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"dread.sh/internal/config"
	"dread.sh/internal/event"
	"dread.sh/internal/hub"
	"dread.sh/internal/store"
	"dread.sh/internal/webhook"
)

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

	h := hub.New()

	mux := http.NewServeMux()

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

	// Install tracking — phone-home from install script
	mux.HandleFunc("POST /api/installed", func(w http.ResponseWriter, r *http.Request) {
		db.Increment("installs")
		w.WriteHeader(http.StatusNoContent)
	})

	// Install stats
	mux.HandleFunc("GET /api/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(db.GetStats())
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
		}
		if err := json.Unmarshal(body, &payload); err != nil || payload.Channels == nil {
			http.Error(w, "invalid payload: requires {\"channels\":[...]}", http.StatusBadRequest)
			return
		}
		if err := db.SaveWorkspace(id, string(payload.Channels)); err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			log.Printf("save workspace: %v", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// Workspace API — get workspace
	mux.HandleFunc("GET /api/workspaces/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		channelsJSON, err := db.GetWorkspace(id)
		if err != nil {
			http.Error(w, "workspace not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"channels":` + channelsJSON + `}`))
	})

	// Install script
	mux.HandleFunc("GET /install", func(w http.ResponseWriter, r *http.Request) {
		db.Increment("install_downloads")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(installScript))
	})

	// Documentation page
	mux.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(docsPage))
	})

	// Landing page
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(landingPage))
	})

	server := &http.Server{
		Addr:    cfg.Server.Addr,
		Handler: mux,
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

const landingPage = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<link rel="icon" href="data:image/svg+xml,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 100 100'><circle cx='50' cy='50' r='40' fill='%23c37960'/></svg>">
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/geist@1.3.1/dist/fonts/geist-sans/style.min.css">
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/geist@1.3.1/dist/fonts/geist-mono/style.min.css">
<title>dread — webhooks to desktop notifications</title>
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

  /* ---- NAV ---- */
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
    font-size: 1rem; font-weight: 600; color: var(--text);
    text-decoration: none; letter-spacing: -0.02em;
  }
  .nav-links { display: flex; gap: 24px; align-items: center; }
  .nav-links a {
    font-size: 0.8rem; color: var(--text-muted);
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

  /* ---- HERO ---- */
  .hero {
    max-width: 1080px; margin: 0 auto;
    padding: 100px 24px 80px;
    text-align: center;
    position: relative;
  }
  .badge {
    display: inline-flex; align-items: center; gap: 8px;
    font-size: 0.75rem; color: var(--accent);
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
    font-size: clamp(2.5rem, 6vw, 4rem);
    color: var(--text);
    font-weight: 700;
    letter-spacing: -0.03em;
    line-height: 1.1;
    margin-bottom: 20px;
    position: relative; z-index: 1;
  }
  h1 span { color: var(--accent); }
  .hero-sub {
    font-size: 1.1rem;
    color: var(--text-muted);
    max-width: 560px; margin: 0 auto 40px;
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
    padding: 12px 20px;
    font-size: 0.85rem; color: var(--text);
    font-family: "Geist Mono", ui-monospace, monospace;
    cursor: pointer; transition: border-color 0.15s;
    user-select: none;
  }
  .hero-install:hover { border-color: var(--text-muted); }
  .hero-install .prompt { color: var(--text-dim); }
  .hero-install .pipe { color: var(--text-dim); }

  /* ---- SECTION ---- */
  .section {
    border-top: 1px solid var(--border-subtle);
    border-bottom: 1px solid var(--border-subtle);
  }
  .section-inner {
    max-width: 1080px; margin: 0 auto;
    padding: 80px 24px;
    border-left: 1px solid var(--border-subtle);
    border-right: 1px solid var(--border-subtle);
  }
  .section-label {
    font-size: 0.7rem; text-transform: uppercase;
    letter-spacing: 0.1em; color: var(--accent);
    margin-bottom: 12px;
  }
  .section-title {
    font-size: 1.6rem; color: var(--text);
    font-weight: 600; letter-spacing: -0.02em;
    margin-bottom: 16px;
  }
  .section-desc {
    color: var(--text-muted); font-size: 0.9rem;
    max-width: 600px; line-height: 1.7;
    margin-bottom: 48px;
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
    width: 24px; height: 24px;
    border: 1px solid var(--border);
    border-radius: 6px;
    display: flex; align-items: center; justify-content: center;
    font-size: 0.7rem; color: var(--text-muted);
    flex-shrink: 0;
  }
  .step-label {
    font-size: 0.8rem; color: var(--text-secondary);
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
    font-size: 0.8rem;
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
    gap: 24px;
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
    width: 36px; height: 36px;
    border-radius: 8px;
    display: flex; align-items: center; justify-content: center;
    margin-bottom: 16px;
  }
  .flow-card-icon svg { width: 18px; height: 18px; }
  .flow-card h3 {
    font-size: 0.95rem; color: var(--text);
    font-weight: 600; margin-bottom: 8px;
  }
  .flow-card p {
    font-size: 0.8rem; color: var(--text-muted);
    line-height: 1.6; margin-bottom: 16px;
  }
  .flow-card pre {
    background: var(--bg);
    border: 1px solid var(--border-subtle);
    border-radius: 8px;
    padding: 14px 16px;
    overflow-x: auto;
    font-size: 0.75rem;
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
    padding: 28px;
    transition: background 0.15s;
  }
  .feat:hover { background: var(--surface); }
  .feat-icon {
    width: 32px; height: 32px;
    border-radius: 8px;
    display: flex; align-items: center; justify-content: center;
    margin-bottom: 14px;
  }
  .feat-icon svg { width: 16px; height: 16px; }
  .feat h3 {
    font-size: 0.85rem; color: var(--text);
    font-weight: 500; margin-bottom: 6px;
  }
  .feat p {
    font-size: 0.75rem; color: var(--text-muted);
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
    gap: 24px;
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
    font-size: 0.7rem; text-transform: uppercase;
    letter-spacing: 0.08em;
    color: var(--text-muted);
    border-bottom: 1px solid var(--border);
  }
  .cmd-group pre {
    padding: 16px 20px;
    font-size: 0.75rem;
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
  .footer-brand { font-size: 0.8rem; color: var(--text-dim); }
  .footer-links { display: flex; gap: 20px; }
  .footer-links a {
    font-size: 0.75rem; color: var(--text-dim);
    text-decoration: none; transition: color 0.15s;
  }
  .footer-links a:hover { color: var(--text-secondary); }

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
  .hero-right { position: relative; max-width: 600px; margin: 0 auto; }
  .terminal {
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: 12px;
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
    grid-template-columns: 80px 70px 1fr;
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
  .terminal-row .source-slack { color: #E01E5A; }
  .terminal-row .source-vercel { color: var(--text-secondary); }
  .terminal-footer {
    padding: 10px 16px;
    border-top: 1px solid var(--border);
    color: var(--text-dim);
    font-size: 0.65rem;
  }
</style>
</head>
<body>

<!-- NAV -->
<nav>
  <div class="nav-inner">
    <a href="/" class="nav-brand">dread.sh</a>
    <div class="nav-links">
      <a href="/docs">Documentation</a>
      <button class="nav-btn" onclick="toggleTheme()" aria-label="Toggle theme"><i data-lucide="moon" id="theme-icon"></i></button>
      <a href="https://github.com/nigel-engel/dread.sh" class="nav-btn" aria-label="GitHub"><svg viewBox="0 0 24 24" fill="currentColor"><path d="M12 0C5.37 0 0 5.37 0 12c0 5.31 3.435 9.795 8.205 11.385.6.105.825-.255.825-.57 0-.285-.015-1.23-.015-2.235-3.015.555-3.795-.735-4.035-1.41-.135-.345-.72-1.41-1.23-1.695-.42-.225-1.02-.78-.015-.795.945-.015 1.62.87 1.845 1.23 1.08 1.815 2.805 1.305 3.495.99.105-.78.42-1.305.765-1.605-2.67-.3-5.46-1.335-5.46-5.925 0-1.305.465-2.385 1.23-3.225-.12-.3-.54-1.53.12-3.18 0 0 1.005-.315 3.3 1.23.96-.27 1.98-.405 3-.405s2.04.135 3 .405c2.295-1.56 3.3-1.23 3.3-1.23.66 1.65.24 2.88.12 3.18.765.84 1.23 1.905 1.23 3.225 0 4.605-2.805 5.625-5.475 5.925.435.375.81 1.095.81 2.22 0 1.605-.015 2.895-.015 3.3 0 .315.225.69.825.57A12.02 12.02 0 0024 12c0-6.63-5.37-12-12-12z"/></svg></a>
    </div>
  </div>
</nav>

<!-- HERO -->
<div class="hero">
  <div class="badge"><span class="badge-dot"></span> developer tool for teams</div>
  <h1>Webhooks to your<br>terminal <span>and desktop</span></h1>
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

<!-- QUICK START -->
<div class="section">
<div class="section-inner">
  <div class="section-label">Quick Start</div>
  <div class="section-title">Three commands. That's it.</div>
  <div class="section-desc">Install, create a channel, paste the webhook URL into Stripe / GitHub / Sentry / anything. Desktop notifications start immediately.</div>

  <div class="steps">
    <div class="step-row">
      <div class="step-num"><span class="step-n">1</span><span class="step-label">Install</span></div>
      <div class="step-content">
        <div class="copy-wrap">
          <pre><code>curl -sSL dread.sh/install | sh</code></pre>
          <button class="copy-btn" onclick="copyText('curl -sSL dread.sh/install | sh', this)" type="button"><i data-lucide="copy"></i></button>
        </div>
      </div>
    </div>
    <div class="step-row">
      <div class="step-num"><span class="step-n">2</span><span class="step-label">Create a channel</span></div>
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
      <div class="step-num"><span class="step-n">3</span><span class="step-label">Wire up the webhook</span></div>
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
<div class="section" id="workspace">
<div class="section-inner">
  <div class="section-label">Team Workspaces</div>
  <div class="section-title">Share once, sync forever</div>
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
  <div class="section-title">Everything you need, nothing you don't</div>
  <div class="section-desc">No accounts, no dashboards, no browser tabs. A single binary that does one thing well.</div>

  <div class="feat-grid">
    <div class="feat">
      <div class="feat-icon ic-green"><i data-lucide="bell"></i></div>
      <h3>Desktop notifications</h3>
      <p>Native macOS + Linux. Works in the background, no terminal needed.</p>
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
      <p>Any service that sends webhooks &mdash; just paste the URL.</p>
    </div>
    <div class="feat">
      <div class="feat-icon ic-amber"><i data-lucide="zap"></i></div>
      <h3>Zero config</h3>
      <p>No accounts, no YAML, no environment variables. Just works.</p>
    </div>
  </div>
</div>
</div>

<!-- COMMANDS -->
<div class="section" id="commands">
<div class="section-inner">
  <div class="section-label">Reference</div>
  <div class="section-title">Commands</div>
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
      <pre><code>dread watch                 <span class="c"># headless mode</span>
dread watch --filter stripe <span class="c"># filtered</span></code></pre>
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
    <span class="footer-brand">dread.sh</span>
    <div class="footer-links">
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
    {time:'1h ago', src:'stripe', cls:'source-stripe', msg:'Invoice invoice.paid $249.00'},
    {time:'52m ago', src:'linear', cls:'source-linear', msg:'Issue ENG-481 moved to In Review'},
    {time:'41m ago', src:'slack', cls:'source-slack', msg:'#deploys: Production deploy v2.4.1'},
    {time:'33m ago', src:'sentry', cls:'source-sentry', msg:'ReferenceError: db is not…'},
    {time:'24m ago', src:'github', cls:'source-github', msg:'PR merged #139 → main'},
    {time:'18m ago', src:'vercel', cls:'source-vercel', msg:'Deployment ready (prod)'},
    {time:'9m ago', src:'github', cls:'source-github', msg:'Push to main (3 commits)'},
    {time:'2m ago', src:'stripe', cls:'source-stripe', msg:'Payment charge.succeeded $59.00'},
    {time:'5s ago', src:'sentry', cls:'source-sentry', msg:'TypeError: Cannot read prop…'}
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

const installScript = `#!/bin/sh
set -e

REPO="nigel-engel/dread.sh"
BINARY="dread"
INSTALL_DIR="/usr/local/bin"

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
curl -sL "$URL" -o "$TMPDIR/$TARBALL"
tar -xzf "$TMPDIR/$TARBALL" -C "$TMPDIR"

if [ -w "$INSTALL_DIR" ]; then
  mv "$TMPDIR/$BINARY" "$INSTALL_DIR/$BINARY"
else
  echo "Installing to $INSTALL_DIR (requires sudo)..."
  sudo mv "$TMPDIR/$BINARY" "$INSTALL_DIR/$BINARY"
fi

chmod +x "$INSTALL_DIR/$BINARY"
echo "Installed dread to $INSTALL_DIR/$BINARY"

# Set up background notifications
if [ "$OS" = "darwin" ]; then
  PLIST="$HOME/Library/LaunchAgents/dev.dread.watch.plist"
  mkdir -p "$HOME/Library/LaunchAgents"
  cat > "$PLIST" << 'PLISTEOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>dev.dread.watch</string>
	<key>ProgramArguments</key>
	<array>
		<string>/usr/local/bin/dread</string>
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

elif [ "$OS" = "linux" ]; then
  UNIT_DIR="$HOME/.config/systemd/user"
  mkdir -p "$UNIT_DIR"
  cat > "$UNIT_DIR/dread-watch.service" << 'UNITEOF'
[Unit]
Description=dread webhook notifications
After=network-online.target

[Service]
ExecStart=/usr/local/bin/dread watch
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
echo "Next: dread new \"My Channel\""
`

const docsPage = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<link rel="icon" href="data:image/svg+xml,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 100 100'><circle cx='50' cy='50' r='40' fill='%23c37960'/></svg>">
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/geist@1.3.1/dist/fonts/geist-sans/style.min.css">
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/geist@1.3.1/dist/fonts/geist-mono/style.min.css">
<title>Documentation — dread.sh</title>
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

  /* ---- NAV ---- */
  nav {
    position: fixed; top: 0; left: 0; right: 0; z-index: 100;
    border-bottom: 1px solid var(--border);
    background: var(--nav-bg);
    backdrop-filter: blur(16px);
    -webkit-backdrop-filter: blur(16px);
    height: 56px;
  }
  .nav-inner {
    max-width: 100%; margin: 0; padding: 0 24px; height: 56px;
    display: flex; align-items: center; justify-content: space-between;
  }
  .nav-brand {
    font-size: 1rem; font-weight: 600; color: var(--text);
    text-decoration: none; letter-spacing: -0.02em;
  }
  .nav-links { display: flex; gap: 24px; align-items: center; }
  .nav-links a {
    font-size: 0.8rem; color: var(--text-muted);
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
    max-width: 760px; padding: 48px 48px 120px;
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

  /* ---- MOBILE MENU BUTTON ---- */
  .docs-menu-btn {
    display: none; background: none; border: 1px solid var(--border);
    border-radius: 6px; padding: 6px 8px; cursor: pointer;
    color: var(--text-muted); align-items: center; justify-content: center;
    margin-right: 12px;
  }
  .docs-menu-btn svg { width: 18px; height: 18px; }
  .docs-menu-btn:hover { color: var(--text); background: var(--surface); }

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

<!-- NAV -->
<nav>
  <div class="nav-inner">
    <div style="display:flex;align-items:center;">
      <button class="docs-menu-btn" id="menu-btn" aria-label="Toggle menu"><i data-lucide="menu"></i></button>
      <a href="/" class="nav-brand">dread.sh</a>
    </div>
    <div class="nav-links">
      <a href="/docs">Documentation</a>
      <button class="nav-btn" onclick="toggleTheme()" aria-label="Toggle theme"><i data-lucide="moon" id="theme-icon"></i></button>
      <a href="https://github.com/nigel-engel/dread.sh" class="nav-btn" aria-label="GitHub"><svg viewBox="0 0 24 24" fill="currentColor"><path d="M12 0C5.37 0 0 5.37 0 12c0 5.31 3.435 9.795 8.205 11.385.6.105.825-.255.825-.57 0-.285-.015-1.23-.015-2.235-3.015.555-3.795-.735-4.035-1.41-.135-.345-.72-1.41-1.23-1.695-.42-.225-1.02-.78-.015-.795.945-.015 1.62.87 1.845 1.23 1.08 1.815 2.805 1.305 3.495.99.105-.78.42-1.305.765-1.605-2.67-.3-5.46-1.335-5.46-5.925 0-1.305.465-2.385 1.23-3.225-.12-.3-.54-1.53.12-3.18 0 0 1.005-.315 3.3 1.23.96-.27 1.98-.405 3-.405s2.04.135 3 .405c2.295-1.56 3.3-1.23 3.3-1.23.66 1.65.24 2.88.12 3.18.765.84 1.23 1.905 1.23 3.225 0 4.605-2.805 5.625-5.475 5.925.435.375.81 1.095.81 2.22 0 1.605-.015 2.895-.015 3.3 0 .315.225.69.825.57A12.02 12.02 0 0024 12c0-6.63-5.37-12-12-12z"/></svg></a>
    </div>
  </div>
</nav>

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
      <div class="docs-sidebar-label">CLI Reference</div>
      <a href="#cli-dread">dread (TUI)</a>
      <a href="#cli-new">dread new</a>
      <a href="#cli-list">dread list</a>
      <a href="#cli-logs">dread logs</a>
      <a href="#cli-status">dread status</a>
      <a href="#cli-test">dread test</a>
      <a href="#cli-add-remove">dread add / remove</a>
      <a href="#cli-watch">dread watch</a>
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
    </div>
    <div class="docs-sidebar-group">
      <div class="docs-sidebar-label">Forwarding &amp; Replay</div>
      <a href="#forward">Forward to Localhost</a>
      <a href="#replay">Replay Past Events</a>
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
        <li>Download the <code>dread</code> binary to <code>/usr/local/bin</code></li>
        <li>Set up a background service (<code>launchd</code> on macOS, <code>systemd</code> on Linux) for desktop notifications</li>
        <li>Start listening for webhook events immediately</li>
      </ul>
      <p>Supported platforms: macOS and Linux (amd64 and arm64).</p>
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
      <p>dread auto-detects the following webhook sources and extracts structured event data:</p>
      <table>
        <tr><th>Source</th><th>Detection Header</th><th>Example Summary</th></tr>
        <tr><td>Stripe</td><td><code>Stripe-Signature</code></td><td>invoice.paid $249.00</td></tr>
        <tr><td>GitHub</td><td><code>X-GitHub-Event</code></td><td>PR merged #139 → main</td></tr>
        <tr><td>Shopify</td><td><code>X-Shopify-Topic</code></td><td>orders/create #1042</td></tr>
        <tr><td>Twilio</td><td><code>X-Twilio-Signature</code></td><td>SMS from +1234567890</td></tr>
        <tr><td>SendGrid</td><td><code>X-Twilio-Email-Event-Webhook-Signature</code></td><td>email.delivered</td></tr>
        <tr><td>Slack</td><td><code>X-Slack-Signature</code></td><td>#deploys: Production v2.4</td></tr>
        <tr><td>Discord</td><td><code>X-Signature-Ed25519</code></td><td>interaction.create</td></tr>
        <tr><td>Linear</td><td><code>Linear-Delivery</code></td><td>Issue ENG-481 → In Review</td></tr>
        <tr><td>Svix</td><td><code>Svix-Id</code></td><td>message.created</td></tr>
        <tr><td>Paddle</td><td><code>Paddle-Signature</code></td><td>subscription.activated</td></tr>
      </table>
      <p>Any unrecognized source is labeled "webhook" with the raw event type if available.</p>
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
      <p>To set a custom source name, include the <code>X-Dread-Source</code> header:</p>
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
      <p>dread sends native desktop notifications for every webhook event. The background service (<code>dread watch</code>) runs at login automatically.</p>
      <ul>
        <li><strong>macOS</strong> — uses <code>osascript</code> with sound. Notifications appear in Notification Center.</li>
        <li><strong>Linux</strong> — uses <code>notify-send</code>. Works with any desktop environment that supports freedesktop notifications.</li>
      </ul>
      <p>The install script sets this up as a <code>launchd</code> service (macOS) or <code>systemd</code> user service (Linux) that starts at login.</p>
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
