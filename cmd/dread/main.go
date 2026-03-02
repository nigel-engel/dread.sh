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
	case "follow":
		cmdFollow(os.Args[2:])
	case "unfollow":
		cmdUnfollow(os.Args[2:])
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
	fmt.Println(`dread — webhooks → your team's desktop

Usage:
  dread                          launch the TUI
  dread new <name>               create a new channel
  dread add <channel-id> <name>  subscribe to an existing channel
  dread remove <channel-id>      unsubscribe from a channel
  dread list                     list subscribed channels
  dread watch                    headless mode — desktop notifications only
  dread share                    print your workspace ID to share with teammates
  dread follow <workspace-id>    subscribe to a teammate's workspace
  dread unfollow <workspace-id>  unsubscribe from a workspace
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

	channels := cfg.Channels
	for _, wsID := range cfg.Follows {
		remote, err := resolveWorkspace(*serverURL, wsID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: workspace %s: %v\n", wsID, err)
			continue
		}
		channels = mergeChannels(channels, remote)
	}

	if len(channels) == 0 {
		fmt.Println("No channels configured. Create one first:")
		fmt.Println()
		fmt.Println("  dread new \"Stripe Prod\"")
		fmt.Println()
		os.Exit(0)
	}

	m := tui.New(*serverURL, channels, *forwardURL, *filter)
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

	cfg, err := auth.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if err := watch.Run(*serverURL, *filter, cfg.Follows); err != nil {
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

	if cfg.WorkspaceID == "" {
		cfg.WorkspaceID = auth.GenerateWorkspace()
	}

	if err := cfg.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "error saving config: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Created channel: %s (%s)\n", name, ch)
	fmt.Printf("Webhook URL:     https://dread.sh/wh/%s\n", ch)

	if err := publishWorkspace("https://dread.sh", cfg); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to publish workspace: %v\n", err)
	} else {
		fmt.Println("Workspace published — followers will pick this up automatically")
	}

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

	if cfg.WorkspaceID != "" {
		if err := publishWorkspace("https://dread.sh", cfg); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to update workspace: %v\n", err)
		}
	}
}

func cmdList() {
	cfg, err := auth.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if len(cfg.Channels) == 0 && len(cfg.Follows) == 0 {
		fmt.Println("No channels. Create one with: dread new <name>")
		return
	}

	if len(cfg.Channels) > 0 {
		fmt.Println("Your channels:")
		for _, ch := range cfg.Channels {
			fmt.Printf("  %-20s  %s\n", ch.Name, ch.ID)
			fmt.Printf("  %-20s  https://dread.sh/wh/%s\n", "", ch.ID)
		}
	}

	if cfg.WorkspaceID != "" {
		fmt.Printf("\nWorkspace: %s\n", cfg.WorkspaceID)
	}

	for _, wsID := range cfg.Follows {
		remote, err := resolveWorkspace("https://dread.sh", wsID)
		if err != nil {
			fmt.Printf("\nFollowing %s (error: %v)\n", wsID, err)
			continue
		}
		fmt.Printf("\nFollowing %s (%d channels):\n", wsID, len(remote))
		for _, ch := range remote {
			fmt.Printf("  %-20s  %s\n", ch.Name, ch.ID)
		}
	}
}

func cmdShare(args []string) {
	cfg, err := auth.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if cfg.WorkspaceID == "" {
		if len(cfg.Channels) == 0 {
			fmt.Println("No channels yet. Create one first:")
			fmt.Println("  dread new \"Stripe Prod\"")
			return
		}
		cfg.WorkspaceID = auth.GenerateWorkspace()
		if err := cfg.Save(); err != nil {
			fmt.Fprintf(os.Stderr, "error saving config: %v\n", err)
			os.Exit(1)
		}
		if err := publishWorkspace("https://dread.sh", cfg); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to publish workspace: %v\n", err)
		}
	}

	fmt.Println("Share this with your team:")
	fmt.Printf("  dread follow %s\n", cfg.WorkspaceID)
	fmt.Println()
	fmt.Println("They'll get all your channels (and any you add later).")
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

	channels := cfg.Channels
	for _, wsID := range cfg.Follows {
		remote, err := resolveWorkspace(*serverURL, wsID)
		if err != nil {
			continue
		}
		channels = mergeChannels(channels, remote)
	}

	if len(channels) == 0 {
		fmt.Println("No channels. Create one with: dread new <name>")
		return
	}

	ids := make([]string, len(channels))
	for i, ch := range channels {
		ids[i] = ch.ID
	}
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

	nameByID := make(map[string]string, len(channels))
	for _, ch := range channels {
		nameByID[ch.ID] = ch.Name
	}

	// Events come in reverse chronological order, print oldest first
	for i := len(msg.Events) - 1; i >= 0; i-- {
		e := msg.Events[i]
		name := nameByID[e.Channel]
		if name == "" {
			name = e.Channel
		}
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
	if len(cfg.Channels) == 0 && len(cfg.Follows) == 0 {
		fmt.Println("No channels configured.")
	} else {
		if len(cfg.Channels) > 0 {
			fmt.Printf("Your channels (%d):\n", len(cfg.Channels))
			for _, ch := range cfg.Channels {
				lastEvent := fetchLastEvent(*serverURL, ch.ID)
				if lastEvent != "" {
					fmt.Printf("  %-20s  %s  (last: %s)\n", ch.Name, ch.ID, lastEvent)
				} else {
					fmt.Printf("  %-20s  %s  (no events)\n", ch.Name, ch.ID)
				}
			}
		}
		if cfg.WorkspaceID != "" {
			fmt.Printf("\nWorkspace: %s\n", cfg.WorkspaceID)
		}
		for _, wsID := range cfg.Follows {
			remote, err := resolveWorkspace(*serverURL, wsID)
			if err != nil {
				fmt.Printf("\nFollowing %s (error: %v)\n", wsID, err)
				continue
			}
			fmt.Printf("\nFollowing %s (%d channels):\n", wsID, len(remote))
			for _, ch := range remote {
				lastEvent := fetchLastEvent(*serverURL, ch.ID)
				if lastEvent != "" {
					fmt.Printf("  %-20s  %s  (last: %s)\n", ch.Name, ch.ID, lastEvent)
				} else {
					fmt.Printf("  %-20s  %s  (no events)\n", ch.Name, ch.ID)
				}
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

func cmdFollow(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: dread follow <workspace-id>")
		fmt.Fprintln(os.Stderr, "  e.g. dread follow ws_a1b2c3d4e5f6")
		os.Exit(1)
	}
	wsID := args[0]

	cfg, err := auth.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	for _, existing := range cfg.Follows {
		if existing == wsID {
			fmt.Printf("Already following %s\n", wsID)
			return
		}
	}

	// Verify workspace exists
	channels, err := resolveWorkspace("https://dread.sh", wsID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: could not fetch workspace %s: %v\n", wsID, err)
		os.Exit(1)
	}

	cfg.Follows = append(cfg.Follows, wsID)
	if err := cfg.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "error saving config: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Following workspace %s (%d channels):\n", wsID, len(channels))
	for _, ch := range channels {
		fmt.Printf("  %-20s  %s\n", ch.Name, ch.ID)
	}
	fmt.Println()
	fmt.Println("New channels will sync automatically on reconnect.")
}

func cmdUnfollow(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: dread unfollow <workspace-id>")
		os.Exit(1)
	}
	wsID := args[0]

	cfg, err := auth.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	found := false
	for i, existing := range cfg.Follows {
		if existing == wsID {
			cfg.Follows = append(cfg.Follows[:i], cfg.Follows[i+1:]...)
			found = true
			break
		}
	}

	if !found {
		fmt.Printf("Not following %s\n", wsID)
		return
	}

	if err := cfg.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "error saving config: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Unfollowed workspace %s\n", wsID)
}

func publishWorkspace(serverURL string, cfg *auth.UserConfig) error {
	channelsJSON, err := json.Marshal(cfg.Channels)
	if err != nil {
		return err
	}
	body := fmt.Sprintf(`{"channels":%s}`, channelsJSON)
	req, err := http.NewRequest("PUT", serverURL+"/api/workspaces/"+cfg.WorkspaceID, bytes.NewBufferString(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("server returned %d", resp.StatusCode)
	}
	return nil
}

func resolveWorkspace(serverURL, wsID string) ([]auth.Channel, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(serverURL + "/api/workspaces/" + wsID)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("not found (status %d)", resp.StatusCode)
	}
	var payload struct {
		Channels []auth.Channel `json:"channels"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	return payload.Channels, nil
}

func mergeChannels(local, remote []auth.Channel) []auth.Channel {
	seen := make(map[string]bool, len(local))
	for _, ch := range local {
		seen[ch.ID] = true
	}
	merged := make([]auth.Channel, len(local))
	copy(merged, local)
	for _, ch := range remote {
		if !seen[ch.ID] {
			merged = append(merged, ch)
			seen[ch.ID] = true
		}
	}
	return merged
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
