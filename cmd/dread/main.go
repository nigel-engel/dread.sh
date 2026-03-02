package main

import (
	"flag"
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"

	"dread.sh/internal/auth"
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
  dread add <channel-id>         subscribe to an existing channel
  dread remove <channel-id>      unsubscribe from a channel
  dread list                     list subscribed channels
  dread watch                    headless mode — desktop notifications only

Flags (TUI / watch mode):
  --server <url>                 dread server URL (default: https://dread.sh)

Flags (TUI mode only):
  --forward <url>                forward webhooks to this URL

To run at login (macOS):
  cp dev.dread.watch.plist ~/Library/LaunchAgents/
  launchctl load ~/Library/LaunchAgents/dev.dread.watch.plist`)
}

func runTUI() {
	fs := flag.NewFlagSet("dread", flag.ExitOnError)
	serverURL := fs.String("server", "https://dread.sh", "dread server URL")
	forwardURL := fs.String("forward", "", "forward webhooks to this URL")
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

	m := tui.New(*serverURL, cfg.Channels, *forwardURL)
	p := tea.NewProgram(m)

	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func cmdWatch(args []string) {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	serverURL := fs.String("server", "https://dread.sh", "dread server URL")
	fs.Parse(args)

	if err := watch.Run(*serverURL); err != nil {
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
