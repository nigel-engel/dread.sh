package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"dread.sh/internal/auth"
	"dread.sh/internal/event"
	"dread.sh/internal/forward"
	"dread.sh/internal/hub"
	"dread.sh/internal/tui"
	"dread.sh/internal/watch"
)

func main() {
	if len(os.Args) < 2 {
		runTUI()
		return
	}

	switch os.Args[1] {
	case "new":
		cmdNew(os.Args[2:])
	case "add":
		cmdAdd(os.Args[2:])
	case "remove":
		cmdRemove(os.Args[2:])
	case "list":
		cmdList()
	case "watch":
		cmdWatch(os.Args[2:])
	case "share":
		cmdShare(os.Args[2:])
	case "replay":
		cmdReplay(os.Args[2:])
	case "logs":
		cmdLogs(os.Args[2:])
	case "status":
		cmdStatus(os.Args[2:])
	case "test":
		cmdTest(os.Args[2:])
	case "help", "--help", "-h":
		printUsage()
	default:
		if os.Args[1][0] == '-' {
			runTUI()
			return
		}
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`dread — terminal webhook feed

Usage:
  dread                          launch the TUI
  dread new <name>               create a new channel
  dread add <channel-id> <name>  subscribe to an existing channel
  dread remove <channel-id>      unsubscribe from a channel
  dread list                     list subscribed channels
  dread watch                    headless mode — desktop notifications only
  dread share <channel-id>       print a command to share a channel with teammates
  dread replay <event-id>        re-forward a past event to a URL
  dread logs                     print recent events to stdout
  dread status                   show channels, last events, and service status
  dread test <channel-id>        send a test webhook event

Flags (TUI / watch mode):
  --server <url>                 dread server URL (default: https://dread.sh)
  --filter <pattern>             only show events matching pattern

Flags (TUI mode only):
  --forward <url>                forward webhooks to this URL`)
}

func runTUI() {
	fs := flag.NewFlagSet("dread", flag.ExitOnError)
	serverURL := fs.String("server", "https://dread.sh", "dread server URL")
	forwardURL := fs.String("forward", "", "forward webhooks to this URL")
	filter := fs.String("filter", "", "filter events by substring match")
	fs.Parse(os.Args[1:])

	cfg, err := auth.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading config: %v\n", err)
		os.Exit(1)
	}

	if len(cfg.Channels) == 0 {
		fmt.Println("No channels configured. Create one first:")
		fmt.Println()
		fmt.Println("  dread new \"Stripe Prod\"")
		fmt.Println()
		os.Exit(0)
	}

	m := tui.New(*serverURL, cfg.Channels, *forwardURL, *filter)
	p := tea.NewProgram(m)

	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func cmdWatch(args []string) {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	serverURL := fs.String("server", "https://dread.sh", "dread server URL")
	filter := fs.String("filter", "", "filter events by substring match")
	fs.Parse(args)

	if err := watch.Run(*serverURL, *filter); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func cmdNew(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: dread new <name>")
		fmt.Fprintln(os.Stderr, `  e.g. dread new "Stripe Prod"`)
		os.Exit(1)
	}
	name := args[0]

	cfg, err := auth.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	ch := auth.GenerateChannel(name)
	cfg.AddChannel(ch, name)
	if err := cfg.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "error saving config: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Created channel: %s (%s)\n", name, ch)
	fmt.Printf("Webhook URL:     https://dread.sh/wh/%s\n", ch)
	fmt.Println()
	fmt.Println("Paste the webhook URL into your service (Stripe, GitHub, etc.)")
	fmt.Println("Then run: dread")
}

func cmdAdd(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: dread add <channel-id> <name>")
		fmt.Fprintln(os.Stderr, `  e.g. dread add ch_stripe-prod_abc123 "Stripe Prod"`)
		os.Exit(1)
	}
	ch := args[0]
	name := args[1]

	cfg, err := auth.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if !cfg.AddChannel(ch, name) {
		fmt.Println("Already subscribed to", ch)
		return
	}

	if err := cfg.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "error saving config: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Subscribed to: %s (%s)\n", name, ch)
	fmt.Printf("Webhook URL: https://dread.sh/wh/%s\n", ch)
}

func cmdRemove(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: dread remove <channel-id>")
		os.Exit(1)
	}
	ch := args[0]

	cfg, err := auth.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if !cfg.RemoveChannel(ch) {
		fmt.Println("Not subscribed to", ch)
		return
	}

	if err := cfg.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "error saving config: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Unsubscribed from: %s\n", ch)
}

func cmdList() {
	cfg, err := auth.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if len(cfg.Channels) == 0 {
		fmt.Println("No channels. Create one with: dread new <name>")
		return
	}

	fmt.Println("Subscribed channels:")
	for _, ch := range cfg.Channels {
		fmt.Printf("  %-20s  %s\n", ch.Name, ch.ID)
		fmt.Printf("  %-20s  https://dread.sh/wh/%s\n", "", ch.ID)
	}
}

