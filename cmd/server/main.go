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
<link rel="icon" href="data:image/svg+xml,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 100 100'><circle cx='50' cy='50' r='40' fill='%2334d399'/></svg>">
<title>dread — webhooks in your terminal</title>
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
    --accent: oklch(76.5% 0.177 163.22);
    --accent-dim: oklch(50.8% 0.118 165.61);
    --accent-glow: oklch(69.6% 0.17 162.48 / 0.15);
    --accent-glow-strong: oklch(69.6% 0.17 162.48 / 0.3);
    --orange: oklch(75% 0.18 55);
    --orange-dim: oklch(52% 0.16 55);
    --blue: oklch(70.7% 0.165 254.62);
    --violet: oklch(70.2% 0.183 293.54);
    --amber: oklch(82.8% 0.189 84.43);
    --rose: oklch(71.2% 0.194 13.43);
    --cyan: oklch(78.9% 0.154 211.53);
  }

  * { margin: 0; padding: 0; box-sizing: border-box; }

  body {
    font-family: ui-monospace, "Cascadia Code", "Source Code Pro", Menlo, Consolas, "DejaVu Sans Mono", monospace;
    background: var(--bg);
    color: var(--text-secondary);
    line-height: 1.6;
    -webkit-font-smoothing: antialiased;
  }

  /* ---- NAV ---- */
  nav {
    position: sticky; top: 0; z-index: 100;
    border-bottom: 1px solid var(--border);
    background: oklch(10% 0.003 256 / 0.85);
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
  .nav-cta {
    font-size: 0.75rem; color: var(--bg) !important;
    background: var(--accent); padding: 6px 14px;
    border-radius: 6px; font-weight: 500;
    transition: opacity 0.15s;
  }
  .nav-cta:hover { opacity: 0.85; }

  /* ---- HERO ---- */
  .hero {
    max-width: 1080px; margin: 0 auto;
    padding: 100px 24px 80px;
    text-align: center;
    position: relative;
  }
  .hero::before {
    content: "";
    position: absolute; top: 0; left: 50%; transform: translateX(-50%);
    width: 600px; height: 400px;
    background: radial-gradient(ellipse, var(--accent-glow) 0%, transparent 70%);
    pointer-events: none;
    z-index: 0;
  }
  .badge {
    display: inline-flex; align-items: center; gap: 8px;
    font-size: 0.75rem; color: var(--accent);
    border: 1px solid oklch(50.8% 0.118 165.61 / 0.3);
    background: oklch(50.8% 0.118 165.61 / 0.08);
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
    position: relative; z-index: 1;
  }
  .hero-install {
    display: inline-flex; align-items: center; gap: 10px;
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 12px 20px;
    font-size: 0.85rem; color: var(--text);
    font-family: inherit;
    cursor: pointer; transition: border-color 0.15s;
    user-select: all;
  }
  .hero-install:hover { border-color: var(--text-muted); }
  .hero-install .prompt { color: var(--text-dim); }
  .hero-install .pipe { color: var(--text-dim); }

  /* ---- CONTAINER ---- */
  .container {
    max-width: 1080px; margin: 0 auto;
    padding: 0 24px;
  }

  /* ---- SECTION ---- */
  .section {
    padding: 80px 0;
    border-top: 1px solid var(--border-subtle);
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
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: 12px;
    padding: 28px;
    position: relative;
    transition: border-color 0.2s;
  }
  .flow-card:hover { border-color: oklch(32% 0.003 256); }
  .flow-card-icon {
    width: 36px; height: 36px;
    border: 1px solid var(--border);
    border-radius: 8px;
    display: flex; align-items: center; justify-content: center;
    margin-bottom: 16px;
    font-size: 0.9rem;
  }
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
    font-size: 0.85rem;
  }
  .feat h3 {
    font-size: 0.85rem; color: var(--text);
    font-weight: 500; margin-bottom: 6px;
  }
  .feat p {
    font-size: 0.75rem; color: var(--text-muted);
    line-height: 1.6;
  }
  .ic-green { background: oklch(69.6% 0.17 162.48 / 0.12); color: var(--accent); }
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
</style>
</head>
<body>

