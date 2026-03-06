package tui

import (
	"charm.land/lipgloss/v2"
)

const dreadLogo = `      ██████╗ ██████╗ ███████╗ █████╗ ██████╗
      ██╔══██╗██╔══██╗██╔════╝██╔══██╗██╔══██╗
      ██║  ██║██████╔╝█████╗  ███████║██║  ██║
      ██║  ██║██╔══██╗██╔══╝  ██╔══██║██║  ██║
      ██████╔╝██║  ██║███████╗██║  ██║██████╔╝
      ╚═════╝ ╚═╝  ╚═╝╚══════╝╚═╝  ╚═╝╚═════╝`

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
)

func sourceStyle(source string) lipgloss.Style {
	if s, ok := sourceStyles[source]; ok {
		return s
	}
	return defaultSourceStyle
}
