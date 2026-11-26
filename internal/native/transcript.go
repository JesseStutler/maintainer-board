package native

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"maintainer-board/internal/ao"
)

// convBuffer stores the ordered messages and tool activities of one session
// and serves paged snapshots in the format expected by the web drawer.
type convBuffer struct {
	mu         sync.Mutex
	seq        int64
	messages   []ao.Message
	msgIndex   map[string]int
	activities []ao.ConversationActivity
	actIndex   map[string]int
}

func newConvBuffer() *convBuffer {
	return &convBuffer{
		msgIndex: map[string]int{},
		actIndex: map[string]int{},
	}
}

func (b *convBuffer) upsertMessage(id, role, text, turnID, clientMsgID, createdAt string, streaming bool) ao.Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	if createdAt == "" {
		createdAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if idx, ok := b.msgIndex[id]; ok {
		m := &b.messages[idx]
		m.Text = text
		m.Streaming = streaming
		m.Revision++
		if turnID != "" {
			m.TurnID = turnID
		}
		if clientMsgID != "" {
			m.ClientMessageID = clientMsgID
		}
		return *m
	}
	b.seq++
	if id == "" {
		id = fmt.Sprintf("msg-%d", b.seq)
	}
	m := ao.Message{
		Kind:            "message",
		ID:              id,
		Sequence:        b.seq,
		Revision:        1,
		Role:            role,
		Text:            text,
		Streaming:       streaming,
		TurnID:          turnID,
		ClientMessageID: clientMsgID,
		CreatedAt:       createdAt,
	}
	b.msgIndex[id] = len(b.messages)
	b.messages = append(b.messages, m)
	return m
}

func (b *convBuffer) appendMessageDelta(id, role, delta, turnID string) ao.Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	if idx, ok := b.msgIndex[id]; ok {
		m := &b.messages[idx]
		m.Text += delta
		m.Streaming = true
		m.Revision++
		if turnID != "" {
			m.TurnID = turnID
		}
		return *m
	}
	b.seq++
	if id == "" {
		id = fmt.Sprintf("msg-%d", b.seq)
	}
	m := ao.Message{
		Kind:      "message",
		ID:        id,
		Sequence:  b.seq,
		Revision:  1,
		Role:      role,
		Text:      delta,
		Streaming: true,
		TurnID:    turnID,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	b.msgIndex[id] = len(b.messages)
	b.messages = append(b.messages, m)
	return m
}

func (b *convBuffer) finishStreaming(turnID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range b.messages {
		if b.messages[i].Streaming && (turnID == "" || b.messages[i].TurnID == turnID) {
			b.messages[i].Streaming = false
			b.messages[i].Revision++
		}
	}
	for i := range b.activities {
		if b.activities[i].Status == "running" && (turnID == "" || b.activities[i].TurnID == turnID) {
			b.activities[i].Status = "completed"
			b.activities[i].Revision++
		}
	}
}

