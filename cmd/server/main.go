package main

import (
	"encoding/json"
	"flag"
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

	// WebSocket — supports multiple channels
	mux.HandleFunc("GET /ws", h.HandleWS(cfg.Server.BaseURL))

	// Install script
	mux.HandleFunc("GET /install", func(w http.ResponseWriter, r *http.Request) {
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
<title>dread — webhooks in your terminal</title>
<style>
  * { margin: 0; padding: 0; box-sizing: border-box; }
  body {
    font-family: ui-monospace, "Cascadia Code", "Source Code Pro", Menlo, Consolas, monospace;
    background: #0a0a0a; color: #e0e0e0;
    display: flex; justify-content: center;
    padding: 60px 20px;
    line-height: 1.6;
  }
  .container { max-width: 620px; width: 100%; }
  h1 { font-size: 2rem; color: #fff; margin-bottom: 8px; }
  .tagline { color: #888; font-size: 1rem; margin-bottom: 48px; }
  .step { margin-bottom: 32px; }
  .step-label { color: #888; font-size: 0.8rem; margin-bottom: 6px; }
  pre {
    background: #161616; border: 1px solid #282828; border-radius: 8px;
    padding: 16px 20px; overflow-x: auto; font-size: 0.9rem;
  }
  code { color: #f0f0f0; }
  .comment { color: #555; }
  .output { color: #888; }
  .highlight { color: #7ee787; }
  .link {
    color: #888; text-decoration: none; border-bottom: 1px solid #333;
  }
  .link:hover { color: #fff; border-color: #666; }
  .footer { margin-top: 48px; color: #444; font-size: 0.8rem; }
  .footer a { color: #555; text-decoration: none; }
  .footer a:hover { color: #888; }
</style>
</head>
<body>
<div class="container">
  <h1>dread</h1>
  <p class="tagline">webhooks in your terminal</p>

  <div class="step">
    <div class="step-label">install</div>
    <pre><code>curl -sSL dread.sh/install | sh</code></pre>
  </div>

  <div class="step">
    <div class="step-label">create a channel</div>
    <pre><code>$ dread new "Stripe Prod"

<span class="output">Created channel: Stripe Prod (ch_stripe-prod_a1b2c3)
Webhook URL:     <span class="highlight">https://dread.sh/wh/ch_stripe-prod_a1b2c3</span></span></code></pre>
  </div>

  <div class="step">
    <div class="step-label">paste the webhook URL into your service</div>
    <pre><code><span class="comment"># Stripe, GitHub, Slack, Linear, anything that sends webhooks</span></code></pre>
  </div>

  <div class="step">
    <div class="step-label">watch</div>
    <pre><code>$ dread              <span class="comment"># TUI with live feed</span>
$ dread watch        <span class="comment"># headless — desktop notifications only</span></code></pre>
  </div>

  <p class="footer">
    <a href="https://github.com/nigel-engel/dread.sh">github</a>
  </p>
</div>
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
`