<!-- NAV -->
<nav>
  <div class="nav-inner">
    <a href="/" class="nav-brand">dread.sh</a>
    <div class="nav-links">
      <a href="#features">Features</a>
      <a href="#commands">Commands</a>
      <a href="https://github.com/nigel-engel/dread.sh">GitHub</a>
      <a href="/install" class="nav-cta">Install</a>
    </div>
  </div>
</nav>

<!-- HERO -->
<div class="hero">
  <div class="badge"><span class="badge-dot"></span> open source CLI tool</div>
  <h1>Webhooks in<br>your <span>terminal</span></h1>
  <p class="hero-sub">Create channels, paste webhook URLs, get native desktop notifications. One command to share your entire workspace with your team.</p>
  <div class="hero-actions">
    <div class="hero-install"><span class="prompt">$</span> curl -sSL dread.sh/install <span class="pipe">|</span> sh</div>
  </div>
</div>

<!-- QUICK START -->
<div class="container">
<div class="section">
  <div class="section-label">Quick Start</div>
  <div class="section-title">Three commands. That's it.</div>
  <div class="section-desc">Install, create a channel, paste the webhook URL into Stripe / GitHub / Sentry / anything. Desktop notifications start immediately.</div>

  <div class="steps">
    <div class="step-row">
      <div class="step-num"><span class="step-n">1</span><span class="step-label">Install</span></div>
      <div class="step-content">
        <pre><code>curl -sSL dread.sh/install | sh</code></pre>
      </div>
    </div>
    <div class="step-row">
      <div class="step-num"><span class="step-n">2</span><span class="step-label">Create a channel</span></div>
      <div class="step-content">
        <pre><code><span class="kw">$</span> dread new "Stripe Prod"

<span class="o">Created channel: Stripe Prod (ch_stripe-prod_a1b2c3)
Webhook URL:     </span><span class="h">https://dread.sh/wh/ch_stripe-prod_a1b2c3</span></code></pre>
      </div>
    </div>
    <div class="step-row">
      <div class="step-num"><span class="step-n">3</span><span class="step-label">Wire up the webhook</span></div>
      <div class="step-content">
        <pre><code><span class="c"># paste the URL into Stripe, GitHub, Slack, Linear...</span>
<span class="c"># notifications start automatically</span>
<span class="kw">$</span> dread <span class="c"># open the TUI anytime</span></code></pre>
      </div>
    </div>
  </div>
</div>

<!-- WORKSPACE FLOW -->
<div class="section" id="workspace">
  <div class="section-label">Team Workspaces</div>
  <div class="section-title">Share once, sync forever</div>
  <div class="section-desc">A workspace is your set of channels. Teammates follow it with one command. Every channel you add later auto-propagates on their next reconnect.</div>

  <div class="flow-grid">
    <div class="flow-card">
      <div class="flow-card-icon ic-green">+</div>
      <h3>Lead creates channels</h3>
      <p>Each <code>dread new</code> auto-publishes your workspace. No extra steps.</p>
      <pre><code><span class="kw">$</span> dread new "Stripe Prod"
<span class="o">Webhook URL: </span><span class="h">https://dread.sh/wh/ch_stripe...</span>
<span class="o">Workspace published</span>

<span class="kw">$</span> dread new "GitHub Deploys"
<span class="o">Webhook URL: </span><span class="h">https://dread.sh/wh/ch_github...</span>
<span class="o">Workspace published</span></code></pre>
    </div>

    <div class="flow-card">
      <div class="flow-card-icon ic-violet">~</div>
      <h3>Share your workspace</h3>
      <p>One ID covers all your channels &mdash; current and future.</p>
      <pre><code><span class="kw">$</span> dread share

<span class="o">Share this with your team:</span>
  <span class="h">dread follow ws_a1b2c3d4e5f6</span>

