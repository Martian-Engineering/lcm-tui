package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/reflow/wordwrap"
)

type screen int

const (
	screenAgents screen = iota
	screenSessions
	screenConversation
	screenSummaries
	screenFiles
	screenContext
)

const (
	sessionInitialLoadSize = 50
	sessionBatchLoadSize   = 50
)

// model tracks TUI state across all navigation levels.
type model struct {
	screen screen
	paths  appDataPaths

	agents            []agentEntry
	sessionFiles      []sessionFileEntry
	sessionFileCursor int
	sessions          []sessionEntry
	messages          []sessionMessage
	summary           summaryGraph
	summaryRows       []summaryRow

	largeFiles []largeFileEntry
	fileCursor int

	contextItems  []contextItemEntry
	contextCursor int

	agentCursor         int
	sessionCursor       int
	summaryCursor       int
	summaryDetailScroll int
	contextDetailScroll int

	convViewport viewport.Model
	width        int
	height       int

	summarySources   map[string][]summarySource
	summarySourceErr map[string]string

	status string
}

var (
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("69"))
	helpStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))

	selectedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("230")).Background(lipgloss.Color("62"))

	roleUserStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	roleAssistantStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	roleSystemStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("220"))
	roleToolStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "repair" {
		if err := runRepairCommand(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "lcm-tui repair failed: %v\n", err)
			os.Exit(1)
		}
		return
	}

	m := newModel()
	program := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := program.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "openclaw-tui failed: %v\n", err)
		os.Exit(1)
	}
}

func newModel() model {
	m := model{
		screen:           screenAgents,
		summarySources:   make(map[string][]summarySource),
		summarySourceErr: make(map[string]string),
	}

	paths, err := resolveDataPaths()
	if err != nil {
		m.status = "Error: " + err.Error()
		return m
	}
	m.paths = paths

	agents, err := loadAgents(paths.agentsDir)
	if err != nil {
		m.status = "Error: " + err.Error()
		return m
	}
	m.agents = agents
	m.status = fmt.Sprintf("Loaded %d agents from %s", len(agents), paths.agentsDir)
	return m
}

func (m model) Init() tea.Cmd {
	return nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.resizeViewport()
		m.refreshConversationViewport()
		return m, nil
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" || msg.String() == "q" {
			return m, tea.Quit
		}
		return m.handleKey(msg)
	}
	return m, nil
}

func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.screen {
	case screenAgents:
		return m.handleAgentsKey(msg)
	case screenSessions:
		return m.handleSessionsKey(msg)
	case screenConversation:
		return m.handleConversationKey(msg)
	case screenSummaries:
		return m.handleSummariesKey(msg)
	case screenFiles:
		return m.handleFilesKey(msg)
	case screenContext:
		return m.handleContextKey(msg)
	default:
		return m, nil
	}
}

func (m model) handleAgentsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		m.agentCursor = clamp(m.agentCursor-1, 0, len(m.agents)-1)
	case "down", "j":
		m.agentCursor = clamp(m.agentCursor+1, 0, len(m.agents)-1)
	case "enter":
		if len(m.agents) == 0 {
			m.status = "No agents found"
			return m, nil
		}
		agent := m.agents[m.agentCursor]
		if err := m.loadInitialSessions(agent); err != nil {
			m.status = "Error: " + err.Error()
			return m, nil
		}
		m.sessionCursor = 0
		m.messages = nil
		m.summary = summaryGraph{}
		m.summaryRows = nil
		m.screen = screenSessions
		m.status = fmt.Sprintf("Loaded %d of %d sessions for agent %s", len(m.sessions), len(m.sessionFiles), agent.name)
	case "r":
		agents, err := loadAgents(m.paths.agentsDir)
		if err != nil {
			m.status = "Error: " + err.Error()
			return m, nil
		}
		m.agents = agents
		m.agentCursor = clamp(m.agentCursor, 0, len(m.agents)-1)
		m.status = fmt.Sprintf("Reloaded %d agents", len(agents))
	}
	return m, nil
}

