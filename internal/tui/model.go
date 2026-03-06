package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
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

type tabID int

const (
	tabLive tabID = iota
	tabErrors
	tabStats
)

const (
	maxFilterHistory = 20
	maxToasts        = 3
	toastDuration    = 5 * time.Second
)

type toast struct {
	text    string
	expires time.Time
}

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

	filtering     bool
	filterText    string
	filterHistory []string
	filterHistIdx int // -1 = current input

	hasMore bool
	loading bool

	forwarder      *forward.Forwarder
	lastForwardOK  bool
	lastForwardErr string

	sound string
	muted map[string]bool
	now   time.Time

	startedAt     time.Time
	latestVersion string

	// Pause/resume
	paused       bool
	pauseBuffer  []event.Event
	pauseCounter int

	// Tabs
	activeTab tabID

	// Toast notifications
	toasts []toast

	// Help overlay
	showHelp bool

	// Split pane
	splitView bool
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
		filterHistIdx: -1,
		viewport:     vp,
		detailVP:     dvp,
		sound:        sound,
		muted:        mutedSet,
		now:          time.Now(),
		startedAt:    time.Now(),
		activeTab:    tabLive,
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
		checkForUpdate(m.serverURL),
	)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.recalcViewports()
		m.refreshViewport()

	case tickMsg:
		m.now = time.Time(msg)
		// Expire old toasts
		var active []toast
		for _, t := range m.toasts {
			if m.now.Before(t.expires) {
				active = append(active, t)
			}
		}
		m.toasts = active
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
			// Deduplicate: skip if already present
			dup := false
			for _, e := range m.events {
				if e.ID == msg.Event.ID {
					dup = true
					break
				}
			}
			if dup {
				if m.wsConn != nil {
					cmds = append(cmds, listenWS(m.wsConn))
				}
				return m, tea.Batch(cmds...)
			}

			if m.paused {
				// Buffer while paused
				m.pauseBuffer = append(m.pauseBuffer, msg.Event)
				m.pauseCounter++
			} else {
				m.events = append(m.events, msg.Event)
				filtered := m.filteredEvents()
				if m.cursor >= len(filtered)-2 {
					m.cursor = len(filtered) - 1
				}
				m.refreshViewport()
			}

			// Toast for failure events
			if classifyEvent(msg.Event.Type, msg.Event.Summary) == "failure" {
				t := toast{
					text:    fmt.Sprintf("%s: %s", msg.Event.Source, msg.Event.Summary),
					expires: m.now.Add(toastDuration),
				}
				m.toasts = append(m.toasts, t)
				if len(m.toasts) > maxToasts {
					m.toasts = m.toasts[len(m.toasts)-maxToasts:]
				}
			}

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
		// Deduplicate: only prepend events not already present
		seen := make(map[string]bool, len(m.events))
		for _, e := range m.events {
			seen[e.ID] = true
		}
		var fresh []event.Event
		for _, e := range reversed {
			if !seen[e.ID] {
				fresh = append(fresh, e)
			}
		}
		oldLen := len(m.events)
		m.events = append(fresh, m.events...)
		if oldLen == 0 {
			m.cursor = len(m.filteredEvents()) - 1
		} else {
			m.cursor += len(fresh)
		}
		m.refreshViewport()
		if oldLen == 0 {
			m.viewport.GotoBottom()
		}

	case updateCheckMsg:
		if msg.Latest != "" && msg.Latest != Version {
			m.latestVersion = msg.Latest
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

	// Help overlay takes priority
	if m.showHelp {
		switch key {
		case "?", "esc", "q":
			m.showHelp = false
		}
		return nil
	}

	if m.filtering {
		switch key {
		case "esc":
			m.filtering = false
			m.filterText = ""
			m.filterHistIdx = -1
			m.cursor = clamp(m.cursor, 0, len(m.filteredEvents())-1)
			m.refreshViewport()
		case "enter":
			m.filtering = false
			if m.filterText != "" {
				// Save to filter history
				m.filterHistory = append(m.filterHistory, m.filterText)
				if len(m.filterHistory) > maxFilterHistory {
					m.filterHistory = m.filterHistory[len(m.filterHistory)-maxFilterHistory:]
				}
			}
			m.filterHistIdx = -1
			m.cursor = clamp(m.cursor, 0, len(m.filteredEvents())-1)
			m.refreshViewport()
		case "up":
			// Browse filter history
			if len(m.filterHistory) > 0 {
				if m.filterHistIdx == -1 {
					m.filterHistIdx = len(m.filterHistory) - 1
				} else if m.filterHistIdx > 0 {
					m.filterHistIdx--
				}
				m.filterText = m.filterHistory[m.filterHistIdx]
				m.cursor = 0
				m.refreshViewport()
			}
		case "down":
			if m.filterHistIdx >= 0 {
				m.filterHistIdx++
				if m.filterHistIdx >= len(m.filterHistory) {
					m.filterHistIdx = -1
					m.filterText = ""
				} else {
					m.filterText = m.filterHistory[m.filterHistIdx]
				}
				m.cursor = 0
				m.refreshViewport()
			}
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
		case "esc", "backspace":
			m.mode = viewList
			m.refreshViewport()
		case "q":
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
		case "c":
			// Copy payload to clipboard
			filtered := m.filteredEvents()
			if m.cursor >= 0 && m.cursor < len(filtered) {
				ev := filtered[m.cursor]
				return copyToClipboard(PrettyJSON(ev.RawJSON))
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
	case "?":
		m.showHelp = true
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
			if m.splitView {
				// In split view, just update the detail pane
				m.renderDetail(filtered[m.cursor])
			} else {
				m.mode = viewDetail
				m.renderDetail(filtered[m.cursor])
			}
		}
	case "/":
		m.filtering = true
		m.filterHistIdx = -1
	case "r":
		if m.forwarder != nil {
			filtered := m.filteredEvents()
			if m.cursor >= 0 && m.cursor < len(filtered) {
				ev := filtered[m.cursor]
				return forwardEvent(m.forwarder, &ev)
			}
		}
	case "c":
		// Copy webhook URL
		for _, url := range m.webhookURLs {
			return copyToClipboard(url)
		}
	case "p", " ":
		m.paused = !m.paused
		if !m.paused {
			// Flush buffered events
			m.events = append(m.events, m.pauseBuffer...)
			m.pauseBuffer = nil
			m.pauseCounter = 0
			filtered := m.filteredEvents()
			m.cursor = len(filtered) - 1
			m.refreshViewport()
		}
	case "1":
		m.activeTab = tabLive
		m.cursor = clamp(m.cursor, 0, len(m.filteredEvents())-1)
		m.refreshViewport()
	case "2":
		m.activeTab = tabErrors
		m.cursor = clamp(m.cursor, 0, len(m.filteredEvents())-1)
		m.refreshViewport()
	case "3":
		m.activeTab = tabStats
		m.refreshViewport()
	case "s":
		m.splitView = !m.splitView
		m.recalcViewports()
		m.refreshViewport()
		// If enabling split and we have a selection, show its detail
		if m.splitView {
			filtered := m.filteredEvents()
			if m.cursor >= 0 && m.cursor < len(filtered) {
				m.renderDetail(filtered[m.cursor])
			}
		}
	}
	return nil
}

func (m *Model) recalcViewports() {
	vpHeight := m.height - m.headerHeight() - 1
	if m.splitView {
		listW := m.width / 2
		detailW := m.width - listW
		m.viewport.SetWidth(listW)
		m.viewport.SetHeight(vpHeight)
		m.detailVP.SetWidth(detailW)
		m.detailVP.SetHeight(vpHeight)
	} else {
		m.viewport.SetWidth(m.width)
		m.viewport.SetHeight(vpHeight)
		m.detailVP.SetWidth(m.width)
		m.detailVP.SetHeight(vpHeight)
	}
}

func (m Model) View() tea.View {
	// Help overlay
	if m.showHelp {
		v := tea.NewView(m.renderHelp())
		v.AltScreen = true
		return v
	}

	var b strings.Builder

	// Header: three-column layout — logo | stats | activity
	left := logoStyle.Render(dreadLogo)

	// Column 2: status & connection
	var col2Lines []string

	session := m.now.Sub(m.startedAt).Truncate(time.Second)
	greet := greeting(m.now.Hour())
	col2Lines = append(col2Lines, greetingStyle.Render(greet))
	col2Lines = append(col2Lines, dimInfoStyle.Render("session: "+formatDuration(session)))

	if m.connected {
		connLine := connectedStyle.Render("● connected")
		if m.paused {
			connLine += " " + pausedStyle.Render("[PAUSED")
			if m.pauseCounter > 0 {
				connLine += pausedStyle.Render(fmt.Sprintf(" +%d", m.pauseCounter))
			}
			connLine += pausedStyle.Render("]")
		}
		col2Lines = append(col2Lines, connLine)
	} else if m.err != nil {
		col2Lines = append(col2Lines, forwardErrStyle.Render("● reconnecting..."))
	} else {
		col2Lines = append(col2Lines, dimInfoStyle.Render("● connecting..."))
	}

	// Channels with health dots
	col2Lines = append(col2Lines, detailValueStyle.Render(fmt.Sprintf("%d channels ", len(m.channelIDs)))+m.channelHealthDots())

	// Success / failure breakdown
	success, failure, neutral := m.eventStatusCounts()
	statusLine := successCountStyle.Render(fmt.Sprintf("✓ %d", success)) + "  " +
		failureCountStyle.Render(fmt.Sprintf("✗ %d", failure)) + "  " +
		neutralCountStyle.Render(fmt.Sprintf("○ %d", neutral))
	col2Lines = append(col2Lines, statusLine)

	// Version + update
	verLine := versionStyle.Render("v" + Version)
	if m.latestVersion != "" {
		verLine += " " + updateStyle.Render("↑ v"+m.latestVersion)
	}
	col2Lines = append(col2Lines, verLine)

	// Column 3: activity & tips
	var col3Lines []string

	filtered := m.filteredEvents()
	// Event count + source breakdown
	counts := m.sourceCounts()
	evLine := fmt.Sprintf("%d events", len(filtered))
	if len(counts) > 0 {
		parts := make([]string, 0, len(counts))
		for src, n := range counts {
			parts = append(parts, fmt.Sprintf("%s:%d", src, n))
		}
		evLine += " (" + strings.Join(parts, " ") + ")"
	}
	col3Lines = append(col3Lines, detailValueStyle.Render(evLine))

	// Event rate
	if session > 0 {
		rate := float64(len(m.events)) / session.Hours()
		if rate >= 1 {
			col3Lines = append(col3Lines, dimInfoStyle.Render(fmt.Sprintf("~%.0f events/hr", rate)))
		} else {
			col3Lines = append(col3Lines, dimInfoStyle.Render("<1 event/hr"))
		}
	} else {
		col3Lines = append(col3Lines, dimInfoStyle.Render(""))
	}

	// Per-source sparklines (top 3)
	col3Lines = append(col3Lines, m.perSourceSparklines()...)

	// Last event
	if len(m.events) > 0 {
		last := m.events[len(m.events)-1]
		col3Lines = append(col3Lines, dimInfoStyle.Render("last: ")+detailValueStyle.Render(relativeTime(last.Timestamp, m.now)))
	} else {
		col3Lines = append(col3Lines, dimInfoStyle.Render("waiting for first event..."))
	}

	// Rotating tip
	tip := commandTips[int(m.now.Unix()/10)%len(commandTips)]
	col3Lines = append(col3Lines, tipStyle.Render(tip))

	col2 := infoPanelStyle.Render(strings.Join(col2Lines, "\n"))
	col3 := infoPanelStyle.Render(strings.Join(col3Lines, "\n"))
	header := lipgloss.JoinHorizontal(lipgloss.Top, left, col2, col3)
	b.WriteString(headerBoxStyle.Render(header))
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

	// Tab bar
	b.WriteString(m.renderTabBar())
	b.WriteString("\n")

	// Viewport content based on active tab
	if m.activeTab == tabStats {
		b.WriteString(m.renderStats())
	} else if m.mode == viewDetail && !m.splitView {
		b.WriteString(m.detailVP.View())
	} else if m.splitView {
		// Side-by-side: list | detail
		listView := m.viewport.View()
		detailView := m.detailVP.View()
		split := lipgloss.JoinHorizontal(lipgloss.Top, listView, detailView)
		b.WriteString(split)
	} else {
		b.WriteString(m.viewport.View())
	}
	b.WriteString("\n")

	// Toast notifications (overlay at bottom above footer)
	for _, t := range m.toasts {
		b.WriteString(toastStyle.Render("  ⚠ "+t.text) + "\n")
	}

	// Footer
	var footerText string
	if m.filtering {
		mode := "substring"
		if strings.HasPrefix(m.filterText, "!") {
			mode = "exclude"
		} else if strings.Contains(m.filterText, ":") {
			mode = "field"
		}
		footerText = filterPromptStyle.Render("/"+mode+":") + filterTextStyle.Render(m.filterText) + filterPromptStyle.Render("_") + "  esc clear · ↑↓ history"
	} else if m.mode == viewDetail {
		footerText = "esc back · c copy payload"
		if m.forwarder != nil {
			footerText += " · r replay"
		}
		footerText += " · ↑↓ scroll"
	} else {
		footerText = "q quit · ↑↓ navigate · enter detail · / filter · ? help · p pause · s split · 1-3 tabs"
		if m.forwarder != nil {
			footerText += " · r replay"
		}
	}
	footer := statusBarStyle.Width(m.width).Render(footerText)
	b.WriteString(footer)

	v := tea.NewView(b.String())
	v.AltScreen = true
	return v
}

func (m Model) renderTabBar() string {
	tabs := []struct {
		id   tabID
		name string
	}{
		{tabLive, "Live"},
		{tabErrors, "Errors"},
		{tabStats, "Stats"},
	}
	var parts []string
	for _, t := range tabs {
		label := fmt.Sprintf(" %d:%s ", t.id+1, t.name)
		if t.id == m.activeTab {
			parts = append(parts, tabActiveStyle.Render(label))
		} else {
			parts = append(parts, tabInactiveStyle.Render(label))
		}
	}

	tabLine := "  " + strings.Join(parts, " ")
	if m.splitView {
		tabLine += "  " + dimInfoStyle.Render("[split]")
	}
	return tabLine
}

func (m Model) renderHelp() string {
	var b strings.Builder

	// Fill background
	title := helpSectionStyle.Render("  DREAD — Keybindings")
	b.WriteString("\n" + title + "\n\n")

	sections := []struct {
		name string
		keys []struct{ key, desc string }
	}{
		{"Navigation", []struct{ key, desc string }{
			{"↑/k", "Move cursor up"},
			{"↓/j", "Move cursor down"},
			{"enter", "View event detail"},
			{"esc", "Back / clear filter"},
			{"g/G", "Go to top / bottom"},
		}},
		{"Actions", []struct{ key, desc string }{
			{"/", "Filter events"},
			{"r", "Replay event"},
			{"c", "Copy URL / payload"},
			{"p / Space", "Pause / resume feed"},
			{"s", "Toggle split pane"},
			{"?", "Toggle this help"},
			{"q", "Quit"},
		}},
		{"Tabs", []struct{ key, desc string }{
			{"1", "Live events"},
			{"2", "Errors only"},
			{"3", "Stats view"},
		}},
		{"Filter Syntax", []struct{ key, desc string }{
			{"text", "Substring match"},
			{"!text", "Exclude matching"},
			{"source:name", "Filter by source"},
			{"type:name", "Filter by type"},
			{"↑/↓", "Browse filter history"},
		}},
	}

	for _, sec := range sections {
		b.WriteString("  " + helpSectionStyle.Render(sec.name) + "\n")
		for _, k := range sec.keys {
			b.WriteString("  " + helpKeyStyle.Render(k.key) + helpDescStyle.Render(k.desc) + "\n")
		}
		b.WriteString("\n")
	}

	b.WriteString("  " + dimInfoStyle.Render("Press ? or esc to close"))

	// Center in terminal
	content := helpOverlayStyle.Width(50).Render(b.String())
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, content)
}

