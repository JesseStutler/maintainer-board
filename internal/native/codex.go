package native

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"maintainer-board/internal/ao"
)

// codexApprovalSettings translates the board's approval mode to native
// codex app-server approvalPolicy, thread sandbox, and turn sandboxPolicy.
func codexApprovalSettings(mode string) (approvalPolicy, sandbox string, sandboxPolicy map[string]any) {
	switch strings.TrimSpace(mode) {
	case "bypass-permissions":
		return "never", "danger-full-access", map[string]any{"type": "dangerFullAccess"}
	case "auto", "accept-edits":
		return "on-request", "workspace-write", map[string]any{"type": "workspaceWrite", "networkAccess": true}
	default:
		return "on-request", "workspace-write", map[string]any{"type": "workspaceWrite"}
	}
}

// codexController manages one live `codex app-server` subprocess for a single
// native Codex thread. Zero AO prompts or developerInstructions are injected.
type codexController struct {
	id   string
	cwd  string
	rpc  *rpcConn
	buf  *convBuffer
	emit func(eventType string, payload map[string]any)

	mu           sync.Mutex
	state        string // "ready", "active", "exited"
	activeTurnID string
	model        string
	effort       string
	approvalMode string
	models       []ao.ConversationModel
	lastActivity time.Time
}

func startCodexSession(
	ctx context.Context,
	codexPath string,
	cwd string,
	resumeThreadID string,
	model string,
	effort string,
	approvalMode string,
	initialBuf *convBuffer,
	emit func(eventType string, payload map[string]any),
) (*codexController, error) {
	if initialBuf == nil {
		initialBuf = newConvBuffer()
	}
	ctrl := &codexController{
		id:           resumeThreadID,
		cwd:          cwd,
		buf:          initialBuf,
		emit:         emit,
		state:        "ready",
		model:        model,
		effort:       effort,
		approvalMode: approvalMode,
		lastActivity: time.Now().UTC(),
	}

	rpc, err := startRPC(
		cwd,
		codexPath,
		[]string{"app-server"},
		ctrl.handleNotify,
		ctrl.handleRequest,
		ctrl.handleExit,
	)
	if err != nil {
		return nil, err
	}
	ctrl.rpc = rpc

	if err := rpc.Call(ctx, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "maintainer-board", "version": "0.1.0"},
		"capabilities": map[string]any{"experimentalApi": true},
	}, nil); err != nil {
		_ = rpc.Close()
		return nil, fmt.Errorf("codex initialize: %w", err)
	}
	_ = rpc.Notify("initialized", map[string]any{})

	// Query available models on this app-server connection.
	if models, err := fetchCodexModelsRPC(ctx, rpc); err == nil {
		ctrl.models = models
	}

	approvalPolicy, sandbox, _ := codexApprovalSettings(approvalMode)

	if resumeThreadID == "" {
		params := map[string]any{
			"cwd":            cwd,
			"approvalPolicy": approvalPolicy,
			"sandbox":        sandbox,
		}
		if model != "" {
			params["model"] = model
		}
		var res struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
			Model           string `json:"model"`
			ReasoningEffort string `json:"reasoningEffort"`
		}
		if err := rpc.Call(ctx, "thread/start", params, &res); err != nil {
			_ = rpc.Close()
			return nil, fmt.Errorf("codex thread/start: %w", err)
		}
		ctrl.id = res.Thread.ID
		if ctrl.model == "" && res.Model != "" {
			ctrl.model = res.Model
		}
		if ctrl.effort == "" && res.ReasoningEffort != "" {
			ctrl.effort = res.ReasoningEffort
		}
	} else {
		params := map[string]any{
			"threadId":       resumeThreadID,
			"cwd":            cwd,
			"approvalPolicy": approvalPolicy,
			"sandbox":        sandbox,
		}
		if model != "" {
			params["model"] = model
		}
		var res struct {
			Thread          codexThreadPayload `json:"thread"`
			Model           string             `json:"model"`
			ReasoningEffort string             `json:"reasoningEffort"`
		}
		if err := rpc.Call(ctx, "thread/resume", params, &res); err != nil {
			_ = rpc.Close()
			return nil, fmt.Errorf("codex thread/resume: %w", err)
		}
		if ctrl.model == "" && res.Model != "" {
			ctrl.model = res.Model
		}
		if ctrl.effort == "" && res.ReasoningEffort != "" {
			ctrl.effort = res.ReasoningEffort
		}
		if len(res.Thread.Turns) > 0 {
			ctrl.buf = convBufferFromCodexThread(res.Thread)
		}
	}

	return ctrl, nil
}

type codexThreadPayload struct {
	ID    string `json:"id"`
	Turns []struct {
		ID    string            `json:"id"`
		Items []json.RawMessage `json:"items"`
	} `json:"turns"`
}