func (m model) handleSessionsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		m.sessionCursor = clamp(m.sessionCursor-1, 0, len(m.sessions)-1)
	case "down", "j":
		previousLoaded := len(m.sessions)
		m.sessionCursor = clamp(m.sessionCursor+1, 0, len(m.sessions)-1)
		loaded := m.maybeLoadMoreSessions()
		if loaded > 0 && m.sessionCursor == previousLoaded-1 {
			m.sessionCursor = clamp(m.sessionCursor+1, 0, len(m.sessions)-1)
		}
	case "enter":
		session, ok := m.currentSession()
		if !ok {
			m.status = "No session selected"
			return m, nil
		}
		messages, err := parseSessionMessages(session.path)
		if err != nil {
			m.status = "Error: " + err.Error()
			return m, nil
		}
		m.messages = messages
		m.screen = screenConversation
		m.refreshConversationViewport()
		m.status = fmt.Sprintf("Loaded %d messages from %s", len(messages), session.filename)
	case "b", "backspace":
		m.screen = screenAgents
		m.sessionFiles = nil
		m.sessionFileCursor = 0
		m.sessions = nil
		m.sessionCursor = 0
		m.status = "Back to agents"
	case "r":
		agent, ok := m.currentAgent()
		if !ok {
			m.status = "No agent selected"
			return m, nil
		}
		if err := m.loadInitialSessions(agent); err != nil {
			m.status = "Error: " + err.Error()
			return m, nil
		}
		m.sessionCursor = clamp(m.sessionCursor, 0, len(m.sessions)-1)
		m.status = fmt.Sprintf("Reloaded %d of %d sessions", len(m.sessions), len(m.sessionFiles))
	}
	return m, nil
}

func (m model) handleConversationKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		m.convViewport.LineUp(1)
	case "down", "j":
		m.convViewport.LineDown(1)
	case "pgup":
		m.convViewport.HalfViewUp()
	case "pgdown":
		m.convViewport.HalfViewDown()
	case "g":
		m.convViewport.GotoTop()
	case "G":
		m.convViewport.GotoBottom()
	case "b", "backspace":
		m.screen = screenSessions
		m.status = "Back to sessions"
	case "r":
		session, ok := m.currentSession()
		if !ok {
			m.status = "No session selected"
			return m, nil
		}
		messages, err := parseSessionMessages(session.path)
		if err != nil {
			m.status = "Error: " + err.Error()
			return m, nil
		}
		m.messages = messages
		m.refreshConversationViewport()
		m.status = fmt.Sprintf("Reloaded %d messages", len(messages))
	case "l":
		session, ok := m.currentSession()
		if !ok {
			m.status = "No session selected"
			return m, nil
		}
		summary, err := loadSummaryGraph(m.paths.lcmDBPath, session.id)
		if err != nil {
			m.status = "Error: " + err.Error()
			return m, nil
		}
		m.summary = summary
		m.summaryRows = buildSummaryRows(summary)
		m.summaryCursor = 0
		m.summarySources = make(map[string][]summarySource)
		m.summarySourceErr = make(map[string]string)
		m.loadCurrentSummarySources()
		m.screen = screenSummaries
		m.status = fmt.Sprintf("Loaded %d summaries for conversation %d", len(summary.nodes), summary.conversationID)
	case "f":
		session, ok := m.currentSession()
		if !ok {
			m.status = "No session selected"
			return m, nil
		}
		files, err := loadLargeFiles(m.paths.lcmDBPath, session.id)
		if err != nil {
			m.status = "Error: " + err.Error()
			return m, nil
		}
		m.largeFiles = files
		m.fileCursor = 0
		m.screen = screenFiles
		if len(files) == 0 {
			m.status = fmt.Sprintf("No large files for session %s", session.id)
		} else {
			m.status = fmt.Sprintf("Loaded %d large files", len(files))
		}
	case "c":
		session, ok := m.currentSession()
		if !ok {
			m.status = "No session selected"
			return m, nil
		}
		items, err := loadContextItems(m.paths.lcmDBPath, session.id)
		if err != nil {
			m.status = "Error: " + err.Error()
			return m, nil
		}
		m.contextItems = items
		m.contextCursor = 0
		m.screen = screenContext
		if len(items) == 0 {
			m.status = "No context items for this session"
		} else {
			totalTokens := 0
			summaryCount := 0
			messageCount := 0
			for _, it := range items {
				totalTokens += it.tokenCount
				if it.itemType == "summary" {
					summaryCount++
				} else {
					messageCount++
				}
			}
			m.status = fmt.Sprintf("Context: %d summaries + %d messages = %d items, %dk tokens",
				summaryCount, messageCount, len(items), totalTokens/1000)
		}
	}
	return m, nil
}