func (m Model) headerHeight() int {
	h := 11 // logo (6) + box border (2) + box padding (2) + 1 trailing newline
	if len(m.webhookURLs) > 0 {
		h += len(m.webhookURLs)
	} else {
		h += 1 // "connecting..." or "no channels"
	}
	if m.forwarder != nil {
		h++
	}
	h++ // tab bar
	h += len(m.toasts)
	return h
}

func (m *Model) refreshViewport() {
	if m.mode == viewList || m.splitView {
		m.viewport.SetContent(m.renderEvents())
		if m.splitView {
			filtered := m.filteredEvents()
			if m.cursor >= 0 && m.cursor < len(filtered) {
				m.renderDetail(filtered[m.cursor])
			}
		}
	}
}

func (m Model) filteredEvents() []event.Event {
	events := m.events

	// Tab-level filtering
	if m.activeTab == tabErrors {
		var errs []event.Event
		for _, e := range events {
			if classifyEvent(e.Type, e.Summary) == "failure" {
				errs = append(errs, e)
			}
		}
		events = errs
	}

	if m.filterText == "" {
		return events
	}

	filterStr := m.filterText

	// Exclusion filter: !term
	exclude := false
	if strings.HasPrefix(filterStr, "!") {
		exclude = true
		filterStr = filterStr[1:]
	}

	// Field-specific filter: field:value
	var fieldFilter, valueFilter string
	if idx := strings.Index(filterStr, ":"); idx > 0 {
		candidate := strings.ToLower(filterStr[:idx])
		if candidate == "source" || candidate == "type" || candidate == "channel" {
			fieldFilter = candidate
			valueFilter = strings.ToLower(filterStr[idx+1:])
		}
	}

	var filtered []event.Event
	for _, e := range events {
		var match bool
		if fieldFilter != "" {
			switch fieldFilter {
			case "source":
				match = strings.Contains(strings.ToLower(e.Source), valueFilter)
			case "type":
				match = strings.Contains(strings.ToLower(e.Type), valueFilter)
			case "channel":
				match = strings.Contains(strings.ToLower(e.Channel), valueFilter)
			}
		} else {
			lower := strings.ToLower(filterStr)
			match = strings.Contains(strings.ToLower(e.Summary), lower) ||
				strings.Contains(strings.ToLower(e.Source), lower) ||
				strings.Contains(strings.ToLower(e.Type), lower) ||
				strings.Contains(strings.ToLower(e.Channel), lower) ||
				strings.Contains(strings.ToLower(e.RawJSON), lower)
		}

		if exclude {
			if !match {
				filtered = append(filtered, e)
			}
		} else {
			if match {
				filtered = append(filtered, e)
			}
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
		if m.activeTab == tabErrors {
			return "\n  " + successCountStyle.Render("No errors — all clear!")
		}
		if len(m.channelIDs) == 0 {
			return "\n  No channels configured.\n\n  Run: dread new stripe-prod\n  Then paste the webhook URL into your service."
		}
		return "\n  Waiting for events...\n\n  Paste your webhook URL(s) into Stripe, GitHub, or any service."
	}

	var sb strings.Builder
	maxW := m.width
	if m.splitView {
		maxW = m.width / 2
	}
	for i, e := range filtered {
		var dot string
		switch classifyEvent(e.Type, e.Summary) {
		case "success":
			dot = successCountStyle.Render("●")
		case "failure":
			dot = failureCountStyle.Render("●")
		default:
			dot = neutralCountStyle.Render("●")
		}
		ts := timestampStyle.Render(fmt.Sprintf("%-8s", relativeTime(e.Timestamp, m.now)))
		src := sourceStyle(e.Source).Width(12).Render(e.Source)
		summary := summaryStyle.Render(e.Summary)
		line := fmt.Sprintf("  %s %s  %s  %s", dot, ts, src, summary)

		if i == m.cursor {
			line = selectedStyle.Width(maxW).Render(line)
		}
		sb.WriteString(line + "\n")
	}
	return sb.String()
}

func (m Model) renderStats() string {
	var sb strings.Builder
	vpHeight := m.height - m.headerHeight() - 3

	// Source breakdown with bar chart
	sb.WriteString("\n  " + statsLabelStyle.Render("Events by Source") + "\n\n")
	counts := m.sourceCounts()
	type srcCount struct {
		name  string
		count int
	}
	var sorted []srcCount
	maxCount := 0
	for src, n := range counts {
		sorted = append(sorted, srcCount{src, n})
		if n > maxCount {
			maxCount = n
		}
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].count > sorted[j].count })

	barWidth := 30
	if m.width > 80 {
		barWidth = m.width/3 - 20
	}
	for i, sc := range sorted {
		if i >= 15 {
			break
		}
		label := sourceStyle(sc.name).Width(14).Render(sc.name)
		filled := 0
		if maxCount > 0 {
			filled = (sc.count * barWidth) / maxCount
		}
		if filled < 1 && sc.count > 0 {
			filled = 1
		}
		bar := statsBarStyle.Render(strings.Repeat("█", filled)) +
			statsBarBgStyle.Render(strings.Repeat("░", barWidth-filled))
		countStr := dimInfoStyle.Render(fmt.Sprintf(" %d", sc.count))
		sb.WriteString("  " + label + " " + bar + countStr + "\n")
	}

	// Success/failure breakdown
	success, failure, neutralN := m.eventStatusCounts()
	total := success + failure + neutralN
	sb.WriteString("\n  " + statsLabelStyle.Render("Status Breakdown") + "\n\n")
	if total > 0 {
		for _, item := range []struct {
			label string
			count int
			style lipgloss.Style
		}{
			{"success", success, successCountStyle},
			{"failure", failure, failureCountStyle},
			{"neutral", neutralN, neutralCountStyle},
		} {
			filled := 0
			if total > 0 {
				filled = (item.count * barWidth) / total
			}
			if filled < 1 && item.count > 0 {
				filled = 1
			}
			pct := float64(item.count) / float64(total) * 100
			label := item.style.Width(14).Render(item.label)
			bar := item.style.Render(strings.Repeat("█", filled)) +
				statsBarBgStyle.Render(strings.Repeat("░", barWidth-filled))
			countStr := dimInfoStyle.Render(fmt.Sprintf(" %d (%.0f%%)", item.count, pct))
			sb.WriteString("  " + label + " " + bar + countStr + "\n")
		}
	}

	// Activity heatmap (last 7 days x 24 hours)
	sb.WriteString("\n  " + statsLabelStyle.Render("Activity Heatmap (7d × 24h)") + "\n\n")
	sb.WriteString(m.renderHeatmap())

	// Pad remaining height
	lines := strings.Count(sb.String(), "\n")
	for i := lines; i < vpHeight; i++ {
		sb.WriteString("\n")
	}

	return sb.String()
}

