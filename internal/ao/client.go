package ao

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// APIError is a non-2xx response from the AO daemon.
type APIError struct {
	Status    int    `json:"-"`
	Kind      string `json:"error"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"requestId"`
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("ao: %s (%s, HTTP %d)", e.Message, e.Code, e.Status)
	}
	return fmt.Sprintf("ao: %s (HTTP %d)", e.Message, e.Status)
}

// ErrUnavailable wraps transport failures: the daemon is not reachable.
var ErrUnavailable = errors.New("ao daemon unavailable")

// IsCode reports whether err is an APIError carrying code.
func IsCode(err error, code string) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Code == code
}

// defaultTimeout bounds calls whose context carries no deadline.
const defaultTimeout = 30 * time.Second

type Client struct {
	baseURL string
	http    *http.Client
	// stream serves the long-lived event stream.
	stream *http.Client
}

func New(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{},
		stream:  &http.Client{},
	}
}

func (c *Client) BaseURL() string { return c.baseURL }

func sessionPath(id string, suffix string) string {
	return "/api/v1/sessions/" + url.PathEscape(id) + suffix
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultTimeout)
		defer cancel()
	}
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return fmt.Errorf("%w: read response: %v", ErrUnavailable, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := &APIError{Status: resp.StatusCode}
		if json.Unmarshal(data, apiErr) != nil || apiErr.Message == "" {
			apiErr.Message = strings.TrimSpace(string(data))
			if apiErr.Message == "" {
				apiErr.Message = http.StatusText(resp.StatusCode)
			}
		}
		return apiErr
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("ao: decode %s %s: %w", method, path, err)
	}
	return nil
}

func (c *Client) ListSessions(ctx context.Context) ([]Session, error) {
	var out struct {
		Sessions []Session `json:"sessions"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/sessions", nil, &out); err != nil {
		return nil, err
	}
	return out.Sessions, nil
}

func (c *Client) ListProjects(ctx context.Context) ([]Project, error) {
	var out struct {
		Projects []Project `json:"projects"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/projects", nil, &out); err != nil {
		return nil, err
	}
	return out.Projects, nil
}

func (c *Client) Spawn(ctx context.Context, req SpawnRequest) (*Session, error) {
	var out struct {
		Session Session `json:"session"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/v1/sessions", req, &out); err != nil {
		return nil, err
	}
	if out.Session.ID == "" {
		return nil, errors.New("ao: spawn response did not include a session")
	}
	return &out.Session, nil
}

func (c *Client) Conversation(ctx context.Context, sessionID string, q ConversationQuery) (*Conversation, error) {
	params := url.Values{}
	if q.Limit > 0 {
		params.Set("limit", strconv.Itoa(q.Limit))
	}
	if q.BeforeSequence > 0 {
		params.Set("beforeSequence", strconv.FormatInt(q.BeforeSequence, 10))
	}
	if q.AfterSequence > 0 {
		params.Set("afterSequence", strconv.FormatInt(q.AfterSequence, 10))
	}
	path := sessionPath(sessionID, "/conversation")
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	var out Conversation
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	if out.Messages == nil {
		out.Messages = []Message{}
	}
	if out.Activities == nil {
		out.Activities = []ConversationActivity{}
	}
	return &out, nil
}

func (c *Client) SendMessage(ctx context.Context, sessionID, text, clientMessageID string) (*SendResult, error) {
	body := map[string]string{"text": text}
	if clientMessageID != "" {
		body["clientMessageId"] = clientMessageID
	}
	var out SendResult
	if err := c.do(ctx, http.MethodPost, sessionPath(sessionID, "/conversation/messages"), body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Interrupt(ctx context.Context, sessionID string) error {
	return c.do(ctx, http.MethodPost, sessionPath(sessionID, "/conversation/interrupt"), nil, nil)
}

// Resume restarts a lazily imported or exited agent. AO answers 409 with code
// SESSION_OPEN_ELSEWHERE when the native session is open in another terminal.
func (c *Client) Resume(ctx context.Context, sessionID string) (*Session, error) {
	var out struct {
		Session Session `json:"session"`
	}
	if err := c.do(ctx, http.MethodPost, sessionPath(sessionID, "/resume-agent"), nil, &out); err != nil {
		return nil, err
	}
	return &out.Session, nil
}

func (c *Client) Kill(ctx context.Context, sessionID string) error {
	return c.do(ctx, http.MethodPost, sessionPath(sessionID, "/kill"), struct{}{}, nil)
}

func (c *Client) Rename(ctx context.Context, sessionID, displayName string) error {
	return c.do(ctx, http.MethodPatch, sessionPath(sessionID, ""), map[string]string{"displayName": displayName}, nil)
}

// AgentModels returns the models an agent ("copilot" or "codex") offers.
func (c *Client) AgentModels(ctx context.Context, agent string) (*AgentModels, error) {
	var out AgentModels
	if err := c.do(ctx, http.MethodGet, "/api/v1/agents/"+url.PathEscape(agent)+"/models", nil, &out); err != nil {
		return nil, err
	}
	if out.Models == nil {
		out.Models = []AgentModel{}
	}
	return &out, nil
}

// ConversationModels returns the models a session can switch to and the
// settings currently selected for its next turn.
func (c *Client) ConversationModels(ctx context.Context, sessionID string) (*ConversationModels, error) {
	var out ConversationModels
	if err := c.do(ctx, http.MethodGet, sessionPath(sessionID, "/conversation/models"), nil, &out); err != nil {
		return nil, err
	}
	if out.Models == nil {
		out.Models = []ConversationModel{}
	}
	return &out, nil
}

// UpdateConversationSettings changes the model, reasoning effort or
// approval mode a session uses from its next turn on, and returns the
// settings AO reports afterwards. AO does not check that the model exists;
// callers validate it against AgentModels or ConversationModels first.
func (c *Client) UpdateConversationSettings(ctx context.Context, sessionID string, settings ConversationTurnSettings) (*ConversationSettings, error) {
	var out ConversationSettings
	if err := c.do(ctx, http.MethodPatch, sessionPath(sessionID, "/conversation/settings"), settings, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ImportCandidates(ctx context.Context) ([]ImportCandidate, error) {
	var out struct {
		Candidates []ImportCandidate `json:"candidates"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/session-import", nil, &out); err != nil {
		return nil, err
	}
	if out.Candidates == nil {
		out.Candidates = []ImportCandidate{}
	}
	return out.Candidates, nil
}

func (c *Client) Import(ctx context.Context, ids []string) ([]ImportResult, error) {
	var out struct {
		Results []ImportResult `json:"results"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/v1/session-import", map[string][]string{"ids": ids}, &out); err != nil {
		return nil, err
	}
	return out.Results, nil
}

// Events consumes AO's server-sent event stream until ctx is cancelled or the
// stream ends, calling fn for every event. lastEventID resumes after an
// earlier event when non-empty.
func (c *Client) Events(ctx context.Context, lastEventID string, fn func(Event)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v1/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := c.stream.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &APIError{Status: resp.StatusCode, Message: "event stream: " + resp.Status}
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 8<<20)
	var ev Event
	var data []string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if ev.ID != "" || ev.Type != "" || len(data) > 0 {
				ev.Data = strings.Join(data, "\n")
				fn(ev)
			}
			ev, data = Event{}, nil
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "id":
			ev.ID = value
		case "event":
			ev.Type = value
		case "data":
			data = append(data, value)
		}
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return ctx.Err()
}
