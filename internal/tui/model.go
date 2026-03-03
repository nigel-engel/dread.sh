package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/bubbles/v2/viewport"
	"charm.land/lipgloss/v2"

	"dread.sh/internal/auth"
	"dread.sh/internal/clipboard"
	"dread.sh/internal/event"
	"dread.sh/internal/forward"
	"dread.sh/internal/hub"
	"dread.sh/internal/notify"

	"github.com/coder/websocket"
)

type viewMode int

const (
	viewList viewMode = iota
	viewDetail
)

type Model struct {
	serverURL    string
	channelIDs   []string
	channelNames map[string]string // channel ID -> display name
	webhookURLs  map[string]string // channel ID -> URL
	wsConn       *websocket.Conn
	connected    bool
	err          error

	events []event.Event
	cursor int

	mode     viewMode
	viewport viewport.Model
	detailVP viewport.Model
	width    int
	height   int

	filtering  bool
	filterText string

	hasMore bool
	loading bool

	forwarder      *forward.Forwarder
	lastForwardOK  bool
	lastForwardErr string

	sound  string
	muted  map[string]bool
	now    time.Time
}

func New(serverURL string, channels []auth.Channel, forwardURL string, filter string, sound string, muted []string) Model {
	vp := viewport.New()
	dvp := viewport.New()
	ids := make([]string, len(channels))
	names := make(map[string]string, len(channels))
	for i, ch := range channels {
		ids[i] = ch.ID
		names[ch.ID] = ch.Name
	}
	mutedSet := make(map[string]bool, len(muted))
	for _, m := range muted {
		mutedSet[m] = true
	}
	m := Model{
		serverURL:    serverURL,
		channelIDs:   ids,
		channelNames: names,
		filterText:   filter,
		viewport:     vp,
		detailVP:     dvp,
		sound:        sound,
		muted:        mutedSet,
		now:          time.Now(),
	}
	if forwardURL != "" {
		m.forwarder = forward.New(forwardURL)
	}
	return m
}

// displayName returns the human-readable name for a channel ID.
func (m Model) displayName(channelID string) string {
	if name, ok := m.channelNames[channelID]; ok {
		return name
	}
	return channelID
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(
		connectWS(m.serverURL, m.channelIDs),
		tea.RequestWindowSize,
		tickEvery(),
	)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.viewport.SetWidth(msg.Width)
		m.viewport.SetHeight(msg.Height - m.headerHeight() - 1)
		m.detailVP.SetWidth(msg.Width)
		m.detailVP.SetHeight(msg.Height - m.headerHeight() - 1)
		m.refreshViewport()

	case tickMsg:
		m.now = time.Time(msg)
		m.refreshViewport()
		cmds = append(cmds, tickEvery())

	case tea.KeyPressMsg:
		cmd := m.handleKey(msg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}

	case wsConnectedMsg:
		m.connected = true
		m.err = nil
		m.wsConn = msg.conn
		m.webhookURLs = msg.webhookURLs
		// Copy first webhook URL to clipboard
		for _, url := range msg.webhookURLs {
			cmds = append(cmds, copyToClipboard(url))
			break
		}
		cmds = append(cmds,
			listenWS(msg.conn),
			fetchHistory(m.serverURL, m.channelIDs, time.Time{}, 50),
		)

	case clipboardMsg:
		// silent

	case wsErrorMsg:
		m.connected = false
		m.err = msg.Err
		m.wsConn = nil
		cmds = append(cmds, reconnectAfter(m.serverURL, m.channelIDs, 3*time.Second))

	case newEventMsg:
		if msg.Event.ID != "" {
			m.events = append(m.events, msg.Event)
			filtered := m.filteredEvents()
			if m.cursor >= len(filtered)-2 {
				m.cursor = len(filtered) - 1
			}
			m.refreshViewport()
			if !m.muted[msg.Event.Channel] {
				notify.Send(m.displayName(msg.Event.Channel), msg.Event.Summary, m.sound)
			}
			if m.forwarder != nil {
				cmds = append(cmds, forwardEvent(m.forwarder, &msg.Event))
			}
		}
		if m.wsConn != nil {
			cmds = append(cmds, listenWS(m.wsConn))
		}

	case historyMsg:
		m.hasMore = msg.HasMore
		m.loading = false
		reversed := make([]event.Event, len(msg.Events))
		for i, e := range msg.Events {
			reversed[len(msg.Events)-1-i] = e
		}
		oldLen := len(m.events)
		m.events = append(reversed, m.events...)
		if oldLen == 0 {
			m.cursor = len(m.filteredEvents()) - 1
		} else {
			m.cursor += len(reversed)
		}
		m.refreshViewport()
		if oldLen == 0 {
			m.viewport.GotoBottom()
		}

	case forwardResultMsg:
		if msg.Err != nil {
			m.lastForwardOK = false
			m.lastForwardErr = msg.Err.Error()
		} else {
			m.lastForwardOK = true
			m.lastForwardErr = ""
		}
	}

	if m.mode == viewDetail {
		var cmd tea.Cmd
		m.detailVP, cmd = m.detailVP.Update(msg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	} else {
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}

	if m.mode == viewList && m.viewport.AtTop() && m.hasMore && !m.loading && len(m.events) > 0 {
		m.loading = true
		cmds = append(cmds, fetchHistory(m.serverURL, m.channelIDs, m.events[0].Timestamp, 50))
	}

	return m, tea.Batch(cmds...)
}