func (m Model) renderHeatmap() string {
	// 7 rows (days, today at bottom) x 24 columns (hours)
	var grid [7][24]int
	now := m.now
	for _, e := range m.events {
		age := now.Sub(e.Timestamp)
		if age > 7*24*time.Hour || age < 0 {
			continue
		}
		dayIdx := int(age.Hours() / 24)
		if dayIdx >= 7 {
			continue
		}
		hour := e.Timestamp.Hour()
		grid[dayIdx][hour]++
	}

	// Find max for color scaling
	maxVal := 1
	for d := 0; d < 7; d++ {
		for h := 0; h < 24; h++ {
			if grid[d][h] > maxVal {
				maxVal = grid[d][h]
			}
		}
	}

	var sb strings.Builder
	// Hour labels
	sb.WriteString("          ")
	for h := 0; h < 24; h += 3 {
		sb.WriteString(dimInfoStyle.Render(fmt.Sprintf("%-3d", h)))
	}
	sb.WriteString("\n")

	dayNames := []string{"6d ago", "5d ago", "4d ago", "3d ago", "2d ago", "yesterday", "today   "}
	for d := 6; d >= 0; d-- {
		sb.WriteString("  " + dimInfoStyle.Render(fmt.Sprintf("%-9s", dayNames[6-d])))
		for h := 0; h < 24; h++ {
			val := grid[d][h]
			colorIdx := 0
			if val > 0 && maxVal > 0 {
				colorIdx = (val * (len(heatmapColorStrs) - 1)) / maxVal
				if colorIdx < 1 {
					colorIdx = 1
				}
			}
			cell := lipgloss.NewStyle().Foreground(lipgloss.Color(heatmapColorStrs[colorIdx])).Render("█")
			sb.WriteString(cell)
		}
		sb.WriteString("\n")
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
	sb.WriteString("  " + detailLabelStyle.Render("Received:  ") + detailValueStyle.Render(relativeTime(e.Timestamp, m.now)) + "\n")

	// Status classification
	status := classifyEvent(e.Type, e.Summary)
	var statusStr string
	switch status {
	case "success":
		statusStr = successCountStyle.Render("● success")
	case "failure":
		statusStr = failureCountStyle.Render("● failure")
	default:
		statusStr = neutralCountStyle.Render("● neutral")
	}
	sb.WriteString("  " + detailLabelStyle.Render("Status:    ") + statusStr + "\n\n")

	sb.WriteString("  " + detailLabelStyle.Render("Payload:") + "  " + dimInfoStyle.Render("(c to copy)") + "\n\n")

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

// perSourceSparklines renders sparklines for the top 3 sources.
func (m Model) perSourceSparklines() []string {
	if len(m.events) == 0 {
		return []string{dimInfoStyle.Render("last hour: ") + sparkStyle.Render(m.sparkline())}
	}

	// Count events per source
	counts := make(map[string]int)
	for _, e := range m.events {
		counts[e.Source]++
	}

	// Sort by count descending
	type srcCount struct {
		name  string
		count int
	}
	var sorted []srcCount
	for src, n := range counts {
		sorted = append(sorted, srcCount{src, n})
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].count > sorted[j].count })

	// Show up to 3 per-source sparklines
	maxShow := 3
	if len(sorted) < maxShow {
		maxShow = len(sorted)
	}

	if maxShow <= 1 {
		// Just show aggregate sparkline
		return []string{dimInfoStyle.Render("last hour: ") + sparkStyle.Render(m.sparkline())}
	}

	var lines []string
	for i := 0; i < maxShow; i++ {
		src := sorted[i].name
		spark := m.sparklineForSource(src)
		label := sourceStyle(src).Width(10).Render(src)
		lines = append(lines, label+" "+sparkStyle.Render(spark))
	}
	return lines
}

