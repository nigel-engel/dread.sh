package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
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
	tabChannels
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

type displayItem struct {
	events []event.Event
}

type paletteAction int

const (
	palFilter paletteAction = iota
	palSplit
	palPause
	palCopyURL
	palLive
	palErrors
	palStats
	palExport
	palBookmarks
	palGroup
	palHelp
)

type palCmd struct {
	name   string
	action paletteAction
}

var paletteCommands = []palCmd{
	{"Filter events", palFilter},
	{"Toggle split pane", palSplit},
	{"Pause / Resume", palPause},
	{"Copy webhook URL", palCopyURL},
	{"Live events", palLive},
	{"Errors only", palErrors},
	{"Statistics", palStats},
	{"Export session as HTML", palExport},
	{"Toggle bookmarks view", palBookmarks},
	{"Toggle event grouping", palGroup},
	{"Show help", palHelp},
}

type Model struct {
	serverURL    string
	channelIDs   []string
	channelNames map[string]string
	webhookURLs  map[string]string
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
	filterHistIdx int

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

	// Bookmarks
	bookmarks     map[string]bool
	showBookmarks bool

	// Forward results per event
	forwardResults map[string]forward.Result

	// Command palette
	showPalette   bool
	paletteInput  string
	paletteCursor int

	// Event grouping
	groupEvents bool

	// Diff view in detail
	showDiff bool

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
		serverURL:      serverURL,
		channelIDs:     ids,
		channelNames:   names,
		filterText:     filter,
		filterHistIdx:  -1,
		viewport:       vp,
		detailVP:       dvp,
		sound:          sound,
		muted:          mutedSet,
		now:            time.Now(),
		startedAt:      time.Now(),
		activeTab:      tabLive,
		bookmarks:      make(map[string]bool),
		forwardResults: make(map[string]forward.Result),
	}
	if forwardURL != "" {
		m.forwarder = forward.New(forwardURL)
	}
	return m
}

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