func (m *Model) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	key := msg.String()

	if m.filtering {
		switch key {
		case "esc":
			m.filtering = false
			m.filterText = ""
			m.cursor = clamp(m.cursor, 0, len(m.filteredEvents())-1)
			m.refreshViewport()
		case "enter":
			m.filtering = false
			m.cursor = clamp(m.cursor, 0, len(m.filteredEvents())-1)
			m.refreshViewport()
		case "backspace":
			if len(m.filterText) > 0 {
				m.filterText = m.filterText[:len(m.filterText)-1]
				m.cursor = 0
				m.refreshViewport()
			}
		default:
			if len(key) == 1 {
				m.filterText += key
				m.cursor = 0
				m.refreshViewport()
			}
		}
		return nil
	}

	if m.mode == viewDetail {
		switch key {
		case "esc", "q", "backspace":
			m.mode = viewList
			m.refreshViewport()
		case "r":
			if m.forwarder != nil {
				filtered := m.filteredEvents()
				if m.cursor >= 0 && m.cursor < len(filtered) {
					ev := filtered[m.cursor]
					return forwardEvent(m.forwarder, &ev)
				}
			}
		case "ctrl+c":
			return tea.Quit
		}
		return nil
	}

	switch key {
	case "q", "ctrl+c":
		if m.wsConn != nil {
			m.wsConn.CloseNow()
		}
		return tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
			m.refreshViewport()
			m.ensureCursorVisible()
		}
	case "down", "j":
		filtered := m.filteredEvents()
		if m.cursor < len(filtered)-1 {
			m.cursor++
			m.refreshViewport()
			m.ensureCursorVisible()
		}
	case "enter":
		filtered := m.filteredEvents()
		if m.cursor >= 0 && m.cursor < len(filtered) {
			m.mode = viewDetail
			m.renderDetail(filtered[m.cursor])
		}
	case "/":
		m.filtering = true
	case "r":
		if m.forwarder != nil {
			filtered := m.filteredEvents()
			if m.cursor >= 0 && m.cursor < len(filtered) {
				ev := filtered[m.cursor]
				return forwardEvent(m.forwarder, &ev)
			}
		}
	}
	return nil
}

func (m Model) View() tea.View {
	var b strings.Builder

	// Header: title + status
	header := titleStyle.Render(" dread.sh")
	status := "connecting..."
	if m.connected {
		filtered := m.filteredEvents()
		counts := m.sourceCounts()
		status = fmt.Sprintf("connected — %d events", len(filtered))
		if len(counts) > 0 {
			parts := make([]string, 0, len(counts))
			for src, n := range counts {
				parts = append(parts, fmt.Sprintf("%s:%d", src, n))
			}
			status += " (" + strings.Join(parts, " ") + ")"
		}
	}
	if m.err != nil {
		status = "reconnecting..."
	}
	statusText := statusBarStyle.Width(m.width - lipgloss.Width(header)).Render(status)
	b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, header, statusText))
	b.WriteString("\n")

	// Channel webhook URLs
	if len(m.webhookURLs) > 0 {
		for ch, url := range m.webhookURLs {
			name := m.displayName(ch)
			label := urlLabelStyle.Render("  " + name + ": ")
			u := urlStyle.Render(url)
			b.WriteString(label + u + "\n")
		}
	} else if len(m.channelIDs) > 0 {
		b.WriteString(urlLabelStyle.Render("  connecting to channels...") + "\n")
	} else {
		b.WriteString(urlLabelStyle.Render("  no channels — run: dread new <name>") + "\n")
	}

	// Forward status
	if m.forwarder != nil {
		fwd := forwardStyle.Render(fmt.Sprintf("  forwarding to: %s", m.forwarder.URL))
		if m.lastForwardErr != "" {
			fwd += " " + forwardErrStyle.Render("(err: "+m.lastForwardErr+")")
		}
		b.WriteString(fwd + "\n")
	}

	// Viewport
	if m.mode == viewDetail {
		b.WriteString(m.detailVP.View())
	} else {
		b.WriteString(m.viewport.View())
	}
	b.WriteString("\n")

	// Footer
	var footerText string
	if m.filtering {
		footerText = filterPromptStyle.Render("/") + filterTextStyle.Render(m.filterText) + filterPromptStyle.Render("_") + "  esc clear"
	} else if m.mode == viewDetail {
		footerText = "esc back"
		if m.forwarder != nil {
			footerText += " • r replay"
		}
		footerText += " • ↑↓ scroll"
	} else {
		footerText = "q quit • ↑↓/jk navigate • enter detail • / filter"
		if m.forwarder != nil {
			footerText += " • r replay"
		}
	}
	footer := statusBarStyle.Width(m.width).Render(footerText)
	b.WriteString(footer)

	v := tea.NewView(b.String())
	v.AltScreen = true
	return v
}

