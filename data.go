package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// appDataPaths stores resolved locations for session files and the LCM DB.
type appDataPaths struct {
	agentsDir string
	lcmDBPath string
}

// agentEntry describes one agent directory under ~/.openclaw/agents.
type agentEntry struct {
	name string
	path string
}

// sessionEntry describes one JSONL session file.
type sessionEntry struct {
	id            string
	filename      string
	path          string
	updatedAt     time.Time
	messageCount  int
	summaryCount  int
	fileCount     int
}

// sessionFileEntry stores lightweight metadata used for incremental loading.
type sessionFileEntry struct {
	filename  string
	path      string
	updatedAt time.Time
}

// sessionMessage is a normalized chat message used by the conversation viewer.
type sessionMessage struct {
	id        string
	parentID  string
	timestamp string
	role      string
	text      string
}

// summaryNode holds one summary record and its graph children.
type summaryNode struct {
	id         string
	kind       string
	content    string
	createdAt  string
	tokenCount int
	children   []string
	expanded   bool
}

// largeFileEntry describes one large file intercepted by LCM.
type largeFileEntry struct {
	fileID             string
	conversationID     int64
	fileName           string
	mimeType           string
	byteSize           int64
	storageURI         string
	explorationSummary string
	createdAt          string
}

func (f largeFileEntry) displayName() string {
	if f.fileName != "" {
		return f.fileName
	}
	return "(unnamed)"
}

// summarySource is a source message attached to a summary.
type summarySource struct {
	id        int64
	role      string
	content   string
	timestamp string
}

// summaryGraph is the in-memory DAG used by the summary drill-down view.
type summaryGraph struct {
	conversationID int64
	roots          []string
	nodes          map[string]*summaryNode
}

// summaryRow is one visible row in the flattened summary tree.
type summaryRow struct {
	summaryID string
	depth     int
}

// contentBlock supports the JSONL message content block format.
type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Reasoning string          `json:"reasoning"`
	Content   json.RawMessage `json:"content"`
}