func convBufferFromCodexThread(th codexThreadPayload) *convBuffer {
	buf := newConvBuffer()
	for _, turn := range th.Turns {
		for _, raw := range turn.Items {
			var item struct {
				Type             string `json:"type"`
				ID               string `json:"id"`
				Text             string `json:"text"`
				Command          string `json:"command"`
				AggregatedOutput string `json:"aggregatedOutput"`
				ExitCode         *int   `json:"exitCode"`
				Status           string `json:"status"`
				Content          []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			}
			if json.Unmarshal(raw, &item) != nil {
				continue
			}
			switch item.Type {
			case "userMessage":
				var parts []string
				for _, c := range item.Content {
					if c.Text != "" {
						parts = append(parts, c.Text)
					}
				}
				if text := strings.TrimSpace(strings.Join(parts, "\n")); text != "" {
					buf.upsertMessage(item.ID, "user", text, turn.ID, "", "", false)
				}
			case "agentMessage":
				if text := strings.TrimSpace(item.Text); text != "" {
					buf.upsertMessage(item.ID, "assistant", text, turn.ID, "", "", false)
				}
			case "commandExecution":
				st := normalizeCodexStatus(item.Status)
				sum := "command"
				if item.Command != "" {
					sum = "Ran " + truncateOneLine(item.Command, 110)
				}
				detail := codexCommandDetail(item.Command, item.AggregatedOutput, item.ExitCode)
				buf.upsertActivity(item.ID, "command", st, sum, detail, turn.ID, "")
			case "fileChange":
				buf.upsertActivity(item.ID, "file_change", normalizeCodexStatus(item.Status), "File changes", nil, turn.ID, "")
			case "mcpToolCall":
				buf.upsertActivity(item.ID, "tool", normalizeCodexStatus(item.Status), "MCP tool call", nil, turn.ID, "")
			case "webSearch":
				buf.upsertActivity(item.ID, "tool", normalizeCodexStatus(item.Status), "Web search", nil, turn.ID, "")
			}
		}
	}
	return buf
}

func codexCommandDetail(command, aggregatedOutput string, exitCode *int) map[string]any {
	m := map[string]any{}
	if strings.TrimSpace(command) != "" {
		m["command"] = strings.TrimSpace(command)
	}
	if strings.TrimSpace(aggregatedOutput) != "" {
		m["output"] = tailString(strings.TrimSpace(aggregatedOutput), maxActivityOutputBytes)
	}
	if exitCode != nil {
		m["exitCode"] = *exitCode
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

func normalizeCodexStatus(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "inprogress", "in_progress", "running":
		return "running"
	case "failed", "declined", "error":
		return "failed"
	default:
		return "completed"
	}
}

func (c *codexController) ID() string      { return c.id }
func (c *codexController) Harness() string { return "codex" }

func (c *codexController) State() (string, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.rpc.Alive() {
		return "exited", c.lastActivity
	}
	return c.state, c.lastActivity
}

func (c *codexController) Conversation(title string, q ao.ConversationQuery) *ao.Conversation {
	return c.buf.snapshot(c.id, "codex", "native", title, q)
}

func (c *codexController) Models() *ao.ConversationModels {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ao.ConversationModel, len(c.models))
	copy(out, c.models)
	return &ao.ConversationModels{
		Models: out,
		Selected: ao.ConversationSettings{
			Model:           c.model,
			ReasoningEffort: c.effort,
			ApprovalMode:    c.approvalMode,
		},
	}
}

func (c *codexController) UpdateSettings(s ao.ConversationTurnSettings) ao.ConversationSettings {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s.Model != nil {
		c.model = strings.TrimSpace(*s.Model)
	}
	if s.ReasoningEffort != nil {
		c.effort = strings.TrimSpace(*s.ReasoningEffort)
	}
	if s.ApprovalMode != nil {
		c.approvalMode = strings.TrimSpace(*s.ApprovalMode)
	}
	return ao.ConversationSettings{
		Model:           c.model,
		ReasoningEffort: c.effort,
		ApprovalMode:    c.approvalMode,
	}
}

