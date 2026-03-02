package tui

import (
	"bytes"
	"encoding/json"
	"strings"

	"charm.land/lipgloss/v2"
)

var (
	jsonKeyStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#C678DD"))
	jsonStringStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#98C379"))
	jsonNumberStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#D19A66"))
	jsonBoolStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#56B6C2"))
	jsonNullStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#56B6C2"))
	jsonBraceStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#ABB2BF"))
)

// PrettyJSON formats raw JSON with indentation and syntax highlighting.
func PrettyJSON(raw string) string {
	// First, pretty-print the JSON
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(raw), "", "  "); err != nil {
		// If it's not valid JSON, return as-is
		return raw
	}

	// Tokenize and colorize
	return colorizeJSON(buf.String())
}

func colorizeJSON(formatted string) string {
	var result strings.Builder
	lines := strings.Split(formatted, "\n")

	for i, line := range lines {
		result.WriteString(colorizeLine(line))
		if i < len(lines)-1 {
			result.WriteString("\n")
		}
	}

	return result.String()
}

func colorizeLine(line string) string {
	trimmed := strings.TrimSpace(line)
	indent := line[:len(line)-len(trimmed)]

	if trimmed == "" {
		return line
	}

	var result strings.Builder
	result.WriteString(indent)

	// Handle lines that start with a key: "key": value
	if len(trimmed) > 0 && trimmed[0] == '"' {
		colonIdx := strings.Index(trimmed, "\":")
		if colonIdx > 0 {
			key := trimmed[:colonIdx+1]
			rest := trimmed[colonIdx+1:]

			result.WriteString(jsonKeyStyle.Render(key))

			// The colon
			result.WriteString(jsonBraceStyle.Render(":"))
			rest = rest[1:] // skip the colon

			// The value part
			result.WriteString(colorizeValue(rest))
			return result.String()
		}
	}

	// Not a key-value line, colorize as a value
	result.WriteString(colorizeValue(trimmed))
	return result.String()
}

func colorizeValue(s string) string {
	trimmed := strings.TrimSpace(s)
	prefix := s[:len(s)-len(trimmed)]
	if trimmed == "" {
		return s
	}

	// Remove trailing comma for classification
	trailing := ""
	core := trimmed
	if strings.HasSuffix(core, ",") {
		trailing = ","
		core = core[:len(core)-1]
	}

	var styled string
	switch {
	case core == "{" || core == "}" || core == "[" || core == "]" ||
		core == "{}" || core == "[]":
		styled = jsonBraceStyle.Render(core)
	case core == "true" || core == "false":
		styled = jsonBoolStyle.Render(core)
	case core == "null":
		styled = jsonNullStyle.Render(core)
	case len(core) > 0 && core[0] == '"':
		styled = jsonStringStyle.Render(core)
	case len(core) > 0 && (core[0] >= '0' && core[0] <= '9' || core[0] == '-'):
		styled = jsonNumberStyle.Render(core)
	default:
		styled = core
	}

	if trailing != "" {
		styled += jsonBraceStyle.Render(trailing)
	}

	return prefix + styled
}