func cmdShare(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: dread share <channel-id>")
		os.Exit(1)
	}
	channelID := args[0]

	cfg, err := auth.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	name := cfg.ChannelName(channelID)

	fmt.Println("Share this with your team:")
	fmt.Printf("  dread add %s %q\n", channelID, name)
}

func cmdReplay(args []string) {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	serverURL := fs.String("server", "https://dread.sh", "dread server URL")
	forwardURL := fs.String("forward", "", "URL to forward the event to")
	fs.Parse(args)

	if fs.NArg() == 0 || *forwardURL == "" {
		fmt.Fprintln(os.Stderr, "usage: dread replay <event-id> --forward <url>")
		os.Exit(1)
	}
	eventID := fs.Arg(0)

	resp, err := http.Get(*serverURL + "/api/events/" + eventID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error fetching event: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "event not found: %s\n", eventID)
		os.Exit(1)
	}

	var ev event.Event
	if err := json.NewDecoder(resp.Body).Decode(&ev); err != nil {
		fmt.Fprintf(os.Stderr, "error decoding event: %v\n", err)
		os.Exit(1)
	}

	fwd := forward.New(*forwardURL)
	status, err := fwd.Forward(&ev)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error forwarding: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Forwarded event %s to %s (status %d)\n", eventID, *forwardURL, status)
}

func cmdLogs(args []string) {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	serverURL := fs.String("server", "https://dread.sh", "dread server URL")
	limit := fs.Int("limit", 20, "number of events to show")
	fs.Parse(args)

	cfg, err := auth.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if len(cfg.Channels) == 0 {
		fmt.Println("No channels. Create one with: dread new <name>")
		return
	}

	ids := cfg.ChannelIDs()
	url := fmt.Sprintf("%s/api/events?channels=%s&limit=%d", *serverURL, strings.Join(ids, ","), *limit)

	resp, err := http.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error fetching events: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var msg hub.Message
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		fmt.Fprintf(os.Stderr, "error decoding response: %v\n", err)
		os.Exit(1)
	}

	if len(msg.Events) == 0 {
		fmt.Println("No events yet.")
		return
	}

	// Events come in reverse chronological order, print oldest first
	for i := len(msg.Events) - 1; i >= 0; i-- {
		e := msg.Events[i]
		name := cfg.ChannelName(e.Channel)
		ts := e.Timestamp.Local().Format("15:04:05")
		fmt.Printf("[%s] [%s] %s\n", ts, name, e.Summary)
	}
}

func cmdStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	serverURL := fs.String("server", "https://dread.sh", "dread server URL")
	fs.Parse(args)

	cfg, err := auth.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// Channels
	if len(cfg.Channels) == 0 {
		fmt.Println("No channels configured.")
	} else {
		fmt.Printf("Channels (%d):\n", len(cfg.Channels))
		for _, ch := range cfg.Channels {
			lastEvent := fetchLastEvent(*serverURL, ch.ID)
			if lastEvent != "" {
				fmt.Printf("  %-20s  %s  (last: %s)\n", ch.Name, ch.ID, lastEvent)
			} else {
				fmt.Printf("  %-20s  %s  (no events)\n", ch.Name, ch.ID)
			}
		}
	}

	// Service status
	fmt.Println()
	fmt.Print("Background service: ")
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("launchctl", "list", "dev.dread.watch").CombinedOutput()
		if err != nil {
			fmt.Println("not running")
		} else {
			if strings.Contains(string(out), "dev.dread.watch") {
				fmt.Println("running (launchd)")
			} else {
				fmt.Println("not running")
			}
		}
	case "linux":
		out, err := exec.Command("systemctl", "--user", "is-active", "dread-watch.service").CombinedOutput()
		if err != nil {
			fmt.Println("not running")
		} else {
			status := strings.TrimSpace(string(out))
			if status == "active" {
				fmt.Println("running (systemd)")
			} else {
				fmt.Printf("not running (%s)\n", status)
			}
		}
	default:
		fmt.Println("unknown (unsupported OS)")
	}
}

func fetchLastEvent(serverURL, channelID string) string {
	url := fmt.Sprintf("%s/api/events?channels=%s&limit=1", serverURL, channelID)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	var msg hub.Message
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		return ""
	}
	if len(msg.Events) == 0 {
		return ""
	}
	return msg.Events[0].Timestamp.Local().Format("2006-01-02 15:04:05")
}

func cmdTest(args []string) {
	fs := flag.NewFlagSet("test", flag.ExitOnError)
	serverURL := fs.String("server", "https://dread.sh", "dread server URL")
	fs.Parse(args)

	if fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: dread test <channel-id>")
		os.Exit(1)
	}
	channelID := fs.Arg(0)

	body := `{"type":"test.event","message":"Test event from dread CLI"}`
	url := *serverURL + "/wh/" + channelID

	req, err := http.NewRequest("POST", url, bytes.NewBufferString(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Dread-Source", "test")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error sending test event: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		fmt.Printf("Test event sent to %s\n", channelID)
	} else {
		fmt.Fprintf(os.Stderr, "server returned %d\n", resp.StatusCode)
		os.Exit(1)
	}
}