func (c *codexController) SendMessage(ctx context.Context, text, clientMsgID string) (*ao.SendResult, error) {
	c.mu.Lock()
	model := c.model
	effort := c.effort
	approvalMode := c.approvalMode
	c.mu.Unlock()

	approvalPolicy, _, sandboxPolicy := codexApprovalSettings(approvalMode)
	params := map[string]any{
		"threadId":       c.id,
		"input":          []map[string]any{{"type": "text", "text": text}},
		"approvalPolicy": approvalPolicy,
		"sandboxPolicy":  sandboxPolicy,
	}
	if model != "" {
		params["model"] = model
	}
	if effort != "" {
		params["effort"] = effort
	}

	var res struct {
		Turn struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"turn"`
	}
	if err := c.rpc.Call(ctx, "turn/start", params, &res); err != nil {
		return nil, fmt.Errorf("codex turn/start: %w", err)
	}

	c.mu.Lock()
	c.state = "active"
	c.activeTurnID = res.Turn.ID
	c.lastActivity = time.Now().UTC()
	c.mu.Unlock()

	// Record user message immediately with clientMsgID so the drawer reconciles
	// its optimistic bubble smoothly.
	msg := c.buf.upsertMessage("user-"+res.Turn.ID, "user", text, res.Turn.ID, clientMsgID, "", false)
	c.emit("conversation.updated", map[string]any{
		"sessionId": c.id,
		"sequence":  msg.Sequence,
	})
	c.emit("session.updated", map[string]any{"sessionId": c.id})

	return &ao.SendResult{
		TurnID: res.Turn.ID,
		State:  "active",
	}, nil
}

func (c *codexController) Interrupt(ctx context.Context) error {
	c.mu.Lock()
	turnID := c.activeTurnID
	c.mu.Unlock()
	if turnID == "" {
		return nil
	}
	return c.rpc.Call(ctx, "turn/interrupt", map[string]any{
		"threadId": c.id,
		"turnId":   turnID,
	}, nil)
}

func (c *codexController) Close() error {
	return c.rpc.Close()
}

func (c *codexController) handleNotify(method string, params json.RawMessage) {
	now := time.Now().UTC()
	switch method {
	case "turn/started":
		var p struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		if json.Unmarshal(params, &p) == nil {
			c.mu.Lock()
			c.state = "active"
			if p.Turn.ID != "" {
				c.activeTurnID = p.Turn.ID
			}
			c.lastActivity = now
			c.mu.Unlock()
			c.emit("session.updated", map[string]any{"sessionId": c.id})
		}
	case "item/agentMessage/delta":
		var p struct {
			TurnID string `json:"turnId"`
			ItemID string `json:"itemId"`
			Delta  string `json:"delta"`
		}
		if json.Unmarshal(params, &p) == nil && p.Delta != "" {
			msg := c.buf.appendMessageDelta(p.ItemID, "assistant", p.Delta, p.TurnID)
			c.mu.Lock()
			c.lastActivity = now
			c.mu.Unlock()
			c.emit("conversation.updated", map[string]any{"sessionId": c.id, "sequence": msg.Sequence})
		}
	case "item/commandExecution/outputDelta":
		var p struct {
			TurnID string `json:"turnId"`
			ItemID string `json:"itemId"`
			Delta  string `json:"delta"`
		}
		if json.Unmarshal(params, &p) == nil && p.Delta != "" {
			if act, ok := c.buf.appendActivityOutput(p.ItemID, p.Delta, p.TurnID); ok {
				c.mu.Lock()
				c.lastActivity = now
				c.mu.Unlock()
				c.emit("conversation.updated", map[string]any{"sessionId": c.id, "sequence": act.Sequence})
			}
		}
	case "item/started", "item/completed":
		var p struct {
			TurnID string `json:"turnId"`
			Item   struct {
				Type             string `json:"type"`
				ID               string `json:"id"`
				Text             string `json:"text"`
				Command          string `json:"command"`
				AggregatedOutput string `json:"aggregatedOutput"`
				ExitCode         *int   `json:"exitCode"`
				Status           string `json:"status"`
			} `json:"item"`
		}
		if json.Unmarshal(params, &p) != nil {
			return
		}
		completed := method == "item/completed"
		switch p.Item.Type {
		case "agentMessage":
			if completed || p.Item.Text != "" {
				msg := c.buf.upsertMessage(p.Item.ID, "assistant", p.Item.Text, p.TurnID, "", "", !completed)
				c.emit("conversation.updated", map[string]any{"sessionId": c.id, "sequence": msg.Sequence})
			}
		case "commandExecution":
			st := "running"
			if completed {
				st = normalizeCodexStatus(p.Item.Status)
			}
			sum := "Running command"
			if p.Item.Command != "" {
				prefix := "Running "
				if completed {
					prefix = "Ran "
				}
				sum = prefix + truncateOneLine(p.Item.Command, 110)
			}
			detail := codexCommandDetail(p.Item.Command, p.Item.AggregatedOutput, p.Item.ExitCode)
			act := c.buf.upsertActivity(p.Item.ID, "command", st, sum, detail, p.TurnID, "")
			c.emit("conversation.updated", map[string]any{"sessionId": c.id, "sequence": act.Sequence})
		case "fileChange":
			st := "running"
			if completed {
				st = normalizeCodexStatus(p.Item.Status)
			}
			act := c.buf.upsertActivity(p.Item.ID, "file_change", st, "File changes", nil, p.TurnID, "")
			c.emit("conversation.updated", map[string]any{"sessionId": c.id, "sequence": act.Sequence})
		case "mcpToolCall", "webSearch":
			st := "running"
			if completed {
				st = normalizeCodexStatus(p.Item.Status)
			}
			label := "Tool call"
			if p.Item.Type == "webSearch" {
				label = "Web search"
			}
			act := c.buf.upsertActivity(p.Item.ID, "tool", st, label, nil, p.TurnID, "")
			c.emit("conversation.updated", map[string]any{"sessionId": c.id, "sequence": act.Sequence})
		}
		c.mu.Lock()
		c.lastActivity = now
		c.mu.Unlock()
	case "turn/completed":
		var p struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		_ = json.Unmarshal(params, &p)
		c.buf.finishStreaming(p.Turn.ID)
		c.mu.Lock()
		c.state = "ready"
		c.activeTurnID = ""
		c.lastActivity = now
		c.mu.Unlock()
		c.emit("conversation.updated", map[string]any{"sessionId": c.id})
		c.emit("session.updated", map[string]any{"sessionId": c.id})
	}
}

func (c *codexController) handleRequest(_ json.RawMessage, method string, _ json.RawMessage) (any, error) {
	c.mu.Lock()
	mode := c.approvalMode
	c.mu.Unlock()
	autoApprove := mode == "bypass-permissions" || mode == "auto" || mode == "accept-edits"
	switch method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval", "execCommandApproval", "applyPatchApproval":
		if autoApprove {
			return map[string]any{"decision": "accept"}, nil
		}
		return map[string]any{"decision": "decline"}, nil
	case "item/permissions/requestApproval":
		if autoApprove {
			return map[string]any{"decision": "accept"}, nil
		}
		return map[string]any{"decision": "decline"}, nil
	default:
		return map[string]any{}, nil
	}
}

func (c *codexController) handleExit(_ error) {
	c.buf.finishStreaming("")
	c.mu.Lock()
	c.state = "exited"
	c.activeTurnID = ""
	c.mu.Unlock()
	c.emit("session.updated", map[string]any{"sessionId": c.id})
}

func fetchCodexModelsRPC(ctx context.Context, rpc *rpcConn) ([]ao.ConversationModel, error) {
	var res struct {
		Data []struct {
			ID                        string `json:"id"`
			Model                     string `json:"model"`
			DisplayName               string `json:"displayName"`
			Description               string `json:"description"`
			IsDefault                 bool   `json:"isDefault"`
			Hidden                    bool   `json:"hidden"`
			DefaultReasoningEffort    string `json:"defaultReasoningEffort"`
			SupportedReasoningEfforts []struct {
				ReasoningEffort string `json:"reasoningEffort"`
			} `json:"supportedReasoningEfforts"`
		} `json:"data"`
	}
	if err := rpc.Call(ctx, "model/list", map[string]any{"limit": 50}, &res); err != nil {
		return nil, err
	}
	out := make([]ao.ConversationModel, 0, len(res.Data))
	for _, m := range res.Data {
		if m.Hidden {
			continue
		}
		id := m.Model
		if id == "" {
			id = m.ID
		}
		efforts := make([]string, 0, len(m.SupportedReasoningEfforts))
		for _, e := range m.SupportedReasoningEfforts {
			if e.ReasoningEffort != "" {
				efforts = append(efforts, e.ReasoningEffort)
			}
		}
		label := m.DisplayName
		if label == "" {
			label = id
		}
		out = append(out, ao.ConversationModel{
			ID:            id,
			DisplayName:   label,
			Description:   m.Description,
			Default:       m.IsDefault,
			Efforts:       efforts,
			DefaultEffort: m.DefaultReasoningEffort,
		})
	}
	return out, nil
}

func listCodexModels(ctx context.Context, codexPath, cwd string) (*ao.AgentModels, error) {
	rpc, err := startRPC(cwd, codexPath, []string{"app-server"}, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	defer rpc.Close()

	if err := rpc.Call(ctx, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "maintainer-board", "version": "0.1.0"},
		"capabilities": map[string]any{"experimentalApi": true},
	}, nil); err != nil {
		return nil, err
	}
	_ = rpc.Notify("initialized", map[string]any{})

	models, err := fetchCodexModelsRPC(ctx, rpc)
	if err != nil {
		return nil, err
	}
	out := make([]ao.AgentModel, 0, len(models))
	for _, m := range models {
		out = append(out, ao.AgentModel{
			ID:            m.ID,
			Label:         m.DisplayName,
			IsDefault:     m.Default,
			Efforts:       m.Efforts,
			DefaultEffort: m.DefaultEffort,
		})
	}
	return &ao.AgentModels{AgentID: "codex", Models: out}, nil
}