func (m Model) headerHeight() int {
	h := 1 // title bar
	if len(m.webhookURLs) > 0 {
		h += len(m.webhookURLs)
	} else {
		h += 1 // "connecting..." or "no channels"
	}
	if m.forwarder != nil {
		h++
	}
	return h
}

func (m *Model) refreshViewport() {
	if m.mode == viewList {
		m.viewport.SetContent(m.renderEvents())
	}
}

func (m Model) filteredEvents() []event.Event {
	if m.filterText == "" {
		return m.events
	}
	lower := strings.ToLower(m.filterText)
	var filtered []event.Event
	for _, e := range m.events {
		if strings.Contains(strings.ToLower(e.Summary), lower) ||
			strings.Contains(strings.ToLower(e.Source), lower) ||
			strings.Contains(strings.ToLower(e.Type), lower) ||
			strings.Contains(strings.ToLower(e.Channel), lower) {
			filtered = append(filtered, e)
		}
	}
	return filtered
}

func (m Model) sourceCounts() map[string]int {
	counts := make(map[string]int)
	for _, e := range m.events {
		counts[e.Source]++
	}
	return counts
}

func (m Model) renderEvents() string {
	filtered := m.filteredEvents()
	if len(filtered) == 0 {
		if m.filterText != "" {
			return "\n  No events match filter: " + filterTextStyle.Render(m.filterText)
		}
		if len(m.channelIDs) == 0 {
			return "\n  No channels configured.\n\n  Run: dread new stripe-prod\n  Then paste the webhook URL into your service."
		}
		return "\n  Waiting for events...\n\n  Paste your webhook URL(s) into Stripe, GitHub, or any service."
	}

	var sb strings.Builder
	for i, e := range filtered {
		ts := timestampStyle.Render(fmt.Sprintf("%-8s", relativeTime(e.Timestamp, m.now)))
		src := sourceStyle(e.Source).Width(12).Render(e.Source)
		summary := summaryStyle.Render(e.Summary)
		line := fmt.Sprintf("  %s  %s  %s", ts, src, summary)

		if i == m.cursor {
			line = selectedStyle.Width(m.width).Render(line)
		}
		sb.WriteString(line + "\n")
	}
	return sb.String()
}

func (m *Model) renderDetail(e event.Event) {
	var sb strings.Builder
	sb.WriteString("\n")
	sb.WriteString("  " + detailHeaderStyle.Render(e.Summary) + "\n\n")
	sb.WriteString("  " + detailLabelStyle.Render("ID:        ") + detailValueStyle.Render(e.ID) + "\n")
	sb.WriteString("  " + detailLabelStyle.Render("Channel:   ") + channelStyle.Render(e.Channel) + "\n")
	sb.WriteString("  " + detailLabelStyle.Render("Source:    ") + sourceStyle(e.Source).Render(e.Source) + "\n")
	sb.WriteString("  " + detailLabelStyle.Render("Type:      ") + detailValueStyle.Render(e.Type) + "\n")
	sb.WriteString("  " + detailLabelStyle.Render("Time:      ") + detailValueStyle.Render(e.Timestamp.Local().Format("2006-01-02 15:04:05")) + "\n")
	sb.WriteString("  " + detailLabelStyle.Render("Received:  ") + detailValueStyle.Render(relativeTime(e.Timestamp, m.now)) + "\n\n")
	sb.WriteString("  " + detailLabelStyle.Render("Payload:") + "\n\n")

	jsonStr := PrettyJSON(e.RawJSON)
	for _, line := range strings.Split(jsonStr, "\n") {
		sb.WriteString("    " + line + "\n")
	}

	m.detailVP.SetContent(sb.String())
	m.detailVP.GotoTop()
}