func (b *convBuffer) upsertActivity(id, activityKind, status, summary string, detail any, turnID, createdAt string) ao.ConversationActivity {
	b.mu.Lock()
	defer b.mu.Unlock()
	if createdAt == "" {
		createdAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	var raw json.RawMessage
	if detail != nil {
		if bts, err := json.Marshal(detail); err == nil && string(bts) != "null" {
			raw = bts
		}
	}
	if idx, ok := b.actIndex[id]; ok {
		a := &b.activities[idx]
		if activityKind != "" {
			a.ActivityKind = activityKind
		}
		if status != "" {
			a.Status = status
		}
		if summary != "" {
			a.Summary = summary
		}
		if len(raw) > 0 {
			a.Detail = mergeActivityDetail(a.Detail, raw)
		}
		if turnID != "" {
			a.TurnID = turnID
		}
		a.Revision++
		return *a
	}
	b.seq++
	if id == "" {
		id = fmt.Sprintf("act-%d", b.seq)
	}
	if activityKind == "" {
		activityKind = "tool"
	}
	if status == "" {
		status = "running"
	}
	a := ao.ConversationActivity{
		Kind:         "activity",
		ID:           id,
		Sequence:     b.seq,
		Revision:     1,
		ActivityKind: activityKind,
		Status:       status,
		Summary:      summary,
		Detail:       raw,
		TurnID:       turnID,
		CreatedAt:    createdAt,
	}
	b.actIndex[id] = len(b.activities)
	b.activities = append(b.activities, a)
	return a
}

const maxActivityOutputBytes = 8192

// appendActivityOutput appends streaming stdout/stderr to an activity's
// detail.output field while keeping the tail bounded to maxActivityOutputBytes.
func (b *convBuffer) appendActivityOutput(id, delta, turnID string) (ao.ConversationActivity, bool) {
	if id == "" || delta == "" {
		return ao.ConversationActivity{}, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	idx, ok := b.actIndex[id]
	if !ok {
		b.seq++
		a := ao.ConversationActivity{
			Kind:         "activity",
			ID:           id,
			Sequence:     b.seq,
			Revision:     1,
			ActivityKind: "command",
			Status:       "running",
			Summary:      "Running command",
			TurnID:       turnID,
			CreatedAt:    time.Now().UTC().Format(time.RFC3339Nano),
		}
		idx = len(b.activities)
		b.actIndex[id] = idx
		b.activities = append(b.activities, a)
	}
	a := &b.activities[idx]
	m := map[string]any{}
	if len(a.Detail) > 0 {
		_ = json.Unmarshal(a.Detail, &m)
	}
	cur, _ := m["output"].(string)
	cur = tailString(cur+delta, maxActivityOutputBytes)
	m["output"] = cur
	if bts, err := json.Marshal(m); err == nil {
		a.Detail = bts
	}
	a.Revision++
	return *a, true
}

func mergeActivityDetail(prev, next json.RawMessage) json.RawMessage {
	if len(prev) == 0 || string(prev) == "null" {
		return next
	}
	var pm, nm map[string]any
	if json.Unmarshal(prev, &pm) != nil || pm == nil || json.Unmarshal(next, &nm) != nil || nm == nil {
		return next
	}
	for k, v := range nm {
		if s, ok := v.(string); ok && s == "" {
			continue
		}
		pm[k] = v
	}
	if out, err := json.Marshal(pm); err == nil {
		return out
	}
	return next
}

func tailString(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := s[len(s)-maxBytes:]
	if nl := strings.IndexByte(cut, '\n'); nl >= 0 && nl < len(cut)-1 {
		cut = cut[nl+1:]
	}
	return "…\n" + cut
}

type seqItem struct {
	seq int64
	msg *ao.Message
	act *ao.ConversationActivity
}

func (b *convBuffer) snapshot(sessionID, harness, controller, title string, q ao.ConversationQuery) *ao.Conversation {
	b.mu.Lock()
	defer b.mu.Unlock()

	items := make([]seqItem, 0, len(b.messages)+len(b.activities))
	for i := range b.messages {
		m := b.messages[i]
		items = append(items, seqItem{seq: m.Sequence, msg: &m})
	}
	for i := range b.activities {
		a := b.activities[i]
		items = append(items, seqItem{seq: a.Sequence, act: &a})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].seq < items[j].seq })

	var latestSeq int64
	if len(items) > 0 {
		latestSeq = items[len(items)-1].seq
	}

	filtered := items
	if q.AfterSequence > 0 {
		var next []seqItem
		for _, it := range filtered {
			if it.seq > q.AfterSequence {
				next = append(next, it)
			}
		}
		filtered = next
	}
	if q.BeforeSequence > 0 {
		var prev []seqItem
		for _, it := range filtered {
			if it.seq < q.BeforeSequence {
				prev = append(prev, it)
			}
		}
		filtered = prev
	}

	limit := q.Limit
	if limit <= 0 {
		limit = 200
	}
	hasMoreBefore := false
	if len(filtered) > limit {
		if q.AfterSequence > 0 && q.BeforeSequence == 0 {
			filtered = filtered[:limit]
		} else {
			filtered = filtered[len(filtered)-limit:]
			hasMoreBefore = true
		}
	}

	var oldestSeq int64
	if len(filtered) > 0 {
		oldestSeq = filtered[0].seq
		if !hasMoreBefore && q.BeforeSequence > 0 && len(items) > 0 && items[0].seq < oldestSeq {
			hasMoreBefore = true
		}
	}

	msgs := make([]ao.Message, 0, len(filtered))
	acts := make([]ao.ConversationActivity, 0, len(filtered))
	for _, it := range filtered {
		if it.msg != nil {
			msgs = append(msgs, *it.msg)
		} else if it.act != nil {
			acts = append(acts, *it.act)
		}
	}

	return &ao.Conversation{
		SessionID:      sessionID,
		Harness:        harness,
		Mode:           "native",
		Controller:     controller,
		Title:          title,
		LatestSequence: latestSeq,
		OldestSequence: oldestSeq,
		HasMoreBefore:  hasMoreBefore,
		Messages:       msgs,
		Activities:     acts,
	}
}