func (m model) handleSummariesKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		m.summaryCursor = clamp(m.summaryCursor-1, 0, len(m.summaryRows)-1)
		m.summaryDetailScroll = 0
		m.loadCurrentSummarySources()
	case "down", "j":
		m.summaryCursor = clamp(m.summaryCursor+1, 0, len(m.summaryRows)-1)
		m.summaryDetailScroll = 0
		m.loadCurrentSummarySources()
	case "g":
		m.summaryCursor = 0
		m.summaryDetailScroll = 0
		m.loadCurrentSummarySources()
	case "G":
		m.summaryCursor = max(0, len(m.summaryRows)-1)
		m.summaryDetailScroll = 0
		m.loadCurrentSummarySources()
	case "J":
		m.summaryDetailScroll++
	case "K":
		m.summaryDetailScroll = max(0, m.summaryDetailScroll-1)
	case "enter", "right", "l", " ":
		m.expandOrToggleSelectedSummary()
	case "left", "h":
		m.collapseSelectedSummary()
	case "r":
		session, ok := m.currentSession()
		if !ok {
			m.status = "No session selected"
			return m, nil
		}
		summary, err := loadSummaryGraph(m.paths.lcmDBPath, session.id)
		if err != nil {
			m.status = "Error: " + err.Error()
			return m, nil
		}
		m.summary = summary
		m.summaryRows = buildSummaryRows(summary)
		m.summaryCursor = clamp(m.summaryCursor, 0, len(m.summaryRows)-1)
		m.summarySources = make(map[string][]summarySource)
		m.summarySourceErr = make(map[string]string)
		m.loadCurrentSummarySources()
		m.status = fmt.Sprintf("Reloaded %d summaries", len(summary.nodes))
	case "b", "backspace":
		m.screen = screenConversation
		m.status = "Back to conversation"
	}
	return m, nil
}

func (m model) handleFilesKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		m.fileCursor = clamp(m.fileCursor-1, 0, len(m.largeFiles)-1)
	case "down", "j":
		m.fileCursor = clamp(m.fileCursor+1, 0, len(m.largeFiles)-1)
	case "g":
		m.fileCursor = 0
	case "G":
		m.fileCursor = max(0, len(m.largeFiles)-1)
	case "r":
		session, ok := m.currentSession()
		if !ok {
			m.status = "No session selected"
			return m, nil
		}
		files, err := loadLargeFiles(m.paths.lcmDBPath, session.id)
		if err != nil {
			m.status = "Error: " + err.Error()
			return m, nil
		}
		m.largeFiles = files
		m.fileCursor = clamp(m.fileCursor, 0, len(m.largeFiles)-1)
		m.status = fmt.Sprintf("Reloaded %d large files", len(files))
	case "f":
		session, ok := m.currentSession()
		if !ok {
			m.status = "No session selected"
			return m, nil
		}
		files, err := loadLargeFiles(m.paths.lcmDBPath, session.id)
		if err != nil {
			m.status = "Error: " + err.Error()
			return m, nil
		}
		m.largeFiles = files
		m.fileCursor = 0
		m.screen = screenFiles
		if len(files) == 0 {
			m.status = "No large files for this session"
		} else {
			m.status = fmt.Sprintf("Loaded %d large files", len(files))
		}
	case "b", "backspace":
		m.screen = screenConversation
		m.status = "Back to conversation"
	}
	return m, nil
}

func (m model) handleContextKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		m.contextCursor = clamp(m.contextCursor-1, 0, len(m.contextItems)-1)
		m.contextDetailScroll = 0
	case "down", "j":
		m.contextCursor = clamp(m.contextCursor+1, 0, len(m.contextItems)-1)
		m.contextDetailScroll = 0
	case "g":
		m.contextCursor = 0
		m.contextDetailScroll = 0
	case "G":
		m.contextCursor = max(0, len(m.contextItems)-1)
		m.contextDetailScroll = 0
	case "J":
		m.contextDetailScroll++
	case "K":
		m.contextDetailScroll = max(0, m.contextDetailScroll-1)
	case "r":
		session, ok := m.currentSession()
		if !ok {
			m.status = "No session selected"
			return m, nil
		}
		items, err := loadContextItems(m.paths.lcmDBPath, session.id)
		if err != nil {
			m.status = "Error: " + err.Error()
			return m, nil
		}
		m.contextItems = items
		m.contextCursor = clamp(m.contextCursor, 0, len(m.contextItems)-1)
		m.status = fmt.Sprintf("Reloaded %d context items", len(items))
	case "b", "backspace":
		m.screen = screenConversation
		m.status = "Back to conversation"
	}
	return m, nil
}

