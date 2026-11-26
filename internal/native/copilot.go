package native

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"maintainer-board/internal/ao"
)

// copilotAllowAllValue maps board approvalMode to Copilot ACP's allow_all config option.
func copilotAllowAllValue(mode string) string {
	switch strings.TrimSpace(mode) {
	case "bypass-permissions":
		return "on"
	case "auto", "accept-edits":
		return "auto"
	default:
		return "off"
	}
}

// copilotController manages one live `copilot --acp` subprocess for a single
// native Copilot session. Zero AO agents or custom prompt wrappers are injected.
type copilotController struct {
	id   string
	cwd  string
	rpc  *rpcConn
	buf  *convBuffer
	emit func(eventType string, payload map[string]any)

	turnSeq atomic.Int64

	mu           sync.Mutex
	state        string // "ready", "active", "exited"
	activeTurnID string
	streamMsgID  string
	model        string
	effort       string
	approvalMode string
	models       []ao.ConversationModel
	lastActivity time.Time
	replaying    bool
}

type acpConfigOption struct {
	ID           string `json:"id"`
	CurrentValue string `json:"currentValue"`
	Options      []struct {
		Value       string `json:"value"`
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"options"`
}

func startCopilotSession(
	ctx context.Context,
	copilotPath string,
	cwd string,
	resumeSessionID string,
	model string,
	effort string,
	approvalMode string,
	initialBuf *convBuffer,
	emit func(eventType string, payload map[string]any),
) (*copilotController, error) {
	if initialBuf == nil {
		initialBuf = newConvBuffer()
	}
	ctrl := &copilotController{
		id:           resumeSessionID,
		cwd:          cwd,
		buf:          initialBuf,
		emit:         emit,
		state:        "ready",
		model:        model,
		effort:       effort,
		approvalMode: approvalMode,
		lastActivity: time.Now().UTC(),
	}

	args := []string{"--acp", "--no-auto-update"}
	if approvalMode == "bypass-permissions" || approvalMode == "auto" {
		args = append(args, "--allow-all-tools")
	}

	rpc, err := startRPC(
		cwd,
		copilotPath,
		args,
		ctrl.handleNotify,
		ctrl.handleRequest,
		ctrl.handleExit,
	)
	if err != nil {
		return nil, err
	}
	ctrl.rpc = rpc

	if err := rpc.Call(ctx, "initialize", map[string]any{
		"protocolVersion":    1,
		"clientInfo":         map[string]any{"name": "maintainer-board", "version": "0.1.0"},
		"clientCapabilities": map[string]any{},
	}, nil); err != nil {
		_ = rpc.Close()
		return nil, fmt.Errorf("copilot initialize: %w", err)
	}

	if resumeSessionID == "" {
		var res struct {
			SessionID string `json:"sessionId"`
			Models    struct {
				CurrentModelID  string `json:"currentModelId"`
				AvailableModels []struct {
					ModelID     string `json:"modelId"`
					Name        string `json:"name"`
					Description string `json:"description"`
				} `json:"availableModels"`
			} `json:"models"`
			ConfigOptions []acpConfigOption `json:"configOptions"`
		}
		if err := rpc.Call(ctx, "session/new", map[string]any{
			"cwd":        cwd,
			"mcpServers": []any{},
		}, &res); err != nil {
			_ = rpc.Close()
			return nil, fmt.Errorf("copilot session/new: %w", err)
		}
		ctrl.id = res.SessionID
		ctrl.applyACPMetadata(res.Models.CurrentModelID, res.ConfigOptions)
	} else {
		ctrl.mu.Lock()
		ctrl.replaying = true
		ctrl.buf = newConvBuffer()
		ctrl.mu.Unlock()

		var res struct {
			Models struct {
				CurrentModelID string `json:"currentModelId"`
			} `json:"models"`
			ConfigOptions []acpConfigOption `json:"configOptions"`
		}
		err := rpc.Call(ctx, "session/load", map[string]any{
			"sessionId":  resumeSessionID,
			"cwd":        cwd,
			"mcpServers": []any{},
		}, &res)
		ctrl.mu.Lock()
		ctrl.replaying = false
		ctrl.mu.Unlock()
		if err != nil {
			_ = rpc.Close()
			return nil, fmt.Errorf("copilot session/load: %w", err)
		}
		ctrl.applyACPMetadata(res.Models.CurrentModelID, res.ConfigOptions)
	}

	// Apply requested settings if provided.
	if model != "" {
		_ = ctrl.setConfig(ctx, "model", model)
	}
	if effort != "" {
		_ = ctrl.setConfig(ctx, "reasoning_effort", effort)
	}
	if approvalMode != "" {
		_ = ctrl.setConfig(ctx, "allow_all", copilotAllowAllValue(approvalMode))
	}

	return ctrl, nil
}

func (c *copilotController) applyACPMetadata(currentModel string, opts []acpConfigOption) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var efforts []string
	var defaultEffort string
	for _, opt := range opts {
		switch opt.ID {
		case "reasoning_effort":
			defaultEffort = opt.CurrentValue
			for _, o := range opt.Options {
				if o.Value != "" {
					efforts = append(efforts, o.Value)
				}
			}
			if c.effort == "" && opt.CurrentValue != "" {
				c.effort = opt.CurrentValue
			}
		case "model":
			if c.model == "" && opt.CurrentValue != "" {
				c.model = opt.CurrentValue
			}
			models := make([]ao.ConversationModel, 0, len(opt.Options))
			for _, o := range opt.Options {
				models = append(models, ao.ConversationModel{
					ID:          o.Value,
					DisplayName: o.Name,
					Description: o.Description,
					Default:     o.Value == opt.CurrentValue,
				})
			}
			c.models = models
		}
	}
	if c.model == "" && currentModel != "" {
		c.model = currentModel
	}
	for i := range c.models {
		if len(efforts) > 0 {
			c.models[i].Efforts = efforts
			c.models[i].DefaultEffort = defaultEffort
		}
	}
}

func (c *copilotController) setConfig(ctx context.Context, configID, value string) error {
	var res struct {
		ConfigOptions []acpConfigOption `json:"configOptions"`
	}
	if err := c.rpc.Call(ctx, "session/set_config_option", map[string]any{
		"sessionId": c.id,
		"configId":  configID,
		"value":     value,
	}, &res); err != nil {
		return err
	}
	if len(res.ConfigOptions) > 0 {
		c.applyACPMetadata("", res.ConfigOptions)
	}
	return nil
}

func (c *copilotController) ID() string      { return c.id }
func (c *copilotController) Harness() string { return "copilot" }

func (c *copilotController) State() (string, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.rpc.Alive() {
		return "exited", c.lastActivity
	}
	return c.state, c.lastActivity
}

func (c *copilotController) Conversation(title string, q ao.ConversationQuery) *ao.Conversation {
	return c.buf.snapshot(c.id, "copilot", "native", title, q)
}

func (c *copilotController) Models() *ao.ConversationModels {
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

func (c *copilotController) UpdateSettings(s ao.ConversationTurnSettings) ao.ConversationSettings {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c.mu.Lock()
	if s.Model != nil {
		c.model = strings.TrimSpace(*s.Model)
	}
	if s.ReasoningEffort != nil {
		c.effort = strings.TrimSpace(*s.ReasoningEffort)
	}
	if s.ApprovalMode != nil {
		c.approvalMode = strings.TrimSpace(*s.ApprovalMode)
	}
	model, effort, mode := c.model, c.effort, c.approvalMode
	c.mu.Unlock()

	if s.Model != nil && model != "" {
		_ = c.setConfig(ctx, "model", model)
	}
	if s.ReasoningEffort != nil && effort != "" {
		_ = c.setConfig(ctx, "reasoning_effort", effort)
	}
	if s.ApprovalMode != nil {
		_ = c.setConfig(ctx, "allow_all", copilotAllowAllValue(mode))
	}
	return ao.ConversationSettings{
		Model:           model,
		ReasoningEffort: effort,
		ApprovalMode:    mode,
	}
}

func (c *copilotController) SendMessage(_ context.Context, text, clientMsgID string) (*ao.SendResult, error) {
	turnID := fmt.Sprintf("turn-%d", c.turnSeq.Add(1))

	c.mu.Lock()
	if c.state == "active" {
		c.mu.Unlock()
		return nil, fmt.Errorf("session is already running a turn")
	}
	c.state = "active"
	c.activeTurnID = turnID
	c.streamMsgID = "assistant-" + turnID
	c.lastActivity = time.Now().UTC()
	c.mu.Unlock()

	msg := c.buf.upsertMessage("user-"+turnID, "user", text, turnID, clientMsgID, "", false)
	c.emit("conversation.updated", map[string]any{"sessionId": c.id, "sequence": msg.Sequence})
	c.emit("session.updated", map[string]any{"sessionId": c.id})

	// session/prompt blocks until the Copilot turn finishes; run asynchronously
	// so the HTTP endpoint returns immediately while updates stream over SSE.
	go func() {
		var res struct {
			StopReason string `json:"stopReason"`
		}
		err := c.rpc.Call(context.Background(), "session/prompt", map[string]any{
			"sessionId": c.id,
			"prompt":    []map[string]any{{"type": "text", "text": text}},
		}, &res)
		if err != nil {
			c.buf.upsertActivity("err-"+turnID, "tool", "failed", fmt.Sprintf("Turn error: %v", err), nil, turnID, "")
		}
		c.buf.finishStreaming(turnID)
		c.mu.Lock()
		c.state = "ready"
		c.activeTurnID = ""
		c.streamMsgID = ""
		c.lastActivity = time.Now().UTC()
		c.mu.Unlock()
		c.emit("conversation.updated", map[string]any{"sessionId": c.id})
		c.emit("session.updated", map[string]any{"sessionId": c.id})
	}()

	return &ao.SendResult{
		TurnID: turnID,
		State:  "active",
	}, nil
}

func (c *copilotController) Interrupt(_ context.Context) error {
	return c.rpc.Notify("session/cancel", map[string]any{
		"sessionId": c.id,
	})
}

func (c *copilotController) Close() error {
	return c.rpc.Close()
}

func (c *copilotController) handleNotify(method string, params json.RawMessage) {
	if method != "session/update" {
		return
	}
	var p struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			SessionUpdate string            `json:"sessionUpdate"`
			MessageID     string            `json:"messageId"`
			Content       json.RawMessage   `json:"content"`
			ToolCallID    string            `json:"toolCallId"`
			Title         string            `json:"title"`
			Kind          string            `json:"kind"`
			Status        string            `json:"status"`
			RawInput      json.RawMessage   `json:"rawInput"`
			RawOutput     json.RawMessage   `json:"rawOutput"`
			ConfigOpts    []acpConfigOption `json:"configOptions"`
		} `json:"update"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	u := p.Update
	now := time.Now().UTC()

	c.mu.Lock()
	replaying := c.replaying
	turnID := c.activeTurnID
	streamMsgID := c.streamMsgID
	c.lastActivity = now
	c.mu.Unlock()

	switch u.SessionUpdate {
	case "user_message_chunk":
		var chunk struct {
			Text string `json:"text"`
		}
		if replaying && json.Unmarshal(u.Content, &chunk) == nil && chunk.Text != "" {
			id := u.MessageID
			if id == "" {
				id = fmt.Sprintf("replay-user-%d", now.UnixNano())
			}
			c.buf.appendMessageDelta(id, "user", chunk.Text, "")
			c.buf.finishStreaming("")
		}
	case "agent_message_chunk":
		var chunk struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(u.Content, &chunk) == nil && chunk.Text != "" {
			id := u.MessageID
			if id == "" {
				id = streamMsgID
			}
			if id == "" {
				id = fmt.Sprintf("agent-%d", now.UnixNano())
			}
			msg := c.buf.appendMessageDelta(id, "assistant", chunk.Text, turnID)
			if replaying {
				c.buf.finishStreaming("")
			} else {
				c.emit("conversation.updated", map[string]any{"sessionId": c.id, "sequence": msg.Sequence})
			}
		}
	case "tool_call", "tool_call_update":
		if u.ToolCallID != "" {
			status := strings.ToLower(strings.TrimSpace(u.Status))
			switch status {
			case "in_progress", "pending", "":
				status = "running"
			case "completed", "done":
				status = "completed"
			case "failed", "error":
				status = "failed"
			}
			kind := "tool"
			if u.Kind == "execute" {
				kind = "command"
			} else if u.Kind == "edit" {
				kind = "file_change"
			}
			detail := copilotToolDetail(u.RawInput, u.RawOutput, u.Content)
			act := c.buf.upsertActivity(u.ToolCallID, kind, status, truncateOneLine(u.Title, 110), detail, turnID, "")
			if !replaying {
				c.emit("conversation.updated", map[string]any{"sessionId": c.id, "sequence": act.Sequence})
			}
		}
	case "config_option_update":
		if len(u.ConfigOpts) > 0 {
			c.applyACPMetadata("", u.ConfigOpts)
		}
	}
}

func copilotToolDetail(rawInput, rawOutput, rawContent json.RawMessage) map[string]any {
	m := map[string]any{}
	if len(rawInput) > 0 {
		var in map[string]any
		if json.Unmarshal(rawInput, &in) == nil {
			for _, k := range []string{"command", "cmd"} {
				if s, ok := in[k].(string); ok && strings.TrimSpace(s) != "" {
					m["command"] = strings.TrimSpace(s)
					break
				}
			}
		}
	}
	var outParts []string
	if len(rawContent) > 0 {
		var blocks []struct {
			Type    string `json:"type"`
			Content struct {
				Text string `json:"text"`
			} `json:"content"`
			Text string `json:"text"`
		}
		if json.Unmarshal(rawContent, &blocks) == nil {
			for _, b := range blocks {
				if t := strings.TrimSpace(b.Content.Text); t != "" {
					outParts = append(outParts, t)
				} else if t := strings.TrimSpace(b.Text); t != "" {
					outParts = append(outParts, t)
				}
			}
		}
	}
	if len(outParts) == 0 && len(rawOutput) > 0 {
		var s string
		if json.Unmarshal(rawOutput, &s) == nil && strings.TrimSpace(s) != "" {
			outParts = append(outParts, strings.TrimSpace(s))
		} else {
			var obj map[string]any
			if json.Unmarshal(rawOutput, &obj) == nil {
				for _, k := range []string{"output", "content", "stdout", "text"} {
					if v, ok := obj[k].(string); ok && strings.TrimSpace(v) != "" {
						outParts = append(outParts, strings.TrimSpace(v))
						break
					}
				}
			}
		}
	}
	if len(outParts) > 0 {
		m["output"] = tailString(strings.Join(outParts, "\n"), maxActivityOutputBytes)
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

func (c *copilotController) handleRequest(_ json.RawMessage, method string, params json.RawMessage) (any, error) {
	if method != "session/request_permission" {
		return map[string]any{}, nil
	}
	var p struct {
		Options []struct {
			OptionID string `json:"optionId"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	_ = json.Unmarshal(params, &p)

	c.mu.Lock()
	mode := c.approvalMode
	c.mu.Unlock()
	autoApprove := mode == "bypass-permissions" || mode == "auto" || mode == "accept-edits"

	if autoApprove {
		for _, want := range []string{"allow_always", "allow_once"} {
			for _, o := range p.Options {
				if o.Kind == want || o.OptionID == want {
					return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": o.OptionID}}, nil
				}
			}
		}
		if len(p.Options) > 0 {
			return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": p.Options[0].OptionID}}, nil
		}
	}
	for _, o := range p.Options {
		if strings.Contains(o.Kind, "reject") {
			return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": o.OptionID}}, nil
		}
	}
	return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}, nil
}

func (c *copilotController) handleExit(_ error) {
	c.buf.finishStreaming("")
	c.mu.Lock()
	c.state = "exited"
	c.activeTurnID = ""
	c.mu.Unlock()
	c.emit("session.updated", map[string]any{"sessionId": c.id})
}

func listCopilotModels(ctx context.Context, copilotPath, homeDir string) (*ao.AgentModels, error) {
	tmpDir, err := os.MkdirTemp("", "mb-copilot-models-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	ctrl, err := startCopilotSession(ctx, copilotPath, tmpDir, "", "", "", "", nil, func(string, map[string]any) {})
	if err != nil {
		return nil, err
	}
	sessionID := ctrl.ID()
	cm := ctrl.Models()
	_ = ctrl.Close()

	// Remove the throwaway probe session from ~/.copilot/session-state so it
	// never pollutes Import History.
	if sessionID != "" && homeDir != "" {
		_ = os.RemoveAll(filepath.Join(homeDir, ".copilot", "session-state", sessionID))
	}

	out := make([]ao.AgentModel, 0, len(cm.Models))
	for _, m := range cm.Models {
		out = append(out, ao.AgentModel{
			ID:            m.ID,
			Label:         m.DisplayName,
			IsDefault:     m.Default,
			Efforts:       m.Efforts,
			DefaultEffort: m.DefaultEffort,
		})
	}
	return &ao.AgentModels{AgentID: "copilot", Models: out}, nil
}