// sessionLine is the top-level JSON object in each JSONL row.
type sessionLine struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	ParentID  string          `json:"parentId"`
	Timestamp string          `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
}

// lineMessage is the nested message payload within a session line.
type lineMessage struct {
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Timestamp any             `json:"timestamp"`
}

func resolveDataPaths() (appDataPaths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return appDataPaths{}, fmt.Errorf("resolve home dir: %w", err)
	}
	base := filepath.Join(home, ".openclaw")
	return appDataPaths{
		agentsDir: filepath.Join(base, "agents"),
		lcmDBPath: filepath.Join(base, "lcm.db"),
	}, nil
}

func loadAgents(agentsDir string) ([]agentEntry, error) {
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		return nil, fmt.Errorf("read agents dir %q: %w", agentsDir, err)
	}

	agents := make([]agentEntry, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		agents = append(agents, agentEntry{
			name: entry.Name(),
			path: filepath.Join(agentsDir, entry.Name()),
		})
	}

	sort.Slice(agents, func(i, j int) bool {
		return strings.ToLower(agents[i].name) < strings.ToLower(agents[j].name)
	})
	return agents, nil
}

func discoverSessionFiles(agent agentEntry) ([]sessionFileEntry, error) {
	sessionsDir := filepath.Join(agent.path, "sessions")
	paths, err := filepath.Glob(filepath.Join(sessionsDir, "*.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("glob sessions for agent %q: %w", agent.name, err)
	}

	sessions := make([]sessionFileEntry, 0, len(paths))
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		filename := filepath.Base(path)
		sessions = append(sessions, sessionFileEntry{
			filename:  filename,
			path:      path,
			updatedAt: info.ModTime(),
		})
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].updatedAt.After(sessions[j].updatedAt)
	})
	return sessions, nil
}

func loadSessionBatch(files []sessionFileEntry, offset, limit int, lcmDBPath string) ([]sessionEntry, int, error) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		return nil, offset, nil
	}
	if offset >= len(files) {
		return nil, len(files), nil
	}

	end := offset + limit
	if end > len(files) {
		end = len(files)
	}

	sessions := make([]sessionEntry, 0, end-offset)
	sessionIDs := make([]string, 0, end-offset)
	for _, file := range files[offset:end] {
		messageCount, err := countMessages(file.path)
		if err != nil {
			messageCount = -1
		}
		id := strings.TrimSuffix(file.filename, filepath.Ext(file.filename))
		sessionIDs = append(sessionIDs, id)
		sessions = append(sessions, sessionEntry{
			id:           id,
			filename:     file.filename,
			path:         file.path,
			updatedAt:    file.updatedAt,
			messageCount: messageCount,
		})
	}

	summaryCounts := loadSummaryCounts(lcmDBPath, sessionIDs)
	fileCounts := loadFileCounts(lcmDBPath, sessionIDs)
	for i := range sessions {
		sessions[i].summaryCount = summaryCounts[sessions[i].id]
		sessions[i].fileCount = fileCounts[sessions[i].id]
	}

	return sessions, end, nil
}

func loadSessions(agent agentEntry, lcmDBPath string) ([]sessionEntry, error) {
	files, err := discoverSessionFiles(agent)
	if err != nil {
		return nil, err
	}
	sessions, _, err := loadSessionBatch(files, 0, len(files), lcmDBPath)
	if err != nil {
		return nil, err
	}
	return sessions, nil
}

func countMessages(path string) (int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open session %q: %w", path, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 16*1024*1024)

	count := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var item sessionLine
		if err := json.Unmarshal(line, &item); err != nil {
			continue
		}
		if item.Type == "message" {
			count++
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("scan session %q: %w", path, err)
	}
	return count, nil
}

func parseSessionMessages(path string) ([]sessionMessage, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open session %q: %w", path, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 16*1024*1024)

	messages := make([]sessionMessage, 0, 256)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var item sessionLine
		if err := json.Unmarshal(line, &item); err != nil || item.Type != "message" {
			continue
		}

		var msg lineMessage
		if err := json.Unmarshal(item.Message, &msg); err != nil {
			continue
		}

		role := msg.Role
		if role == "" {
			role = "unknown"
		}
		messages = append(messages, sessionMessage{
			id:        item.ID,
			parentID:  item.ParentID,
			timestamp: pickTimestamp(item.Timestamp, msg.Timestamp),
			role:      role,
			text:      normalizeMessageContent(msg.Content),
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan session %q: %w", path, err)
	}
	return messages, nil
}

func pickTimestamp(primary string, fallback any) string {
	if strings.TrimSpace(primary) != "" {
		return primary
	}
	switch v := fallback.(type) {
	case string:
		return v
	case float64:
		// JSON numbers decode as float64; the source uses epoch milliseconds.
		ms := int64(v)
		if ms <= 0 {
			return ""
		}
		return time.UnixMilli(ms).UTC().Format(time.RFC3339Nano)
	default:
		return ""
	}
}

const maxDisplayBytes = 100_000 // truncate very long text content for display

// sanitizeForTerminal strips non-printable characters that corrupt terminal output.
// If more than 10% of the content is non-printable, it's treated as binary and replaced
// with a placeholder showing the byte count. Very long text is truncated.
func sanitizeForTerminal(s string) string {
	if len(s) == 0 {
		return s
	}
	nonPrintable := 0
	total := 0
	for _, r := range s {
		total++
		if r != '\n' && r != '\r' && r != '\t' && (r < 32 || r == 127 || (r >= 0x80 && r <= 0x9F)) {
			nonPrintable++
		}
	}
	if total > 0 && nonPrintable*10 > total {
		return fmt.Sprintf("[binary content, %s]", formatByteSizeCompact(int64(len(s))))
	}

	truncated := false
	if len(s) > maxDisplayBytes {
		// Truncate at a rune boundary
		count := 0
		for i := range s {
			if i >= maxDisplayBytes {
				s = s[:i]
				truncated = true
				break
			}
			count++
		}
	}

	// Strip individual non-printable characters
	var result string
	if nonPrintable == 0 {
		result = s
	} else {
		var b strings.Builder
		b.Grow(len(s))
		for _, r := range s {
			if r == '\n' || r == '\r' || r == '\t' || (r >= 32 && r != 127 && !(r >= 0x80 && r <= 0x9F)) {
				b.WriteRune(r)
			}
		}
		result = b.String()
	}
	if truncated {
		result += fmt.Sprintf("\n\n[truncated — full content is %s]", formatByteSizeCompact(int64(len(s))))
	}
	return result
}

func formatByteSizeCompact(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}
	if bytes < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(bytes)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(bytes)/(1024*1024))
}

func normalizeMessageContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return sanitizeForTerminal(strings.TrimSpace(asString))
	}

	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		parts := make([]string, 0, len(blocks))
		for _, block := range blocks {
			part := formatContentBlock(block)
			if part != "" {
				parts = append(parts, part)
			}
		}
		if len(parts) > 0 {
			return sanitizeForTerminal(strings.Join(parts, "\n"))
		}
	}

	var asAny any
	if err := json.Unmarshal(raw, &asAny); err == nil {
		return sanitizeForTerminal(strings.TrimSpace(fmt.Sprintf("%v", asAny)))
	}
	return sanitizeForTerminal(strings.TrimSpace(string(raw)))
}

func formatContentBlock(block contentBlock) string {
	switch block.Type {
	case "text":
		return strings.TrimSpace(block.Text)
	case "thinking", "reasoning":
		if strings.TrimSpace(block.Text) != "" {
			return "[thinking] " + strings.TrimSpace(block.Text)
		}
		if strings.TrimSpace(block.Reasoning) != "" {
			return "[thinking] " + strings.TrimSpace(block.Reasoning)
		}
		return "[thinking]"
	case "toolCall":
		name := strings.TrimSpace(block.Name)
		if name == "" {
			name = "unknown"
		}
		args := strings.TrimSpace(string(block.Arguments))
		if args == "" || args == "null" {
			return fmt.Sprintf("[toolCall] %s", name)
		}
		return fmt.Sprintf("[toolCall] %s %s", name, args)
	case "toolResult":
		if strings.TrimSpace(block.Text) != "" {
			return "[toolResult] " + strings.TrimSpace(block.Text)
		}
		if len(block.Content) > 0 {
			nested := normalizeMessageContent(block.Content)
			if nested != "" {
				return "[toolResult] " + nested
			}
		}
		return "[toolResult]"
	default:
		if strings.TrimSpace(block.Text) != "" {
			return strings.TrimSpace(block.Text)
		}
		if len(block.Content) > 0 {
			nested := normalizeMessageContent(block.Content)
			if nested != "" {
				return nested
			}
		}
		if block.Type != "" {
			return "[" + block.Type + "]"
		}
		return ""
	}
}

func openLCMDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite db %q: %w", path, err)
	}
	return db, nil
}

func loadSummaryGraph(dbPath, sessionID string) (summaryGraph, error) {
	db, err := openLCMDB(dbPath)
	if err != nil {
		return summaryGraph{}, err
	}
	defer db.Close()

	conversationID, err := lookupConversationID(db, sessionID)
	if err != nil {
		return summaryGraph{}, err
	}

	nodes, err := loadSummaryNodes(db, conversationID)
	if err != nil {
		return summaryGraph{}, err
	}
	if len(nodes) == 0 {
		return summaryGraph{conversationID: conversationID, nodes: map[string]*summaryNode{}}, nil
	}

	childSet, err := populateSummaryChildren(db, conversationID, nodes)
	if err != nil {
		return summaryGraph{}, err
	}

	roots := findSummaryRoots(nodes, childSet)
	sortSummaryIDs(roots, nodes)
	for _, node := range nodes {
		sortSummaryIDs(node.children, nodes)
	}

	return summaryGraph{
		conversationID: conversationID,
		roots:          roots,
		nodes:          nodes,
	}, nil
}

func lookupConversationID(db *sql.DB, sessionID string) (int64, error) {
	var conversationID int64
	err := db.QueryRow(`
		SELECT conversation_id
		FROM conversations
		WHERE session_id = ?
		ORDER BY updated_at DESC
		LIMIT 1
	`, sessionID).Scan(&conversationID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("no LCM conversation found for session %q", sessionID)
		}
		return 0, fmt.Errorf("lookup conversation for session %q: %w", sessionID, err)
	}
	return conversationID, nil
}

func loadSummaryNodes(db *sql.DB, conversationID int64) (map[string]*summaryNode, error) {
	rows, err := db.Query(`
		SELECT summary_id, kind, content, created_at, token_count
		FROM summaries
		WHERE conversation_id = ?
	`, conversationID)
	if err != nil {
		return nil, fmt.Errorf("query summaries for conversation %d: %w", conversationID, err)
	}
	defer rows.Close()

	nodes := make(map[string]*summaryNode)
	for rows.Next() {
		var node summaryNode
		if err := rows.Scan(&node.id, &node.kind, &node.content, &node.createdAt, &node.tokenCount); err != nil {
			return nil, fmt.Errorf("scan summary row: %w", err)
		}
		node.content = sanitizeForTerminal(node.content)
		nodes[node.id] = &node
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate summary rows: %w", err)
	}
	return nodes, nil
}

func populateSummaryChildren(db *sql.DB, conversationID int64, nodes map[string]*summaryNode) (map[string]bool, error) {
	rows, err := db.Query(`
		SELECT sp.parent_summary_id, sp.summary_id
		FROM summary_parents sp
		JOIN summaries s ON s.summary_id = sp.summary_id
		WHERE s.conversation_id = ?
	`, conversationID)
	if err != nil {
		return nil, fmt.Errorf("query summary edges for conversation %d: %w", conversationID, err)
	}
	defer rows.Close()

	childSet := make(map[string]bool)
	for rows.Next() {
		var parentID, childID string
		if err := rows.Scan(&parentID, &childID); err != nil {
			return nil, fmt.Errorf("scan summary edge: %w", err)
		}
		parentNode, hasParent := nodes[parentID]
		_, hasChild := nodes[childID]
		if hasParent && hasChild {
			parentNode.children = append(parentNode.children, childID)
			childSet[childID] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate summary edges: %w", err)
	}
	return childSet, nil
}

func findSummaryRoots(nodes map[string]*summaryNode, childSet map[string]bool) []string {
	roots := make([]string, 0, len(nodes))
	for id := range nodes {
		if !childSet[id] {
			roots = append(roots, id)
		}
	}
	if len(roots) == 0 {
		for id := range nodes {
			roots = append(roots, id)
		}
	}
	return roots
}

func sortSummaryIDs(ids []string, nodes map[string]*summaryNode) {
	sort.Slice(ids, func(i, j int) bool {
		left := nodes[ids[i]]
		right := nodes[ids[j]]
		if left == nil || right == nil {
			return ids[i] < ids[j]
		}
		if left.createdAt == right.createdAt {
			return left.id < right.id
		}
		return left.createdAt < right.createdAt
	})
}

func loadSummarySources(dbPath, summaryID string) ([]summarySource, error) {
	db, err := openLCMDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT m.message_id, m.role, m.content, m.created_at
		FROM summary_messages sm
		JOIN messages m ON m.message_id = sm.message_id
		WHERE sm.summary_id = ?
		ORDER BY sm.ordinal ASC
	`, summaryID)
	if err != nil {
		return nil, fmt.Errorf("query summary sources for %q: %w", summaryID, err)
	}
	defer rows.Close()

	sources := make([]summarySource, 0, 8)
	for rows.Next() {
		var src summarySource
		if err := rows.Scan(&src.id, &src.role, &src.content, &src.timestamp); err != nil {
			return nil, fmt.Errorf("scan summary source row: %w", err)
		}
		src.content = sanitizeForTerminal(src.content)
		sources = append(sources, src)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate summary source rows: %w", err)
	}
	return sources, nil
}