func (m *model) expandOrToggleSelectedSummary() {
	id, ok := m.currentSummaryID()
	if !ok {
		m.status = "No summary selected"
		return
	}
	node := m.summary.nodes[id]
	if node == nil {
		m.status = "Missing summary node"
		return
	}
	if len(node.children) == 0 {
		m.status = "Summary has no children"
		return
	}
	node.expanded = !node.expanded
	m.summaryRows = buildSummaryRows(m.summary)
	m.summaryCursor = clamp(m.summaryCursor, 0, len(m.summaryRows)-1)
	m.loadCurrentSummarySources()
}

func (m *model) collapseSelectedSummary() {
	id, ok := m.currentSummaryID()
	if !ok {
		m.status = "No summary selected"
		return
	}
	node := m.summary.nodes[id]
	if node == nil {
		m.status = "Missing summary node"
		return
	}
	if node.expanded {
		node.expanded = false
		m.summaryRows = buildSummaryRows(m.summary)
		m.summaryCursor = clamp(m.summaryCursor, 0, len(m.summaryRows)-1)
		m.loadCurrentSummarySources()
		return
	}
	m.status = "Summary already collapsed"
}

func (m *model) loadCurrentSummarySources() {
	id, ok := m.currentSummaryID()
	if !ok {
		return
	}
	if _, exists := m.summarySources[id]; exists {
		return
	}
	if _, exists := m.summarySourceErr[id]; exists {
		return
	}

	sources, err := loadSummarySources(m.paths.lcmDBPath, id)
	if err != nil {
		m.summarySourceErr[id] = err.Error()
		return
	}
	m.summarySources[id] = sources
}

func buildSummaryRows(graph summaryGraph) []summaryRow {
	rows := make([]summaryRow, 0, len(graph.nodes))
	var walk func(summaryID string, depth int, path map[string]bool)

	walk = func(summaryID string, depth int, path map[string]bool) {
		if path[summaryID] {
			return
		}
		node := graph.nodes[summaryID]
		if node == nil {
			return
		}
		rows = append(rows, summaryRow{summaryID: summaryID, depth: depth})
		if !node.expanded {
			return
		}

		path[summaryID] = true
		for _, childID := range node.children {
			walk(childID, depth+1, path)
		}
		delete(path, summaryID)
	}

	for _, rootID := range graph.roots {
		walk(rootID, 0, map[string]bool{})
	}
	return rows
}

func (m model) View() string {
	if m.width <= 0 || m.height <= 0 {
		return "Initializing openclaw-tui..."
	}

	header := m.renderHeader()
	body := m.renderBody()
	footer := helpStyle.Render(m.renderStatus())
	return header + "\n" + body + "\n" + footer
}

func (m model) renderHeader() string {
	title := "openclaw-tui"
	switch m.screen {
	case screenAgents:
		title += " | Agents"
	case screenSessions:
		agentName := ""
		if agent, ok := m.currentAgent(); ok {
			agentName = " | " + agent.name
		}
		title += " | Sessions" + agentName
	case screenConversation:
		title += " | Conversation"
	case screenSummaries:
		title += " | LCM Summary DAG"
	case screenFiles:
		title += " | LCM Large Files"
	case screenContext:
		title += " | LCM Active Context"
	}

	help := m.renderHelp()
	return titleStyle.Render(title) + "\n" + helpStyle.Render(help)
}

func (m model) renderHelp() string {
	switch m.screen {
	case screenAgents:
		return "up/down: move | enter: open agent sessions | r: reload | q: quit"
	case screenSessions:
		return "up/down: move | enter: open conversation | b: back | r: reload | q: quit"
	case screenConversation:
		return "j/k/up/down: scroll | pgup/pgdown | g/G: top/bottom | r: reload | l: LCM summaries | c: context | f: LCM files | b: back | q: quit"
	case screenSummaries:
		return "up/down: move | enter/right/l: expand-toggle | left/h: collapse | Shift+J/K: scroll detail | g/G: top/bottom | f: LCM files | r: reload | b: back | q: quit"
	case screenFiles:
		return "up/down: move | g/G: top/bottom | r: reload | b: back | q: quit"
	case screenContext:
		return "up/down: move | g/G: top/bottom | r: reload | b: back | q: quit"
	default:
		return "q: quit"
	}
}

func (m model) renderBody() string {
	switch m.screen {
	case screenAgents:
		return m.renderAgents()
	case screenSessions:
		return m.renderSessions()
	case screenConversation:
		return m.renderConversation()
	case screenSummaries:
		return m.renderSummaries()
	case screenFiles:
		return m.renderFiles()
	case screenContext:
		return m.renderContext()
	default:
		return "Unknown screen"
	}
}

