package tui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

const Version = "0.1.0"

const dreadLogo = `      ██████╗ ██████╗ ███████╗ █████╗ ██████╗
      ██╔══██╗██╔══██╗██╔════╝██╔══██╗██╔══██╗
      ██║  ██║██████╔╝█████╗  ███████║██║  ██║
      ██║  ██║██╔══██╗██╔══╝  ██╔══██║██║  ██║
      ██████╔╝██║  ██║███████╗██║  ██║██████╔╝
      ╚═════╝ ╚═╝  ╚═╝╚══════╝╚═╝  ╚═╝╚═════╝`

var sparkBlocks = []rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

var commandTips = []string{
	"dread new <name> — create a channel",
	"dread share — invite your team",
	"dread watch — headless desktop notifications",
	"dread service install — run in background",
	"dread logs — print recent events to stdout",
	"dread status — check channels & service",
	"dread digest — summarize recent activity",
	"dread test <id> — send a test webhook",
	"/ filter · ↑↓ navigate · enter detail",
	"r replay · q quit · c copy URL",
	"dread follow <ws-id> — subscribe to a team",
	"dread mute <id> — silence a noisy channel",
}

// classifyEvent returns "success", "failure", or "neutral" based on event type/summary.
func classifyEvent(typ, summary string) string {
	lower := strings.ToLower(typ + " " + summary)
	for _, kw := range []string{
		"fail", "error", "denied", "declined", "expired", "canceled",
		"cancelled", "refused", "rejected", "dispute", "alert",
		"incident", "critical", "warning", "overdue",
	} {
		if strings.Contains(lower, kw) {
			return "failure"
		}
	}
	for _, kw := range []string{
		"succeed", "success", "completed", "paid", "captured",
		"created", "active", "resolved", "delivered", "merged",
		"approved", "ready",
	} {
		if strings.Contains(lower, kw) {
			return "success"
		}
	}
	return "neutral"
}

func greeting(hour int) string {
	switch {
	case hour < 5:
		return "Burning the midnight oil"
	case hour < 12:
		return "Good morning"
	case hour < 17:
		return "Good afternoon"
	case hour < 21:
		return "Good evening"
	default:
		return "Burning the midnight oil"
	}
}

var (
	logoStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#B5835A")).
			Bold(true)

	timestampStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#666666"))

	sourceStyles = map[string]lipgloss.Style{
		"stripe": lipgloss.NewStyle().
			Foreground(lipgloss.Color("#635BFF")).
			Bold(true),
		"github": lipgloss.NewStyle().
			Foreground(lipgloss.Color("#238636")).
			Bold(true),
		"shopify": lipgloss.NewStyle().
			Foreground(lipgloss.Color("#96BF48")).
			Bold(true),
		"twilio": lipgloss.NewStyle().
			Foreground(lipgloss.Color("#F22F46")).
			Bold(true),
		"slack": lipgloss.NewStyle().
			Foreground(lipgloss.Color("#4A154B")).
			Bold(true),
		"discord": lipgloss.NewStyle().
			Foreground(lipgloss.Color("#5865F2")).
			Bold(true),
		"sendgrid": lipgloss.NewStyle().
			Foreground(lipgloss.Color("#1A82E2")).
			Bold(true),
		"linear": lipgloss.NewStyle().
			Foreground(lipgloss.Color("#5E6AD2")).
			Bold(true),
		"paddle": lipgloss.NewStyle().
			Foreground(lipgloss.Color("#3B58CB")).
			Bold(true),
		"trigger.dev": lipgloss.NewStyle().
			Foreground(lipgloss.Color("#22C55E")).
			Bold(true),
	}

	defaultSourceStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#E5C07B")).
				Bold(true)

	summaryStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#ABB2BF"))

	statusBarStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#999999")).
			Background(lipgloss.Color("#1E1E1E")).
			Padding(0, 1)

	titleStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#C678DD")).
			Bold(true)

	urlStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#98C379")).
			Bold(true)

	urlLabelStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#666666"))

	channelStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#61AFEF"))

	selectedStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("#2C313A"))

	detailHeaderStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#E5C07B")).
				Bold(true)

	detailLabelStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#666666"))

	detailValueStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#ABB2BF"))

	filterPromptStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#C678DD")).
				Bold(true)

	filterTextStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#98C379"))

	forwardStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#56B6C2"))

	forwardErrStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#E06C75"))

	countStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#666666"))

	infoPanelStyle = lipgloss.NewStyle().
			BorderLeft(true).
			BorderStyle(lipgloss.NormalBorder()).
			BorderForeground(lipgloss.Color("#333333")).
			PaddingLeft(2).
			MarginLeft(2)

	tipStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#B5835A"))

	versionStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#444444"))

	connectedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#98C379"))

	dimInfoStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#555555"))

	headerBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#333333")).
			Padding(1, 2)

	greetingStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#ABB2BF"))

	sparkStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#B5835A"))

	updateStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#E5C07B")).
			Bold(true)

	healthActiveStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#98C379"))

	healthStaleStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#555555"))

	successCountStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#98C379"))

	failureCountStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#E06C75"))

	neutralCountStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#666666"))
)

func sourceStyle(source string) lipgloss.Style {
	if s, ok := sourceStyles[source]; ok {
		return s
	}
	return defaultSourceStyle
}