func loadSummaryCounts(dbPath string, sessionIDs []string) map[string]int {
	counts := make(map[string]int, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return counts
	}
	db, err := openLCMDB(dbPath)
	if err != nil {
		return counts
	}
	defer db.Close()

	// Build query with placeholders
	placeholders := make([]string, len(sessionIDs))
	args := make([]any, len(sessionIDs))
	for i, id := range sessionIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	query := fmt.Sprintf(`
		SELECT c.session_id, COUNT(s.summary_id)
		FROM conversations c
		JOIN summaries s ON s.conversation_id = c.conversation_id
		WHERE c.session_id IN (%s)
		GROUP BY c.session_id
	`, strings.Join(placeholders, ","))

	rows, err := db.Query(query, args...)
	if err != nil {
		return counts
	}
	defer rows.Close()

	for rows.Next() {
		var sessionID string
		var count int
		if err := rows.Scan(&sessionID, &count); err != nil {
			continue
		}
		counts[sessionID] = count
	}
	return counts
}

func loadLargeFiles(dbPath, sessionID string) ([]largeFileEntry, error) {
	db, err := openLCMDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	conversationID, err := lookupConversationID(db, sessionID)
	if err != nil {
		return nil, err
	}

	rows, err := db.Query(`
		SELECT file_id, conversation_id, file_name, mime_type, byte_size, storage_uri, exploration_summary, created_at
		FROM large_files
		WHERE conversation_id = ?
		ORDER BY created_at ASC
	`, conversationID)
	if err != nil {
		return nil, fmt.Errorf("query large files for conversation %d: %w", conversationID, err)
	}
	defer rows.Close()

	files := make([]largeFileEntry, 0, 8)
	for rows.Next() {
		var f largeFileEntry
		var fileName, mimeType, explorationSummary sql.NullString
		var byteSize sql.NullInt64
		if err := rows.Scan(&f.fileID, &f.conversationID, &fileName, &mimeType, &byteSize, &f.storageURI, &explorationSummary, &f.createdAt); err != nil {
			return nil, fmt.Errorf("scan large file row: %w", err)
		}
		f.fileName = fileName.String
		f.mimeType = mimeType.String
		f.byteSize = byteSize.Int64
		f.explorationSummary = sanitizeForTerminal(explorationSummary.String)
		files = append(files, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate large file rows: %w", err)
	}
	return files, nil
}

func loadFileCounts(dbPath string, sessionIDs []string) map[string]int {
	counts := make(map[string]int, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return counts
	}
	db, err := openLCMDB(dbPath)
	if err != nil {
		return counts
	}
	defer db.Close()

	placeholders := make([]string, len(sessionIDs))
	args := make([]any, len(sessionIDs))
	for i, id := range sessionIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	query := fmt.Sprintf(`
		SELECT c.session_id, COUNT(lf.file_id)
		FROM conversations c
		JOIN large_files lf ON lf.conversation_id = c.conversation_id
		WHERE c.session_id IN (%s)
		GROUP BY c.session_id
	`, strings.Join(placeholders, ","))

	rows, err := db.Query(query, args...)
	if err != nil {
		return counts
	}
	defer rows.Close()

	for rows.Next() {
		var sessionID string
		var count int
		if err := rows.Scan(&sessionID, &count); err != nil {
			continue
		}
		counts[sessionID] = count
	}
	return counts
}

func formatTimeForList(ts time.Time) string {
	return ts.Local().Format("2006-01-02 15:04:05")
}

func formatTimestamp(ts string) string {
	trimmed := strings.TrimSpace(ts)
	if trimmed == "" {
		return ""
	}
	if parsed, err := time.Parse(time.RFC3339Nano, trimmed); err == nil {
		return parsed.Local().Format("2006-01-02 15:04:05")
	}
	return trimmed
}