func (m model) renderStatus() string {
	if m.screen != screenSessions {
		return m.status
	}
	total := len(m.sessionFiles)
	showing := len(m.sessions)
	if m.status == "" {
		return fmt.Sprintf("showing %d of %d", showing, total)
	}
	return fmt.Sprintf("showing %d of %d | %s", showing, total, m.status)
}

func (m model) renderAgents() string {
	if len(m.agents) == 0 {
		return "No agents found under ~/.openclaw/agents"
	}
	visible := max(1, m.height-4)
	offset := listOffset(m.agentCursor, len(m.agents), visible)

	lines := make([]string, 0, visible)
	for idx := offset; idx < min(len(m.agents), offset+visible); idx++ {
		line := fmt.Sprintf("  %s", m.agents[idx].name)
		if idx == m.agentCursor {
			line = selectedStyle.Render("> " + m.agents[idx].name)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func (m model) renderSessions() string {
	if len(m.sessions) == 0 {
		return "No session JSONL files found for this agent"
	}
	visible := max(1, m.height-4)
	offset := listOffset(m.sessionCursor, len(m.sessions), visible)

	lines := make([]string, 0, visible)
	for idx := offset; idx < min(len(m.sessions), offset+visible); idx++ {
		session := m.sessions[idx]
		messageCount := formatMessageCount(session.messageCount)
		extras := ""
		if session.summaryCount > 0 {
			extras += fmt.Sprintf("  sums:%d", session.summaryCount)
		}
		if session.fileCount > 0 {
			extras += fmt.Sprintf("  files:%d", session.fileCount)
		}
		line := fmt.Sprintf("  %s  %s  msgs:%s%s", session.filename, formatTimeForList(session.updatedAt), messageCount, extras)
		if idx == m.sessionCursor {
			line = selectedStyle.Render(fmt.Sprintf("> %s  %s  msgs:%s%s", session.filename, formatTimeForList(session.updatedAt), messageCount, extras))
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func (m model) renderConversation() string {
	if len(m.messages) == 0 {
		return "No messages found in this session"
	}
	if m.convViewport.Width <= 0 || m.convViewport.Height <= 0 {
		return "Resizing conversation viewport..."
	}
	return m.convViewport.View()
}

func (m model) renderSummaries() string {
	if len(m.summary.nodes) == 0 {
		return "No LCM summaries found for this session"
	}
	if len(m.summaryRows) == 0 {
		return "Summary graph is empty"
	}

	available := max(4, m.height-4)
	detailHeight := max(7, available/3)
	listHeight := max(3, available-detailHeight-1)

	listOffsetValue := listOffset(m.summaryCursor, len(m.summaryRows), listHeight)
	listLines := make([]string, 0, listHeight)
	for idx := listOffsetValue; idx < min(len(m.summaryRows), listOffsetValue+listHeight); idx++ {
		row := m.summaryRows[idx]
		node := m.summary.nodes[row.summaryID]
		if node == nil {
			continue
		}
		marker := "-"
		if len(node.children) > 0 {
			if node.expanded {
				marker = "v"
			} else {
				marker = ">"
			}
		}
		preview := oneLine(node.content)
		preview = truncateString(preview, max(8, m.width-50))
		line := fmt.Sprintf("%s%s %s [%s, %dt] %s", strings.Repeat("  ", row.depth), marker, node.id, node.kind, node.tokenCount, preview)
		if idx == m.summaryCursor {
			line = selectedStyle.Render(line)
		}
		listLines = append(listLines, line)
	}

	detailLines := m.renderSummaryDetail(detailHeight)
	return strings.Join(listLines, "\n") + "\n" + helpStyle.Render(strings.Repeat("-", max(20, m.width-1))) + "\n" + strings.Join(detailLines, "\n")
}

func (m *model) renderSummaryDetail(detailHeight int) []string {
	id, ok := m.currentSummaryID()
	if !ok {
		return padLines([]string{"No summary selected"}, detailHeight)
	}
	node := m.summary.nodes[id]
	if node == nil {
		return padLines([]string{"Missing summary node"}, detailHeight)
	}

	// Build ALL lines (no height limit)
	var allLines []string
	allLines = append(allLines, fmt.Sprintf("Summary: %s", id))
	allLines = append(allLines, fmt.Sprintf("Created: %s  Tokens: %d", node.createdAt, node.tokenCount))
	allLines = append(allLines, "Content:")
	wrappedContent := wrapText(node.content, max(20, m.width-4))
	for _, line := range strings.Split(wrappedContent, "\n") {
		allLines = append(allLines, "  "+line)
	}

	allLines = append(allLines, "Sources:")
	if errMsg, exists := m.summarySourceErr[id]; exists {
		allLines = append(allLines, "  error: "+errMsg)
	} else {
		sources := m.summarySources[id]
		if len(sources) == 0 {
			allLines = append(allLines, "  (no source messages)")
		} else {
			for _, src := range sources {
				content := oneLine(src.content)
				content = truncateString(content, max(8, m.width-24))
				line := fmt.Sprintf("  #%d %s %s", src.id, strings.ToUpper(src.role), content)
				allLines = append(allLines, roleStyle(src.role).Render(line))
			}
		}
	}

	// Clamp scroll offset
	maxScroll := max(0, len(allLines)-detailHeight)
	m.summaryDetailScroll = clamp(m.summaryDetailScroll, 0, maxScroll)

	// Slice visible window
	start := m.summaryDetailScroll
	end := min(len(allLines), start+detailHeight)
	visible := allLines[start:end]

	// Add scroll indicator
	if maxScroll > 0 {
		indicator := fmt.Sprintf(" [%d/%d lines, Shift+J/K to scroll]", m.summaryDetailScroll+detailHeight, len(allLines))
		if len(visible) > 0 {
			visible[0] = visible[0] + helpStyle.Render(indicator)
		}
	}

	return padLines(visible, detailHeight)
}

var (
	fileIDStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("183"))
	fileMimeStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
)

func (m model) renderFiles() string {
	if len(m.largeFiles) == 0 {
		return "No large files found for this session"
	}

	available := max(4, m.height-4)
	detailHeight := max(7, available/2)
	listHeight := max(3, available-detailHeight-1)

	listOffsetValue := listOffset(m.fileCursor, len(m.largeFiles), listHeight)
	listLines := make([]string, 0, listHeight)
	for idx := listOffsetValue; idx < min(len(m.largeFiles), listOffsetValue+listHeight); idx++ {
		f := m.largeFiles[idx]
		sizeStr := formatByteSizeCompact(f.byteSize)
		line := fmt.Sprintf("  %s  %s  %s  %s  %s",
			fileIDStyle.Render(f.fileID),
			f.displayName(),
			fileMimeStyle.Render(f.mimeType),
			sizeStr,
			formatTimestamp(f.createdAt))
		if idx == m.fileCursor {
			line = selectedStyle.Render(fmt.Sprintf("> %s  %s  %s  %s  %s",
				f.fileID,
				f.displayName(),
				f.mimeType,
				sizeStr,
				formatTimestamp(f.createdAt)))
		}
		listLines = append(listLines, line)
	}

	detailLines := m.renderFileDetail(detailHeight)
	return strings.Join(listLines, "\n") + "\n" + helpStyle.Render(strings.Repeat("-", max(20, m.width-1))) + "\n" + strings.Join(detailLines, "\n")
}

func (m model) renderFileDetail(detailHeight int) []string {
	lines := make([]string, 0, detailHeight)
	if m.fileCursor < 0 || m.fileCursor >= len(m.largeFiles) {
		return append(lines, "No file selected")
	}
	f := m.largeFiles[m.fileCursor]

	lines = append(lines, fmt.Sprintf("File: %s", f.fileID))
	lines = append(lines, fmt.Sprintf("Name: %s  MIME: %s  Size: %s  Created: %s",
		f.displayName(), f.mimeType, formatByteSizeCompact(f.byteSize), formatTimestamp(f.createdAt)))
	if f.storageURI != "" {
		lines = append(lines, fmt.Sprintf("Storage: %s", f.storageURI))
	}
	lines = append(lines, "")
	lines = append(lines, "Exploration Summary:")

	summary := strings.TrimSpace(f.explorationSummary)
	if summary == "" {
		summary = "(no exploration summary)"
	}
	wrappedSummary := wrapText(summary, max(20, m.width-4))
	for _, line := range strings.Split(wrappedSummary, "\n") {
		if len(lines) >= detailHeight {
			break
		}
		lines = append(lines, "  "+line)
	}
	return padLines(lines, detailHeight)
}

func (m model) renderContext() string {
	if len(m.contextItems) == 0 {
		return "No context items found for this session"
	}

	available := max(4, m.height-4)
	detailHeight := max(7, available/3)
	listHeight := max(3, available-detailHeight-1)

	listOffsetValue := listOffset(m.contextCursor, len(m.contextItems), listHeight)
	listLines := make([]string, 0, listHeight)
	for idx := listOffsetValue; idx < min(len(m.contextItems), listOffsetValue+listHeight); idx++ {
		item := m.contextItems[idx]
		line := m.formatContextItemLine(item)
		if idx == m.contextCursor {
			line = selectedStyle.Render(line)
		}
		listLines = append(listLines, line)
	}

	detailLines := m.renderContextDetail(detailHeight)
	return strings.Join(listLines, "\n") + "\n" + helpStyle.Render(strings.Repeat("-", max(20, m.width-1))) + "\n" + strings.Join(detailLines, "\n")
}

func (m model) formatContextItemLine(item contextItemEntry) string {
	maxPreview := max(8, m.width-60)
	preview := truncateString(item.preview, maxPreview)

	if item.itemType == "summary" {
		return fmt.Sprintf("  %3d  %-10s [%s, %dt] %s",
			item.ordinal, item.kind, item.summaryID[:min(16, len(item.summaryID))], item.tokenCount, preview)
	}
	// message
	roleStyle := roleUserStyle
	switch item.kind {
	case "assistant":
		roleStyle = roleAssistantStyle
	case "system":
		roleStyle = roleSystemStyle
	case "tool":
		roleStyle = roleToolStyle
	}
	return fmt.Sprintf("  %3d  %-10s [msg %d, %dt] %s",
		item.ordinal, roleStyle.Render(item.kind), item.messageID, item.tokenCount, preview)
}

func (m *model) renderContextDetail(detailHeight int) []string {
	if m.contextCursor < 0 || m.contextCursor >= len(m.contextItems) {
		return padLines([]string{"No item selected"}, detailHeight)
	}
	item := m.contextItems[m.contextCursor]

	var allLines []string
	if item.itemType == "summary" {
		allLines = append(allLines, fmt.Sprintf("Summary: %s [%s]", item.summaryID, item.kind))
		allLines = append(allLines, fmt.Sprintf("Tokens: %d  Created: %s", item.tokenCount, formatTimestamp(item.createdAt)))
	} else {
		allLines = append(allLines, fmt.Sprintf("Message: #%d [%s]", item.messageID, item.kind))
		allLines = append(allLines, fmt.Sprintf("Tokens: %d  Created: %s", item.tokenCount, formatTimestamp(item.createdAt)))
	}
	allLines = append(allLines, "")
	content := strings.TrimSpace(item.content)
	if content == "" {
		content = "(empty)"
	}
	wrapped := wrapText(content, max(20, m.width-4))
	for _, line := range strings.Split(wrapped, "\n") {
		allLines = append(allLines, "  "+line)
	}

	// Clamp scroll offset
	maxScroll := max(0, len(allLines)-detailHeight)
	m.contextDetailScroll = clamp(m.contextDetailScroll, 0, maxScroll)

	// Slice visible window
	start := m.contextDetailScroll
	end := min(len(allLines), start+detailHeight)
	visible := allLines[start:end]

	// Add scroll indicator
	if maxScroll > 0 {
		indicator := fmt.Sprintf(" [%d/%d lines, Shift+J/K to scroll]", m.contextDetailScroll+detailHeight, len(allLines))
		if len(visible) > 0 {
			visible[0] = visible[0] + helpStyle.Render(indicator)
		}
	}

	return padLines(visible, detailHeight)
}

func (m *model) resizeViewport() {
	width := max(20, m.width-2)
	height := max(3, m.height-4)
	if m.convViewport.Width == 0 {
		m.convViewport = viewport.New(width, height)
		return
	}
	m.convViewport.Width = width
	m.convViewport.Height = height
}

func (m *model) refreshConversationViewport() {
	if m.convViewport.Width <= 0 || m.convViewport.Height <= 0 {
		return
	}
	if len(m.messages) == 0 {
		m.convViewport.SetContent("No messages loaded")
		m.convViewport.GotoTop()
		return
	}
	content := renderConversationText(m.messages, m.convViewport.Width)
	m.convViewport.SetContent(content)
	m.convViewport.GotoBottom()
}

func renderConversationText(messages []sessionMessage, width int) string {
	maxWidth := max(20, width-2)
	chunks := make([]string, 0, len(messages))
	for _, msg := range messages {
		timestamp := formatTimestamp(msg.timestamp)
		header := strings.TrimSpace(fmt.Sprintf("%s  %s", timestamp, strings.ToUpper(msg.role)))
		if header == "" {
			header = strings.ToUpper(msg.role)
		}

		body := msg.text
		if strings.TrimSpace(body) == "" {
			body = "(no text content)"
		}

		wrapped := wrapText(body, maxWidth)
		styledHeader := roleStyle(msg.role).Bold(true).Render(header)
		styledBody := roleStyle(msg.role).Render(indentLines(wrapped, "  "))
		chunks = append(chunks, styledHeader+"\n"+styledBody)
	}
	return strings.Join(chunks, "\n\n")
}

func wrapText(text string, width int) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return ""
	}
	wrapped := wordwrap.String(trimmed, width)
	return strings.ReplaceAll(wrapped, "\r", "")
}

func indentLines(text, prefix string) string {
	lines := strings.Split(text, "\n")
	for idx := range lines {
		lines[idx] = prefix + lines[idx]
	}
	return strings.Join(lines, "\n")
}

func roleStyle(role string) lipgloss.Style {
	switch strings.ToLower(role) {
	case "user":
		return roleUserStyle
	case "assistant":
		return roleAssistantStyle
	case "system":
		return roleSystemStyle
	case "tool", "toolresult":
		return roleToolStyle
	default:
		return roleToolStyle
	}
}

func formatMessageCount(count int) string {
	if count < 0 {
		return "?"
	}
	return fmt.Sprintf("%d", count)
}

func (m model) currentAgent() (agentEntry, bool) {
	if len(m.agents) == 0 || m.agentCursor < 0 || m.agentCursor >= len(m.agents) {
		return agentEntry{}, false
	}
	return m.agents[m.agentCursor], true
}

func (m model) currentSession() (sessionEntry, bool) {
	if len(m.sessions) == 0 || m.sessionCursor < 0 || m.sessionCursor >= len(m.sessions) {
		return sessionEntry{}, false
	}
	return m.sessions[m.sessionCursor], true
}

func (m model) currentSummaryID() (string, bool) {
	if len(m.summaryRows) == 0 || m.summaryCursor < 0 || m.summaryCursor >= len(m.summaryRows) {
		return "", false
	}
	return m.summaryRows[m.summaryCursor].summaryID, true
}

func listOffset(cursor, total, visible int) int {
	if total <= visible {
		return 0
	}
	offset := cursor - visible/2
	maxOffset := total - visible
	return clamp(offset, 0, maxOffset)
}

func oneLine(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return ""
	}
	fields := strings.Fields(trimmed)
	return strings.Join(fields, " ")
}

func truncateString(text string, width int) string {
	if width <= 0 {
		return ""
	}
	if len(text) <= width {
		return text
	}
	if width <= 1 {
		return text[:width]
	}
	if width <= 3 {
		return text[:width]
	}
	return text[:width-3] + "..."
}

func padLines(lines []string, minHeight int) []string {
	for len(lines) < minHeight {
		lines = append(lines, "")
	}
	return lines
}

func clamp(value, low, high int) int {
	if high < low {
		return low
	}
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (m *model) loadInitialSessions(agent agentEntry) error {
	files, err := discoverSessionFiles(agent)
	if err != nil {
		return err
	}
	m.sessionFiles = files
	m.sessionFileCursor = 0
	m.sessions = nil
	loaded, err := m.appendSessionBatch(sessionInitialLoadSize)
	if err != nil {
		return err
	}
	m.sessionCursor = clamp(m.sessionCursor, 0, max(0, loaded-1))
	return nil
}

func (m *model) appendSessionBatch(limit int) (int, error) {
	batch, nextCursor, err := loadSessionBatch(m.sessionFiles, m.sessionFileCursor, limit, m.paths.lcmDBPath)
	if err != nil {
		return 0, err
	}
	m.sessionFileCursor = nextCursor
	m.sessions = append(m.sessions, batch...)
	return len(batch), nil
}

func (m *model) maybeLoadMoreSessions() int {
	if len(m.sessions)-m.sessionCursor > 3 {
		return 0
	}
	if m.sessionFileCursor >= len(m.sessionFiles) {
		return 0
	}
	loaded, err := m.appendSessionBatch(sessionBatchLoadSize)
	if err != nil {
		m.status = "Error: " + err.Error()
		return 0
	}
	if loaded > 0 {
		m.status = fmt.Sprintf("Loaded %d of %d sessions", len(m.sessions), len(m.sessionFiles))
	}
	return loaded
}