// displayItems returns grouped or ungrouped event list for rendering.
func (m Model) displayItems() []displayItem {
	filtered := m.filteredEvents()
	if !m.groupEvents || len(filtered) < 3 {
		items := make([]displayItem, len(filtered))
		for i, e := range filtered {
			items[i] = displayItem{events: []event.Event{e}}
		}
		return items
	}

	var items []displayItem
	i := 0
	for i < len(filtered) {
		j := i + 1
		for j < len(filtered) &&
			filtered[j].Source == filtered[i].Source &&
			filtered[j].Type == filtered[i].Type &&
			absDuration(filtered[j].Timestamp.Sub(filtered[i].Timestamp)) < 60*time.Second {
			j++
		}
		if j-i >= 3 {
			group := make([]event.Event, j-i)
			copy(group, filtered[i:j])
			items = append(items, displayItem{events: group})
		} else {
			for k := i; k < j; k++ {
				items = append(items, displayItem{events: []event.Event{filtered[k]}})
			}
		}
		i = j
	}
	return items
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// cursorEvent returns the event under the cursor.
func (m Model) cursorEvent() (event.Event, bool) {
	items := m.displayItems()
	if m.cursor >= 0 && m.cursor < len(items) {
		evs := items[m.cursor].events
		return evs[len(evs)-1], true
	}
	return event.Event{}, false
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

	case tea.MouseClickMsg:
		// mouse disabled

	case wsConnectedMsg:
		m.connected = true
		m.err = nil
		m.wsConn = msg.conn
		m.webhookURLs = msg.webhookURLs
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
				m.pauseBuffer = append(m.pauseBuffer, msg.Event)
				m.pauseCounter++
			} else {
				m.events = append(m.events, msg.Event)
				items := m.displayItems()
				if m.cursor >= len(items)-2 {
					m.cursor = len(items) - 1
				}
				m.refreshViewport()
			}

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
			items := m.displayItems()
			m.cursor = len(items) - 1
		} else {
			// Adjust cursor for prepended items
			oldItems := len(m.displayItems()) // recompute
			_ = oldItems
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
		// Store per-event result
		if msg.EventID != "" {
			m.forwardResults[msg.EventID] = forward.Result{
				StatusCode: msg.StatusCode,
				Headers:    msg.Headers,
				Body:       msg.Body,
				Duration:   msg.Duration,
				Err:        msg.Err,
			}
		}

	case exportDoneMsg:
		if msg.Err != nil {
			m.toasts = append(m.toasts, toast{
				text:    "Export failed: " + msg.Err.Error(),
				expires: m.now.Add(toastDuration),
			})
		} else {
			m.toasts = append(m.toasts, toast{
				text:    "Exported to " + msg.Path,
				expires: m.now.Add(toastDuration),
			})
		}

		// silent
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

func (m *Model) handleMouse(msg tea.MouseClickMsg) tea.Cmd {
	if m.showHelp || m.showPalette || m.filtering {
		return nil
	}
	if m.mode == viewDetail && !m.splitView {
		return nil
	}

	// Calculate which event line was clicked
	hdrH := m.headerHeight()
	if msg.Y >= hdrH && msg.Y < m.height-1 {
		contentLine := msg.Y - hdrH + m.viewport.YOffset()
		items := m.displayItems()
		if contentLine >= 0 && contentLine < len(items) {
			m.cursor = contentLine
			m.refreshViewport()
			// Double-click effect: if already selected, open detail
			if m.splitView {
				if ev, ok := m.cursorEvent(); ok {
					m.renderDetail(ev)
				}
			}
		}
	}
	return nil
}

func (m *Model) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	key := msg.String()

	// Help overlay
	if m.showHelp {
		switch key {
		case "?", "esc", "q":
			m.showHelp = false
		}
		return nil
	}

	// Command palette
	if m.showPalette {
		return m.handlePaletteKey(key)
	}

	// Filter input
	if m.filtering {
		switch key {
		case "esc":
			m.filtering = false
			m.filterText = ""
			m.filterHistIdx = -1
			items := m.displayItems()
			m.cursor = clamp(m.cursor, 0, len(items)-1)
			m.refreshViewport()
		case "enter":
			m.filtering = false
			if m.filterText != "" {
				m.filterHistory = append(m.filterHistory, m.filterText)
				if len(m.filterHistory) > maxFilterHistory {
					m.filterHistory = m.filterHistory[len(m.filterHistory)-maxFilterHistory:]
				}
			}
			m.filterHistIdx = -1
			items := m.displayItems()
			m.cursor = clamp(m.cursor, 0, len(items)-1)
			m.refreshViewport()
		case "up":
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

	// Detail view
	if m.mode == viewDetail {
		switch key {
		case "esc", "backspace":
			m.mode = viewList
			m.showDiff = false
			m.refreshViewport()
		case "q":
			m.mode = viewList
			m.showDiff = false
			m.refreshViewport()
		case "r":
			if m.forwarder != nil {
				if ev, ok := m.cursorEvent(); ok {
					return forwardEvent(m.forwarder, &ev)
				}
			}
		case "c":
			if ev, ok := m.cursorEvent(); ok {
				return copyToClipboard(PrettyJSON(ev.RawJSON))
			}
		case "d":
			m.showDiff = !m.showDiff
			if ev, ok := m.cursorEvent(); ok {
				m.renderDetail(ev)
			}
		case "f":
			if ev, ok := m.cursorEvent(); ok {
				m.bookmarks[ev.ID] = !m.bookmarks[ev.ID]
				if !m.bookmarks[ev.ID] {
					delete(m.bookmarks, ev.ID)
				}
				m.renderDetail(ev)
			}
		case "ctrl+c":
			return tea.Quit
		}
		return nil
	}

	// List view
	switch key {
	case "q", "ctrl+c":
		if m.wsConn != nil {
			m.wsConn.CloseNow()
		}
		return tea.Quit
	case "?":
		m.showHelp = true
	case "ctrl+p":
		m.showPalette = true
		m.paletteInput = ""
		m.paletteCursor = 0
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
			m.refreshViewport()
			m.ensureCursorVisible()
		}
	case "down", "j":
		items := m.displayItems()
		if m.cursor < len(items)-1 {
			m.cursor++
			m.refreshViewport()
			m.ensureCursorVisible()
		}
	case "enter":
		if ev, ok := m.cursorEvent(); ok {
			if m.splitView {
				m.renderDetail(ev)
			} else {
				m.mode = viewDetail
				m.showDiff = false
				m.renderDetail(ev)
			}
		}
	case "/":
		m.filtering = true
		m.filterHistIdx = -1
	case "r":
		if m.forwarder != nil {
			if ev, ok := m.cursorEvent(); ok {
				return forwardEvent(m.forwarder, &ev)
			}
		}
	case "c":
		for _, url := range m.webhookURLs {
			return copyToClipboard(url)
		}
	case "p", " ":
		m.paused = !m.paused
		if !m.paused {
			m.events = append(m.events, m.pauseBuffer...)
			m.pauseBuffer = nil
			m.pauseCounter = 0
			items := m.displayItems()
			m.cursor = len(items) - 1
			m.refreshViewport()
		}
	case "1":
		m.activeTab = tabLive
		items := m.displayItems()
		m.cursor = clamp(m.cursor, 0, len(items)-1)
		m.refreshViewport()
	case "2":
		m.activeTab = tabErrors
		items := m.displayItems()
		m.cursor = clamp(m.cursor, 0, len(items)-1)
		m.refreshViewport()
	case "3":
		m.activeTab = tabStats
		m.refreshViewport()
	case "4":
		m.activeTab = tabChannels
		m.refreshViewport()
	case "s":
		m.splitView = !m.splitView
		m.recalcViewports()
		m.refreshViewport()
		if m.splitView {
			if ev, ok := m.cursorEvent(); ok {
				m.renderDetail(ev)
			}
		}
	case "f":
		if ev, ok := m.cursorEvent(); ok {
			m.bookmarks[ev.ID] = !m.bookmarks[ev.ID]
			if !m.bookmarks[ev.ID] {
				delete(m.bookmarks, ev.ID)
			}
			m.refreshViewport()
		}
	case "F":
		m.showBookmarks = !m.showBookmarks
		items := m.displayItems()
		m.cursor = clamp(m.cursor, 0, len(items)-1)
		m.refreshViewport()
	case "d":
		if ev, ok := m.cursorEvent(); ok {
			m.showDiff = true
			m.mode = viewDetail
			m.renderDetail(ev)
		}
	case "g":
		m.groupEvents = !m.groupEvents
		items := m.displayItems()
		m.cursor = clamp(m.cursor, 0, len(items)-1)
		m.refreshViewport()
	case "x":
		return exportSessionHTML(m.filteredEvents())
	}
	return nil
}

func (m *Model) handlePaletteKey(key string) tea.Cmd {
	switch key {
	case "esc":
		m.showPalette = false
	case "enter":
		m.showPalette = false
		filtered := m.filteredPaletteCommands()
		if m.paletteCursor >= 0 && m.paletteCursor < len(filtered) {
			return m.executePaletteCmd(filtered[m.paletteCursor].action)
		}
	case "up":
		if m.paletteCursor > 0 {
			m.paletteCursor--
		}
	case "down":
		filtered := m.filteredPaletteCommands()
		if m.paletteCursor < len(filtered)-1 {
			m.paletteCursor++
		}
	case "backspace":
		if len(m.paletteInput) > 0 {
			m.paletteInput = m.paletteInput[:len(m.paletteInput)-1]
			m.paletteCursor = 0
		}
	default:
		if len(key) == 1 {
			m.paletteInput += key
			m.paletteCursor = 0
		}
	}
	return nil
}

func (m Model) filteredPaletteCommands() []palCmd {
	if m.paletteInput == "" {
		return paletteCommands
	}
	lower := strings.ToLower(m.paletteInput)
	var result []palCmd
	for _, cmd := range paletteCommands {
		if strings.Contains(strings.ToLower(cmd.name), lower) {
			result = append(result, cmd)
		}
	}
	return result
}

func (m *Model) executePaletteCmd(action paletteAction) tea.Cmd {
	switch action {
	case palFilter:
		m.filtering = true
		m.filterHistIdx = -1
	case palSplit:
		m.splitView = !m.splitView
		m.recalcViewports()
		m.refreshViewport()
	case palPause:
		m.paused = !m.paused
		if !m.paused {
			m.events = append(m.events, m.pauseBuffer...)
			m.pauseBuffer = nil
			m.pauseCounter = 0
			items := m.displayItems()
			m.cursor = len(items) - 1
			m.refreshViewport()
		}
	case palCopyURL:
		for _, url := range m.webhookURLs {
			return copyToClipboard(url)
		}
	case palLive:
		m.activeTab = tabLive
		m.refreshViewport()
	case palErrors:
		m.activeTab = tabErrors
		m.refreshViewport()
	case palStats:
		m.activeTab = tabStats
		m.refreshViewport()
	case palExport:
		return exportSessionHTML(m.filteredEvents())
	case palBookmarks:
		m.showBookmarks = !m.showBookmarks
		m.refreshViewport()
	case palGroup:
		m.groupEvents = !m.groupEvents
		m.refreshViewport()
	case palHelp:
		m.showHelp = true
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

	// Command palette overlay
	if m.showPalette {
		v := tea.NewView(m.renderPalette())
		v.AltScreen = true
		return v
	}

	var b strings.Builder

	// Header
	left := logoStyle.Render(dreadLogo)

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

	col2Lines = append(col2Lines, detailValueStyle.Render(fmt.Sprintf("%d channels", len(m.channelIDs))))

	success, failure, neutral := m.eventStatusCounts()
	statusLine := successCountStyle.Render(fmt.Sprintf("✓ %d", success)) + "  " +
		failureCountStyle.Render(fmt.Sprintf("✗ %d", failure)) + "  " +
		neutralCountStyle.Render(fmt.Sprintf("○ %d", neutral))
	if len(m.bookmarks) > 0 {
		statusLine += "  " + bookmarkStyle.Render(fmt.Sprintf("★ %d", len(m.bookmarks)))
	}
	col2Lines = append(col2Lines, statusLine)

	col2Lines = append(col2Lines, versionStyle.Render("v"+Version))

	var col3Lines []string
	filtered := m.filteredEvents()
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

	if len(m.events) > 0 {
		last := m.events[len(m.events)-1]
		col3Lines = append(col3Lines, dimInfoStyle.Render("last: ")+detailValueStyle.Render(relativeTime(last.Timestamp, m.now)))
	} else {
		col3Lines = append(col3Lines, dimInfoStyle.Render("waiting for first event..."))
	}

	if m.latestVersion != "" {
		col3Lines = append(col3Lines, updateStyle.Render("⬆ Update available: v"+m.latestVersion+" · curl -sSL dread.sh/install | sh"))
	} else {
		tip := commandTips[int(m.startedAt.Unix())%len(commandTips)]
		col3Lines = append(col3Lines, tipStyle.Render(tip))
	}

	col2 := infoPanelStyle.Render(strings.Join(col2Lines, "\n"))
	col3 := infoPanelStyle.Render(strings.Join(col3Lines, "\n"))
	header := lipgloss.JoinHorizontal(lipgloss.Top, left, col2, col3)
	b.WriteString(headerBoxStyle.Render(header))
	b.WriteString("\n")

	// Webhook URLs — show inline if ≤3, otherwise compact summary
	if len(m.webhookURLs) > 0 {
		if len(m.webhookURLs) <= 3 {
			for ch, url := range m.webhookURLs {
				name := m.displayName(ch)
				label := urlLabelStyle.Render("  " + name + ": ")
				u := urlStyle.Render(url)
				b.WriteString(label + u + "\n")
			}
		} else {
			// Compact: just show count and hint
			b.WriteString(urlLabelStyle.Render(fmt.Sprintf("  %d channels", len(m.webhookURLs))) +
				dimInfoStyle.Render(" · press 4 for URLs · c to copy") + "\n")
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
		} else if m.lastForwardOK {
			fwd += " " + fwdStatusOKStyle.Render("✓")
		}
		b.WriteString(fwd + "\n")
	}

	// Tab bar
	b.WriteString(m.renderTabBar())
	b.WriteString("\n")

	// Content
	if m.activeTab == tabChannels {
		b.WriteString(m.renderChannels())
	} else if m.activeTab == tabStats {
		b.WriteString(m.renderStats())
	} else if m.mode == viewDetail && !m.splitView {
		b.WriteString(m.detailVP.View())
	} else if m.splitView {
		listView := m.viewport.View()
		detailView := m.detailVP.View()
		split := lipgloss.JoinHorizontal(lipgloss.Top, listView, detailView)
		b.WriteString(split)
	} else {
		b.WriteString(m.viewport.View())
	}
	b.WriteString("\n")

	// Toasts
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
		footerText = "esc back · c copy · d diff"
		if m.forwarder != nil {
			footerText += " · r replay"
		}
		footerText += " · f bookmark · ↑↓ scroll"
	} else {
		footerText = "q quit · ↑↓ navigate · enter detail · / filter · ? help · Ctrl+P palette"
		if m.showBookmarks {
			footerText = "F bookmarks ON · " + footerText
		}
		if m.groupEvents {
			footerText += " · g:grouped"
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
		{tabChannels, "Channels"},
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
	if m.showBookmarks {
		tabLine += "  " + bookmarkStyle.Render("[★ bookmarks]")
	}
	if m.groupEvents {
		tabLine += "  " + groupBadgeStyle.Render("[grouped]")
	}
	return tabLine
}

func (m Model) renderHelp() string {
	var b strings.Builder

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
		}},
		{"Actions", []struct{ key, desc string }{
			{"/", "Filter events"},
			{"r", "Replay event"},
			{"c", "Copy URL / payload"},
			{"p / Space", "Pause / resume feed"},
			{"s", "Toggle split pane"},
			{"f", "Bookmark / unbookmark event"},
			{"F", "Toggle bookmarks view"},
			{"d", "Diff with previous same-source event"},
			{"g", "Toggle event grouping"},
			{"x", "Export session as HTML"},
			{"Ctrl+P", "Command palette"},
			{"?", "Toggle this help"},
			{"q", "Quit"},
		}},
		{"Tabs", []struct{ key, desc string }{
			{"1", "Live events"},
			{"2", "Errors only"},
			{"3", "Stats + swimlane"},
			{"4", "Channels + URLs"},
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

	content := helpOverlayStyle.Width(50).Render(b.String())
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, content)
}

func (m Model) renderPalette() string {
	var b strings.Builder

	b.WriteString("\n  " + helpSectionStyle.Render("Command Palette") + "\n\n")
	b.WriteString("  " + paletteInputStyle.Render("> "+m.paletteInput) + filterPromptStyle.Render("_") + "\n\n")

	cmds := m.filteredPaletteCommands()
	for i, cmd := range cmds {
		if i >= 10 {
			break
		}
		if i == m.paletteCursor {
			b.WriteString("  " + paletteSelectedStyle.Render("▸ "+cmd.name) + "\n")
		} else {
			b.WriteString("  " + paletteItemStyle.Render("  "+cmd.name) + "\n")
		}
	}

	if len(cmds) == 0 {
		b.WriteString("  " + dimInfoStyle.Render("No matching commands") + "\n")
	}

	b.WriteString("\n  " + dimInfoStyle.Render("↑↓ navigate · enter select · esc close"))

	content := helpOverlayStyle.Width(44).Render(b.String())
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, content)
}

func (m Model) headerHeight() int {
	h := 11
	if len(m.webhookURLs) > 3 {
		h += 1 // compact summary line
	} else if len(m.webhookURLs) > 0 {
		h += len(m.webhookURLs)
	} else {
		h += 1
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
			if ev, ok := m.cursorEvent(); ok {
				m.renderDetail(ev)
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

	// Bookmark filter
	if m.showBookmarks {
		var bookmarked []event.Event
		for _, e := range events {
			if m.bookmarks[e.ID] {
				bookmarked = append(bookmarked, e)
			}
		}
		events = bookmarked
	}

	if m.filterText == "" {
		return events
	}

	filterStr := m.filterText

	exclude := false
	if strings.HasPrefix(filterStr, "!") {
		exclude = true
		filterStr = filterStr[1:]
	}

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
	items := m.displayItems()
	if len(items) == 0 {
		if m.showBookmarks {
			return "\n  " + bookmarkStyle.Render("No bookmarked events") + "\n\n  " + dimInfoStyle.Render("Press f to bookmark an event, F to exit bookmark view")
		}
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
	for i, item := range items {
		ev := item.events[len(item.events)-1]
		isGroup := len(item.events) > 1

		var dot string
		switch classifyEvent(ev.Type, ev.Summary) {
		case "success":
			dot = successCountStyle.Render("●")
		case "failure":
			dot = failureCountStyle.Render("●")
		default:
			dot = neutralCountStyle.Render("●")
		}

		bookmark := " "
		if m.bookmarks[ev.ID] {
			bookmark = bookmarkStyle.Render("★")
		}

		ts := timestampStyle.Render(fmt.Sprintf("%-8s", relativeTime(ev.Timestamp, m.now)))
		src := sourceStyle(ev.Source).Width(12).Render(ev.Source)
		summary := summaryStyle.Render(ev.Summary)

		// Forward result badge
		fwdBadge := ""
		if r, ok := m.forwardResults[ev.ID]; ok {
			if r.Err != nil {
				fwdBadge = " " + fwdStatusErrStyle.Render("→err")
			} else {
				fwdBadge = " " + fwdStatusOKStyle.Render(fmt.Sprintf("→%d", r.StatusCode))
			}
		}

		groupBadge := ""
		if isGroup {
			groupBadge = " " + groupBadgeStyle.Render(fmt.Sprintf("×%d", len(item.events)))
		}

		line := fmt.Sprintf("  %s%s %s  %s  %s%s%s", dot, bookmark, ts, src, summary, fwdBadge, groupBadge)

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

	// Source breakdown
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
		pct := 0
		if maxCount > 0 {
			pct = sc.count * 100 / maxCount
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
		countStr := dimInfoStyle.Render(fmt.Sprintf(" %d (%d%%)", sc.count, pct))
		sb.WriteString("  " + label + " " + bar + countStr + "\n\n")
	}

	// Status breakdown
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

	// Swimlane timeline
	sb.WriteString("\n  " + statsLabelStyle.Render("Swimlane Timeline (last hour)") + "\n\n")
	sb.WriteString(m.renderSwimlane())

	// Heatmap
	sb.WriteString("\n  " + statsLabelStyle.Render("Activity Heatmap (7d × 24h)") + "\n\n")
	sb.WriteString(m.renderHeatmap())

	lines := strings.Count(sb.String(), "\n")
	for i := lines; i < vpHeight; i++ {
		sb.WriteString("\n")
	}

	return sb.String()
}

func (m Model) renderSwimlane() string {
	const lanes = 60 // 60 minutes, 1 char per minute
	now := m.now

	// Get unique sources
	srcSet := make(map[string]bool)
	for _, e := range m.events {
		srcSet[e.Source] = true
	}
	var sources []string
	for s := range srcSet {
		sources = append(sources, s)
	}
	sort.Strings(sources)

	if len(sources) == 0 {
		return "  " + dimInfoStyle.Render("No events in the last hour") + "\n"
	}

	var sb strings.Builder

	// Minute labels
	sb.WriteString("  " + dimInfoStyle.Render(fmt.Sprintf("%-14s", "")) + " ")
	sb.WriteString(dimInfoStyle.Render("-60m"))
	sb.WriteString(strings.Repeat(" ", lanes-8))
	sb.WriteString(dimInfoStyle.Render("now"))
	sb.WriteString("\n")

	for _, src := range sources {
		var buckets [lanes]int
		for _, e := range m.events {
			if e.Source != src {
				continue
			}
			age := now.Sub(e.Timestamp)
			if age < 0 || age > time.Duration(lanes)*time.Minute {
				continue
			}
			idx := lanes - 1 - int(age.Minutes())
			if idx < 0 {
				idx = 0
			}
			if idx >= lanes {
				idx = lanes - 1
			}
			buckets[idx]++
		}

		label := sourceStyle(src).Width(14).Render(src)
		sb.WriteString("  " + label + " ")
		for _, count := range buckets {
			if count > 0 {
				sb.WriteString(swimlaneActiveStyle.Render("▮"))
			} else {
				sb.WriteString(swimlaneEmptyStyle.Render("▯"))
			}
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

func (m Model) renderChannels() string {
	var sb strings.Builder
	vpHeight := m.height - m.headerHeight() - 3

	sb.WriteString("\n  " + statsLabelStyle.Render("Webhook Channels") + "\n\n")

	if len(m.webhookURLs) == 0 {
		if len(m.channelIDs) > 0 {
			sb.WriteString("  " + dimInfoStyle.Render("Connecting to channels...") + "\n")
		} else {
			sb.WriteString("  " + dimInfoStyle.Render("No channels — run: dread new <name>") + "\n")
		}
	} else {
		for ch, url := range m.webhookURLs {
			name := m.displayName(ch)
			sb.WriteString("  " + channelStyle.Render(name) + "\n")
			sb.WriteString("    " + urlStyle.Render(url) + "\n")

			// Show last event time and count for this channel
			var lastTime time.Time
			count := 0
			for _, e := range m.events {
				if e.Channel == ch {
					count++
					if e.Timestamp.After(lastTime) {
						lastTime = e.Timestamp
					}
				}
			}
			if count > 0 {
				sb.WriteString("    " + dimInfoStyle.Render(fmt.Sprintf("%d events · last: %s", count, relativeTime(lastTime, m.now))) + "\n")
			} else {
				sb.WriteString("    " + dimInfoStyle.Render("no events yet") + "\n")
			}
			sb.WriteString("\n")
		}
	}

	// Per-source sparklines
	if len(m.events) > 0 {
		sparkLines := m.perSourceSparklines()
		if len(sparkLines) > 0 {
			sb.WriteString("  " + statsLabelStyle.Render("Source Activity (last hour)") + "\n\n")
			for _, line := range sparkLines {
				sb.WriteString("  " + line + "\n")
			}
			sb.WriteString("\n")
		}
	}

	// Per-channel sparklines
	if len(m.events) > 0 && len(m.webhookURLs) > 0 {
		maxNameLen := 0
		for ch := range m.webhookURLs {
			n := len(m.displayName(ch))
			if n > maxNameLen {
				maxNameLen = n
			}
		}
		if maxNameLen < 8 {
			maxNameLen = 8
		}
		sb.WriteString("  " + statsLabelStyle.Render("Channel Activity (last hour)") + "\n\n")
		for ch := range m.webhookURLs {
			name := m.displayName(ch)
			spark := m.sparklineForChannel(ch)
			label := channelStyle.Width(maxNameLen + 2).Render(name)
			sb.WriteString("  " + label + sparkStyle.Render(spark) + "\n")
		}
	}

	lines := strings.Count(sb.String(), "\n")
	for i := lines; i < vpHeight; i++ {
		sb.WriteString("\n")
	}
	return sb.String()
}

// sparklineForChannel renders a sparkline for a specific channel.
func (m Model) sparklineForChannel(channelID string) string {
	const buckets = 12
	bucketDur := 5 * time.Minute
	var counts [buckets]int
	cutoff := m.now.Add(-time.Duration(buckets) * bucketDur)
	for _, e := range m.events {
		if e.Channel != channelID || e.Timestamp.Before(cutoff) {
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
		return strings.Repeat(string(sparkBlocks[0]), buckets)
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

func (m Model) renderHeatmap() string {
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

	maxVal := 1
	for d := 0; d < 7; d++ {
		for h := 0; h < 24; h++ {
			if grid[d][h] > maxVal {
				maxVal = grid[d][h]
			}
		}
	}

	var sb strings.Builder
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

	// Status
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
	sb.WriteString("  " + detailLabelStyle.Render("Status:    ") + statusStr + "\n")

	// Bookmark indicator
	if m.bookmarks[e.ID] {
		sb.WriteString("  " + detailLabelStyle.Render("Bookmark:  ") + bookmarkStyle.Render("★ bookmarked") + "\n")
	}

	// Forward result
	if r, ok := m.forwardResults[e.ID]; ok {
		sb.WriteString("\n  " + detailHeaderStyle.Render("Forward Response") + "\n\n")
		if r.Err != nil {
			sb.WriteString("  " + detailLabelStyle.Render("Error:     ") + fwdStatusErrStyle.Render(r.Err.Error()) + "\n")
		} else {
			statusColor := fwdStatusOKStyle
			if r.StatusCode >= 400 {
				statusColor = fwdStatusErrStyle
			}
			sb.WriteString("  " + detailLabelStyle.Render("Status:    ") + statusColor.Render(fmt.Sprintf("%d", r.StatusCode)) + "\n")
			sb.WriteString("  " + detailLabelStyle.Render("Duration:  ") + detailValueStyle.Render(r.Duration.String()) + "\n")
			if len(r.Headers) > 0 {
				sb.WriteString("  " + detailLabelStyle.Render("Headers:") + "\n")
				for k, vs := range r.Headers {
					for _, v := range vs {
						sb.WriteString("    " + dimInfoStyle.Render(k+": ") + detailValueStyle.Render(v) + "\n")
					}
				}
			}
			if r.Body != "" {
				sb.WriteString("  " + detailLabelStyle.Render("Body:") + "\n")
				bodyJSON := PrettyJSON(r.Body)
				for _, line := range strings.Split(bodyJSON, "\n") {
					sb.WriteString("    " + line + "\n")
				}
			}
		}
	}

	// Diff view
	if m.showDiff {
		sb.WriteString("\n  " + detailHeaderStyle.Render("Diff with Previous") + "  " + dimInfoStyle.Render("(same source)") + "\n\n")
		diffStr := m.computeDiff(e)
		if diffStr == "" {
			sb.WriteString("  " + dimInfoStyle.Render("No previous event from this source to diff against") + "\n")
		} else {
			for _, line := range strings.Split(diffStr, "\n") {
				sb.WriteString("  " + line + "\n")
			}
		}
	} else {
		// Payload
		sb.WriteString("\n  " + detailLabelStyle.Render("Payload:") + "  " + dimInfoStyle.Render("(c to copy, d to diff)") + "\n\n")
		jsonStr := PrettyJSON(e.RawJSON)
		for _, line := range strings.Split(jsonStr, "\n") {
			sb.WriteString("    " + line + "\n")
		}
	}

	m.detailVP.SetContent(sb.String())
	m.detailVP.GotoTop()
}

// computeDiff compares the current event's JSON with the previous event from the same source.
func (m Model) computeDiff(current event.Event) string {
	// Find previous event from same source
	var prev *event.Event
	for i := len(m.events) - 1; i >= 0; i-- {
		e := m.events[i]
		if e.Source == current.Source && e.ID != current.ID && e.Timestamp.Before(current.Timestamp) {
			prev = &e
			break
		}
	}
	if prev == nil {
		return ""
	}

	oldLines := strings.Split(PrettyJSON(prev.RawJSON), "\n")
	newLines := strings.Split(PrettyJSON(current.RawJSON), "\n")

	var sb strings.Builder
	sb.WriteString(dimInfoStyle.Render(fmt.Sprintf("--- %s (%s)", prev.Type, relativeTime(prev.Timestamp, m.now))) + "\n")
	sb.WriteString(dimInfoStyle.Render(fmt.Sprintf("+++ %s (%s)", current.Type, relativeTime(current.Timestamp, m.now))) + "\n")

	// Simple line-by-line diff
	maxLen := len(oldLines)
	if len(newLines) > maxLen {
		maxLen = len(newLines)
	}

	for i := 0; i < maxLen; i++ {
		var oldLine, newLine string
		if i < len(oldLines) {
			oldLine = oldLines[i]
		}
		if i < len(newLines) {
			newLine = newLines[i]
		}

		if oldLine == newLine {
			sb.WriteString("  " + dimInfoStyle.Render(newLine) + "\n")
		} else {
			if oldLine != "" {
				sb.WriteString(diffRemStyle.Render("- "+oldLine) + "\n")
			}
			if newLine != "" {
				sb.WriteString(diffAddStyle.Render("+ "+newLine) + "\n")
			}
		}
	}

	return sb.String()
}

func (m *Model) ensureCursorVisible() {
	if m.cursor < m.viewport.YOffset() {
		m.viewport.SetYOffset(m.cursor)
	}
	vpHeight := m.height - m.headerHeight() - 1
	if m.cursor >= m.viewport.YOffset()+vpHeight {
		m.viewport.SetYOffset(m.cursor - vpHeight + 1)
	}
}

func (m Model) perSourceSparklines() []string {
	if len(m.events) == 0 {
		return []string{dimInfoStyle.Render("last hour: ") + sparkStyle.Render(m.sparkline())}
	}

	counts := make(map[string]int)
	for _, e := range m.events {
		counts[e.Source]++
	}

	type srcCount struct {
		name  string
		count int
	}
	var sorted []srcCount
	for src, n := range counts {
		sorted = append(sorted, srcCount{src, n})
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].count > sorted[j].count })

	maxShow := 3
	if len(sorted) < maxShow {
		maxShow = len(sorted)
	}

	if maxShow <= 1 {
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
		r := fwd.ForwardFull(ev)
		return forwardResultMsg{
			EventID:    ev.ID,
			StatusCode: r.StatusCode,
			Headers:    r.Headers,
			Body:       r.Body,
			Duration:   r.Duration,
			Err:        r.Err,
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

func exportSessionHTML(events []event.Event) tea.Cmd {
	return func() tea.Msg {
		path := fmt.Sprintf("dread-export-%s.html", time.Now().Format("20060102-150405"))

		var buf bytes.Buffer
		buf.WriteString(`<!DOCTYPE html>
<html><head><meta charset="utf-8"><title>Dread Session Export</title>
<style>
body{font-family:monospace;background:#1e1e1e;color:#abb2bf;padding:2em}
h1{color:#b5835a}
table{border-collapse:collapse;width:100%}
th,td{text-align:left;padding:8px 12px;border-bottom:1px solid #333}
th{color:#c678dd;border-bottom:2px solid #555}
.success{color:#98c379}.failure{color:#e06c75}.neutral{color:#666}
.source{color:#e5c07b;font-weight:bold}
pre{background:#282c34;padding:1em;border-radius:4px;overflow-x:auto;font-size:12px}
details{margin:4px 0}
summary{cursor:pointer;color:#61afef}
</style></head><body>
<h1>Dread Session Export</h1>
<p>`)
		buf.WriteString(fmt.Sprintf("Generated: %s — %d events", time.Now().Format("2006-01-02 15:04:05"), len(events)))
		buf.WriteString(`</p>
<table><tr><th>Time</th><th>Source</th><th>Type</th><th>Summary</th><th>Status</th><th>Payload</th></tr>
`)
		for _, e := range events {
			status := classifyEvent(e.Type, e.Summary)
			statusClass := "neutral"
			if status == "success" {
				statusClass = "success"
			} else if status == "failure" {
				statusClass = "failure"
			}

			var prettyJSON bytes.Buffer
			json.Indent(&prettyJSON, []byte(e.RawJSON), "", "  ")

			buf.WriteString(fmt.Sprintf(
				`<tr><td>%s</td><td class="source">%s</td><td>%s</td><td>%s</td><td class="%s">%s</td><td><details><summary>payload</summary><pre>%s</pre></details></td></tr>`+"\n",
				e.Timestamp.Local().Format("15:04:05"),
				htmlEscape(e.Source),
				htmlEscape(e.Type),
				htmlEscape(e.Summary),
				statusClass, status,
				htmlEscape(prettyJSON.String()),
			))
		}
		buf.WriteString(`</table></body></html>`)

		err := os.WriteFile(path, buf.Bytes(), 0644)
		return exportDoneMsg{Path: path, Err: err}
	}
}

func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}


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
