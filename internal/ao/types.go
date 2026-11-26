// Package ao is a typed HTTP client for the local agent-orchestrator daemon.
package ao

import "encoding/json"

type Activity struct {
	State          string `json:"state"`
	LastActivityAt string `json:"lastActivityAt"`
}

type BranchState struct {
	Branch    string `json:"branch"`
	Base      string `json:"base,omitempty"`
	Commits   int    `json:"commits"`
	Unpushed  int    `json:"unpushed"`
	Behind    int    `json:"behind"`
	Head      string `json:"head,omitempty"`
	Conflicts bool   `json:"conflicts"`
}

// SessionPR mirrors the PR facts AO attaches to a session.
type SessionPR struct {
	URL       string `json:"url"`
	Number    int    `json:"number"`
	State     string `json:"state"`
	CI        string `json:"ci"`
	Review    string `json:"review"`
	UpdatedAt string `json:"updatedAt"`
}

type Session struct {
	ID            string       `json:"id"`
	ProjectID     string       `json:"projectId"`
	Kind          string       `json:"kind"`
	Harness       string       `json:"harness"`
	DisplayName   string       `json:"displayName"`
	Mode          string       `json:"mode"`
	Activity      Activity     `json:"activity"`
	IsTerminated  bool         `json:"isTerminated"`
	NeedsResume   bool         `json:"needsResume"`
	Status        string       `json:"status"`
	KanbanColumn  string       `json:"kanbanColumn"`
	DisplayStatus string       `json:"displayStatus"`
	Branch        string       `json:"branch"`
	BranchState   *BranchState `json:"branchState,omitempty"`
	CreatedAt     string       `json:"createdAt"`
	UpdatedAt     string       `json:"updatedAt"`
	PRs           []SessionPR  `json:"prs"`
}

type Project struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Path          string `json:"path"`
	Kind          string `json:"kind"`
	SessionPrefix string `json:"sessionPrefix"`
}

// SpawnRequest is the subset of AO's SpawnSessionRequest used by the board.
type SpawnRequest struct {
	ProjectID           string `json:"projectId"`
	Harness             string `json:"harness"`
	Mode                string `json:"mode"`
	Kind                string `json:"kind"`
	DisplayName         string `json:"displayName,omitempty"`
	DisplayNameExplicit bool   `json:"displayNameExplicit,omitempty"`
	Prompt              string `json:"prompt,omitempty"`
	// Model, Effort and ApprovalMode choose the initial settings of the new
	// session; empty means AO's / the agent's default.
	Model        string `json:"model,omitempty"`
	Effort       string `json:"effort,omitempty"`
	ApprovalMode string `json:"approvalMode,omitempty"`
}

// AgentModel is one model an agent (copilot or codex) can run.
type AgentModel struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	IsDefault bool   `json:"isDefault"`
	// Efforts lists the reasoning efforts the model accepts; empty when
	// the model has no such setting.
	Efforts       []string `json:"efforts"`
	DefaultEffort string   `json:"defaultEffort"`
}

// AgentModels is the model catalog of one agent.
type AgentModels struct {
	AgentID string       `json:"agentId"`
	Models  []AgentModel `json:"models"`
}

// ConversationModel is one model a running session can switch to.
type ConversationModel struct {
	ID            string   `json:"id"`
	DisplayName   string   `json:"displayName"`
	Description   string   `json:"description"`
	Default       bool     `json:"default"`
	Efforts       []string `json:"efforts"`
	DefaultEffort string   `json:"defaultEffort"`
}

// ConversationSettings are the per-turn settings of a session as AO
// reports them. Empty fields mean the agent's default.
type ConversationSettings struct {
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoningEffort"`
	ApprovalMode    string `json:"approvalMode"`
}

// ConversationModels is the model picker data of a session. Models is
// empty for a session whose agent is not running, such as an imported
// session that was not resumed yet.
type ConversationModels struct {
	Models   []ConversationModel  `json:"models"`
	Selected ConversationSettings `json:"selected"`
}

// ConversationTurnSettings is a settings change for the next turn of a
// session. A nil field is left as it is; a pointer to "" resets the field
// to the agent's default.
type ConversationTurnSettings struct {
	Model           *string `json:"model,omitempty"`
	ReasoningEffort *string `json:"reasoningEffort,omitempty"`
	ApprovalMode    *string `json:"approvalMode,omitempty"`
}

type Message struct {
	Kind      string `json:"kind"`
	ID        string `json:"id"`
	Sequence  int64  `json:"sequence"`
	Revision  int64  `json:"revision"`
	Role      string `json:"role"`
	Origin    string `json:"origin,omitempty"`
	Text      string `json:"text"`
	Streaming bool   `json:"streaming"`
	TurnID    string `json:"turnId,omitempty"`
	// ClientMessageID echoes the idempotency key the sender supplied.
	ClientMessageID string `json:"clientMessageId,omitempty"`
	CreatedAt       string `json:"createdAt"`
}

type ConversationActivity struct {
	Kind         string          `json:"kind"`
	ID           string          `json:"id"`
	Sequence     int64           `json:"sequence"`
	Revision     int64           `json:"revision"`
	ActivityKind string          `json:"activityKind"`
	Status       string          `json:"status"`
	Summary      string          `json:"summary"`
	Detail       json.RawMessage `json:"detail,omitempty"`
	TurnID       string          `json:"turnId,omitempty"`
	CreatedAt    string          `json:"createdAt"`
}

type Conversation struct {
	SessionID      string                 `json:"sessionId"`
	Harness        string                 `json:"harness"`
	Mode           string                 `json:"mode"`
	Controller     string                 `json:"controller"`
	Title          string                 `json:"title,omitempty"`
	LatestSequence int64                  `json:"latestSequence"`
	OldestSequence int64                  `json:"oldestSequence"`
	HasMoreBefore  bool                   `json:"hasMoreBefore"`
	Messages       []Message              `json:"messages"`
	Activities     []ConversationActivity `json:"activities"`
}

type ConversationQuery struct {
	Limit          int
	BeforeSequence int64
	AfterSequence  int64
}

type SendResult struct {
	TurnID    string `json:"turnId"`
	State     string `json:"state"`
	Duplicate bool   `json:"duplicate"`
}

type ImportCandidate struct {
	ID           string `json:"id"`
	Harness      string `json:"harness"`
	Title        string `json:"title"`
	ProjectID    string `json:"projectId,omitempty"`
	Folder       string `json:"folder,omitempty"`
	LastActivity string `json:"lastActivity"`
	MessageCount int    `json:"messageCount"`
	// SessionID is set when the candidate was already imported into AO.
	SessionID string `json:"sessionId,omitempty"`
	Shortened bool   `json:"shortened,omitempty"`
}

type ImportResult struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	SessionID string `json:"sessionId,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Event is one message of AO's server-sent event stream.
type Event struct {
	ID   string
	Type string
	Data string
}