<span class="o">They'll get all your channels
(and any you add later).</span></code></pre>
    </div>

    <div class="flow-card flow-card-full">
      <div class="flow-inner">
        <div>
          <div class="flow-card-icon ic-blue">&gt;</div>
          <h3>Teammates follow once</h3>
          <p>One command subscribes to every channel in the workspace. New channels sync automatically on reconnect &mdash; no manual adding.</p>
        </div>
        <pre><code><span class="kw">$</span> curl -sSL dread.sh/install | sh
<span class="kw">$</span> dread follow <span class="ws">ws_a1b2c3d4e5f6</span>

<span class="o">Following workspace ws_a1b2... (3 channels):</span>
  <span class="o">Stripe Prod        ch_stripe-prod_a1b2c3</span>
  <span class="o">GitHub Deploys     ch_github-deploys_d4e5f6</span>
  <span class="o">Sentry Alerts      ch_sentry-alerts_g7h8i9</span>

<span class="o">New channels will sync automatically.</span></code></pre>
      </div>
    </div>
  </div>
</div>

<!-- FEATURES -->
<div class="section" id="features">
  <div class="section-label">Features</div>
  <div class="section-title">Everything you need, nothing you don't</div>
  <div class="section-desc">No accounts, no dashboards, no browser tabs. A single binary that does one thing well.</div>

  <div class="feat-grid">
    <div class="feat">
      <div class="feat-icon ic-green">&#9673;</div>
      <h3>Desktop notifications</h3>
      <p>Native macOS + Linux. Works in the background, no terminal needed.</p>
    </div>
    <div class="feat">
      <div class="feat-icon ic-blue">&#9618;</div>
      <h3>Terminal TUI</h3>
      <p>Live feed of all webhook events with full payload inspection.</p>
    </div>
    <div class="feat">
      <div class="feat-icon ic-violet">&#8644;</div>
      <h3>Team workspaces</h3>
      <p>Follow a workspace once. New channels auto-sync on reconnect.</p>
    </div>
    <div class="feat">
      <div class="feat-icon ic-orange">&#9783;</div>
      <h3>Multiple channels</h3>
      <p>Separate channels per service &mdash; Stripe, GitHub, Slack, whatever.</p>
    </div>
    <div class="feat">
      <div class="feat-icon ic-amber">&#8981;</div>
      <h3>Event filtering</h3>
      <p>Filter by source, type, or content in the TUI and watch mode.</p>
    </div>
    <div class="feat">
      <div class="feat-icon ic-cyan">&#8634;</div>
      <h3>Event history</h3>
      <p>Scroll back through past events, stored server-side.</p>
    </div>
    <div class="feat">
      <div class="feat-icon ic-rose">&#8618;</div>
      <h3>Webhook forwarding</h3>
      <p>Forward events to localhost or any URL for local development.</p>
    </div>
    <div class="feat">
      <div class="feat-icon ic-green">&#8635;</div>
      <h3>Event replay</h3>
      <p>Re-forward any past event to a URL for debugging.</p>
    </div>
    <div class="feat">
      <div class="feat-icon ic-blue">&#8645;</div>
      <h3>Auto-reconnect</h3>
      <p>Drops connection? Reconnects in 3s, picks up new channels.</p>
    </div>
    <div class="feat">
      <div class="feat-icon ic-orange">&#9211;</div>
      <h3>Runs at login</h3>
      <p>Installs as a launchd/systemd service. Starts automatically.</p>
    </div>
    <div class="feat">
      <div class="feat-icon ic-violet">&#9881;</div>
      <h3>Works with everything</h3>
      <p>Any service that sends webhooks &mdash; just paste the URL.</p>
    </div>
    <div class="feat">
      <div class="feat-icon ic-amber">&#9671;</div>
      <h3>Zero config</h3>
      <p>No accounts, no YAML, no environment variables. Just works.</p>
    </div>
  </div>
</div>

<!-- COMMANDS -->
<div class="section" id="commands">
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
      <a href="https://github.com/nigel-engel/dread.sh">GitHub</a>
    </div>
  </div>
</footer>

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