// sparklineForSource renders a sparkline for a specific source.
func (m Model) sparklineForSource(source string) string {
	const buckets = 12
	bucketDur := 5 * time.Minute
	var counts [buckets]int
	cutoff := m.now.Add(-time.Duration(buckets) * bucketDur)
	for _, e := range m.events {
		if e.Source != source || e.Timestamp.Before(cutoff) {
			continue
		}
		idx := int(m.now.Sub(e.Timestamp) / bucketDur)
		if idx >= buckets {
			idx = buckets - 1
		}
		counts[buckets-1-idx]++
	}
	maxCount := 0
	for _, c := range counts {
		if c > maxCount {
			maxCount = c
		}
	}
	if maxCount == 0 {
		return ""
	}
	var sb strings.Builder
	for _, c := range counts {
		if c == 0 {
			sb.WriteRune(sparkBlocks[0])
		} else {
			idx := (c * (len(sparkBlocks) - 1)) / maxCount
			sb.WriteRune(sparkBlocks[idx])
		}
	}
	return sb.String()
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

func checkForUpdate(serverURL string) tea.Cmd {
	return func() tea.Msg {
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get(serverURL + "/api/version")
		if err != nil {
			return updateCheckMsg{}
		}
		defer resp.Body.Close()
		var v struct {
			Latest string `json:"latest"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
			return updateCheckMsg{}
		}
		return updateCheckMsg{Latest: v.Latest}
	}
}

// sparkline renders a 12-bucket activity graph for the last hour.
func (m Model) sparkline() string {
	if len(m.events) == 0 {
		return ""
	}
	const buckets = 12
	bucketDur := 5 * time.Minute
	var counts [buckets]int
	cutoff := m.now.Add(-time.Duration(buckets) * bucketDur)
	for _, e := range m.events {
		if e.Timestamp.Before(cutoff) {
			continue
		}
		idx := int(m.now.Sub(e.Timestamp) / bucketDur)
		if idx >= buckets {
			idx = buckets - 1
		}
		counts[buckets-1-idx]++
	}
	maxCount := 0
	for _, c := range counts {
		if c > maxCount {
			maxCount = c
		}
	}
	if maxCount == 0 {
		return ""
	}
	var sb strings.Builder
	for _, c := range counts {
		if c == 0 {
			sb.WriteRune(sparkBlocks[0])
		} else {
			idx := (c * (len(sparkBlocks) - 1)) / maxCount
			sb.WriteRune(sparkBlocks[idx])
		}
	}
	return sb.String()
}

// channelHealthDots shows a colored dot per channel based on recent activity.
func (m Model) channelHealthDots() string {
	var sb strings.Builder
	staleThreshold := 30 * time.Minute
	lastEvent := make(map[string]time.Time)
	for _, e := range m.events {
		if t, ok := lastEvent[e.Channel]; !ok || e.Timestamp.After(t) {
			lastEvent[e.Channel] = e.Timestamp
		}
	}
	for _, ch := range m.channelIDs {
		t, ok := lastEvent[ch]
		if ok && m.now.Sub(t) < staleThreshold {
			sb.WriteString(healthActiveStyle.Render("●"))
		} else {
			sb.WriteString(healthStaleStyle.Render("●"))
		}
	}
	return sb.String()
}

// eventStatusCounts classifies events into success, failure, neutral.
func (m Model) eventStatusCounts() (success, failure, neutral int) {
	for _, e := range m.events {
		switch classifyEvent(e.Type, e.Summary) {
		case "success":
			success++
		case "failure":
			failure++
		default:
			neutral++
		}
	}
	return
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	return fmt.Sprintf("%dh %dm", h, m)
}