func (m *Model) ensureCursorVisible() {
	if m.cursor < m.viewport.YPosition {
		m.viewport.SetYOffset(m.cursor)
	}
	vpHeight := m.height - m.headerHeight() - 1
	if m.cursor >= m.viewport.YPosition+vpHeight {
		m.viewport.SetYOffset(m.cursor - vpHeight + 1)
	}
}

func relativeTime(t time.Time, now time.Time) string {
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		s := int(d.Seconds())
		if s <= 0 {
			return "now"
		}
		return fmt.Sprintf("%ds ago", s)
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1d ago"
		}
		return fmt.Sprintf("%dd ago", days)
	}
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// --- Commands ---

func connectWS(serverURL string, channels []string) tea.Cmd {
	return func() tea.Msg {
		wsURL := strings.Replace(serverURL, "http://", "ws://", 1)
		wsURL = strings.Replace(wsURL, "https://", "wss://", 1)
		conn, _, err := websocket.Dial(context.Background(), wsURL+"/ws?channels="+strings.Join(channels, ","), nil)
		if err != nil {
			return wsErrorMsg{Err: err}
		}

		_, data, err := conn.Read(context.Background())
		if err != nil {
			conn.CloseNow()
			return wsErrorMsg{Err: err}
		}

		var msg hub.Message
		if err := json.Unmarshal(data, &msg); err != nil {
			conn.CloseNow()
			return wsErrorMsg{Err: fmt.Errorf("bad registration message: %w", err)}
		}

		return wsConnectedMsg{conn: conn, webhookURLs: msg.WebhookURLs}
	}
}

func listenWS(conn *websocket.Conn) tea.Cmd {
	return func() tea.Msg {
		_, data, err := conn.Read(context.Background())
		if err != nil {
			return wsErrorMsg{Err: err}
		}

		var msg hub.Message
		if err := json.Unmarshal(data, &msg); err != nil {
			return newEventMsg{}
		}

		if msg.Type == hub.MsgTypeEvent && msg.Event != nil {
			return newEventMsg{Event: *msg.Event}
		}
		return newEventMsg{}
	}
}

func fetchHistory(serverURL string, channels []string, before time.Time, limit int) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		u := fmt.Sprintf("%s/api/events?channels=%s&limit=%d", serverURL, strings.Join(channels, ","), limit)
		if !before.IsZero() {
			u += "&before=" + before.UTC().Format(time.RFC3339Nano)
		}

		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			return wsErrorMsg{Err: err}
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return wsErrorMsg{Err: err}
		}
		defer resp.Body.Close()

		var msg hub.Message
		if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
			return wsErrorMsg{Err: err}
		}

		return historyMsg{Events: msg.Events, HasMore: msg.HasMore}
	}
}

func reconnectAfter(serverURL string, channels []string, d time.Duration) tea.Cmd {
	return func() tea.Msg {
		time.Sleep(d)
		wsURL := strings.Replace(serverURL, "http://", "ws://", 1)
		wsURL = strings.Replace(wsURL, "https://", "wss://", 1)
		conn, _, err := websocket.Dial(context.Background(), wsURL+"/ws?channels="+strings.Join(channels, ","), nil)
		if err != nil {
			return wsErrorMsg{Err: err}
		}

		_, data, err := conn.Read(context.Background())
		if err != nil {
			conn.CloseNow()
			return wsErrorMsg{Err: err}
		}

		var msg hub.Message
		json.Unmarshal(data, &msg)
		return wsConnectedMsg{conn: conn, webhookURLs: msg.WebhookURLs}
	}
}

func tickEvery() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func copyToClipboard(text string) tea.Cmd {
	return func() tea.Msg {
		err := clipboard.Copy(text)
		return clipboardMsg{Err: err}
	}
}

func forwardEvent(fwd *forward.Forwarder, ev *event.Event) tea.Cmd {
	return func() tea.Msg {
		status, err := fwd.Forward(ev)
		return forwardResultMsg{
			EventID:    ev.ID,
			StatusCode: status,
			Err:        err,
		}
	}
}