// findCodexRolloutPath locates the rollout JSONL file for a Codex thread UUID.
func findCodexRolloutPath(homeDir, sessionID string) (string, error) {
	suffix := sessionID + ".jsonl"
	roots := []string{
		filepath.Join(homeDir, ".codex", "sessions"),
		filepath.Join(homeDir, ".codex", "archived_sessions"),
	}
	var found string
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if strings.HasSuffix(d.Name(), suffix) {
				found = path
				return filepath.SkipAll
			}
			return nil
		})
		if found != "" {
			return found, nil
		}
	}
	return "", os.ErrNotExist
}

// readCodexRollout reads a native Codex rollout JSONL into a convBuffer so
// conversation history is available immediately without resuming the CLI.
func readCodexRollout(homeDir, sessionID string) (*convBuffer, error) {
	path, err := findCodexRolloutPath(homeDir, sessionID)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	buf := newConvBuffer()
	br := bufio.NewReaderSize(f, 256*1024)
	callSummary := map[string]string{}

	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			parseCodexRolloutLine(buf, line, callSummary)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return buf, err
		}
	}
	return buf, nil
}

func parseCodexRolloutLine(buf *convBuffer, line []byte, callSummary map[string]string) {
	var rec struct {
		Timestamp string          `json:"timestamp"`
		Type      string          `json:"type"`
		Payload   json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(line, &rec); err != nil {
		return
	}
	switch rec.Type {
	case "event_msg":
		var p struct {
			Type    string          `json:"type"`
			Message string          `json:"message"`
			TurnID  string          `json:"turn_id"`
			Item    json.RawMessage `json:"item"`
		}
		if err := json.Unmarshal(rec.Payload, &p); err != nil {
			return
		}
		switch p.Type {
		case "user_message":
			if text := strings.TrimSpace(p.Message); text != "" {
				buf.upsertMessage("", "user", text, p.TurnID, "", rec.Timestamp, false)
			}
		case "agent_message":
			if text := strings.TrimSpace(p.Message); text != "" {
				buf.upsertMessage("", "assistant", text, p.TurnID, "", rec.Timestamp, false)
			}
		case "item_completed":
			parseCodexCompletedItem(buf, p.Item, p.TurnID, rec.Timestamp)
		}
	case "response_item":
		var p struct {
			Type      string          `json:"type"`
			Name      string          `json:"name"`
			CallID    string          `json:"call_id"`
			Arguments string          `json:"arguments"`
			Input     string          `json:"input"`
			Output    json.RawMessage `json:"output"`
		}
		if err := json.Unmarshal(rec.Payload, &p); err != nil {
			return
		}
		switch p.Type {
		case "function_call":
			summary := summarizeToolCall(p.Name, p.Arguments, p.Input)
			id := p.CallID
			if id != "" {
				callSummary[id] = summary
			}
			kind := "tool"
			if p.Name == "exec_command" || p.Name == "shell_command" || p.Name == "shell" {
				kind = "command"
			} else if p.Name == "apply_patch" {
				kind = "file_change"
			}
			detail := toolCallDetail(p.Name, p.Arguments, p.Input)
			buf.upsertActivity(id, kind, "completed", summary, detail, "", rec.Timestamp)
		case "function_call_output":
			if p.CallID != "" {
				sum := callSummary[p.CallID]
				var detail map[string]any
				if out := cleanCodexToolOutput(extractCodexOutputText(p.Output)); out != "" {
					detail = map[string]any{"output": tailString(out, maxActivityOutputBytes)}
				}
				buf.upsertActivity(p.CallID, "", "completed", sum, detail, "", rec.Timestamp)
			}
		}
	}
}

func parseCodexCompletedItem(buf *convBuffer, raw json.RawMessage, turnID, ts string) {
	if len(raw) == 0 {
		return
	}
	var it struct {
		Type             string          `json:"type"`
		ID               string          `json:"id"`
		Text             string          `json:"text"`
		Command          json.RawMessage `json:"command"`
		ParsedCmd        []struct {
			Cmd string `json:"cmd"`
		} `json:"parsed_cmd"`
		Status           string `json:"status"`
		AggregatedOutput string `json:"aggregated_output"`
		ExitCode         *int   `json:"exit_code"`
		Content          []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &it) != nil {
		return
	}
	switch it.Type {
	case "UserMessage", "userMessage":
		var parts []string
		for _, c := range it.Content {
			if c.Text != "" {
				parts = append(parts, c.Text)
			}
		}
		if text := strings.TrimSpace(strings.Join(parts, "\n")); text != "" {
			buf.upsertMessage(it.ID, "user", text, turnID, "", ts, false)
		}
	case "AgentMessage", "agentMessage":
		var parts []string
		for _, c := range it.Content {
			if c.Text != "" {
				parts = append(parts, c.Text)
			}
		}
		if it.Text != "" && len(parts) == 0 {
			parts = append(parts, it.Text)
		}
		if text := strings.TrimSpace(strings.Join(parts, "\n")); text != "" {
			buf.upsertMessage(it.ID, "assistant", text, turnID, "", ts, false)
		}
	case "CommandExecution", "commandExecution":
		cmd := formatCodexCommand(it.Command)
		if cmd == "" && len(it.ParsedCmd) > 0 {
			cmd = it.ParsedCmd[0].Cmd
		}
		sum := "Ran command"
		if cmd != "" {
			sum = "Ran " + truncateOneLine(cmd, 110)
		}
		detail := codexCommandDetail(cmd, it.AggregatedOutput, it.ExitCode)
		buf.upsertActivity(it.ID, "command", normalizeCodexStatus(it.Status), sum, detail, turnID, ts)
	}
}

func formatCodexCommand(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var arr []string
	if json.Unmarshal(raw, &arr) == nil && len(arr) > 0 {
		// Unwrap ["/bin/bash", "-lc", "<actual command>"] for clean display.
		if len(arr) >= 3 && (strings.HasSuffix(arr[0], "bash") || strings.HasSuffix(arr[0], "sh")) && (arr[1] == "-lc" || arr[1] == "-c") {
			return strings.TrimSpace(arr[2])
		}
		return strings.TrimSpace(strings.Join(arr, " "))
	}
	return ""
}

func extractCodexOutputText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var arr []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &arr) == nil {
		var parts []string
		for _, item := range arr {
			if item.Text != "" {
				parts = append(parts, item.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// readCopilotEvents reads a native Copilot CLI events.jsonl into a convBuffer.
func readCopilotEvents(homeDir, sessionID string) (*convBuffer, error) {
	path := filepath.Join(homeDir, ".copilot", "session-state", sessionID, "events.jsonl")
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	buf := newConvBuffer()
	br := bufio.NewReaderSize(f, 256*1024)

	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			parseCopilotEventLine(buf, line)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return buf, err
		}
	}
	return buf, nil
}

func parseCopilotEventLine(buf *convBuffer, line []byte) {
	var rec struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		Timestamp string          `json:"timestamp"`
		Data      json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(line, &rec); err != nil {
		return
	}
	switch rec.Type {
	case "user.message":
		var d struct {
			Content string `json:"content"`
		}
		if json.Unmarshal(rec.Data, &d) == nil {
			if text := strings.TrimSpace(d.Content); text != "" {
				buf.upsertMessage(rec.ID, "user", text, "", "", rec.Timestamp, false)
			}
		}
	case "assistant.message":
		var d struct {
			MessageID string `json:"messageId"`
			Content   string `json:"content"`
		}
		if json.Unmarshal(rec.Data, &d) == nil {
			if text := strings.TrimSpace(d.Content); text != "" {
				id := d.MessageID
				if id == "" {
					id = rec.ID
				}
				buf.upsertMessage(id, "assistant", text, "", "", rec.Timestamp, false)
			}
		}
	case "tool.execution_start":
		var d struct {
			ToolCallID string          `json:"toolCallId"`
			ToolName   string          `json:"toolName"`
			Arguments  json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal(rec.Data, &d) == nil && d.ToolCallID != "" {
			kind := "tool"
			if d.ToolName == "bash" || d.ToolName == "shell" {
				kind = "command"
			} else if d.ToolName == "edit" || d.ToolName == "create" {
				kind = "file_change"
			}
			summary := summarizeToolCall(d.ToolName, string(d.Arguments), "")
			detail := toolCallDetail(d.ToolName, string(d.Arguments), "")
			buf.upsertActivity(d.ToolCallID, kind, "running", summary, detail, "", rec.Timestamp)
		}
	case "tool.execution_complete":
		var d struct {
			ToolCallID string `json:"toolCallId"`
			Success    bool   `json:"success"`
			Result     struct {
				Content string `json:"content"`
			} `json:"result"`
		}
		if json.Unmarshal(rec.Data, &d) == nil && d.ToolCallID != "" {
			status := "completed"
			if !d.Success {
				status = "failed"
			}
			var detail map[string]any
			if out := strings.TrimSpace(d.Result.Content); out != "" {
				detail = map[string]any{"output": tailString(out, maxActivityOutputBytes)}
			}
			buf.upsertActivity(d.ToolCallID, "", status, "", detail, "", rec.Timestamp)
		}
	}
}

func toolCallDetail(name, rawArgs, rawInput string) map[string]any {
	var args map[string]any
	if rawArgs != "" {
		_ = json.Unmarshal([]byte(rawArgs), &args)
	}
	for _, key := range []string{"cmd", "command"} {
		if v, ok := args[key].(string); ok && strings.TrimSpace(v) != "" {
			return map[string]any{"command": strings.TrimSpace(v)}
		}
	}
	if strings.TrimSpace(rawInput) != "" && name == "apply_patch" {
		return map[string]any{"command": truncateOneLine(rawInput, 200)}
	}
	return nil
}

// cleanCodexToolOutput extracts the actual command output body from Codex's
// exec_command wrapper ("Chunk ID: ...\nOutput:\n...") when present.
func cleanCodexToolOutput(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if idx := strings.Index(raw, "\nOutput:\n"); idx >= 0 {
		return strings.TrimSpace(raw[idx+len("\nOutput:\n"):])
	}
	return raw
}

func summarizeToolCall(name, rawArgs, rawInput string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "tool"
	}
	var args map[string]any
	if rawArgs != "" {
		_ = json.Unmarshal([]byte(rawArgs), &args)
	}
	for _, key := range []string{"cmd", "command", "description", "path", "file_path", "pattern", "query", "url"} {
		if v, ok := args[key].(string); ok && strings.TrimSpace(v) != "" {
			return fmt.Sprintf("%s: %s", name, truncateOneLine(v, 110))
		}
	}
	if strings.TrimSpace(rawInput) != "" {
		return fmt.Sprintf("%s: %s", name, truncateOneLine(rawInput, 110))
	}
	return name
}

func truncateOneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-1]) + "…"
}
