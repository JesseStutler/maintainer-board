package native

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
)

// rpcError represents a JSON-RPC 2.0 error object returned by an agent server.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

type rpcEnvelope struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcPending struct {
	result json.RawMessage
	err    error
	done   chan struct{}
}

// rpcConn manages one newline-delimited JSON-RPC 2.0 subprocess over stdio.
type rpcConn struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	writeM sync.Mutex

	nextID atomic.Int64

	mu      sync.Mutex
	pending map[string]*rpcPending
	closed  bool
	closeCh chan struct{}
	exitErr error

	onNotify  func(method string, params json.RawMessage)
	onRequest func(id json.RawMessage, method string, params json.RawMessage) (any, error)
	onExit    func(err error)
}

func startRPC(
	cwd string,
	bin string,
	args []string,
	onNotify func(method string, params json.RawMessage),
	onRequest func(id json.RawMessage, method string, params json.RawMessage) (any, error),
	onExit func(err error),
) (*rpcConn, error) {
	cmd := exec.Command(bin, args...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", bin, err)
	}
	c := &rpcConn{
		cmd:       cmd,
		stdin:     stdin,
		pending:   map[string]*rpcPending{},
		closeCh:   make(chan struct{}),
		onNotify:  onNotify,
		onRequest: onRequest,
		onExit:    onExit,
	}
	// Drain stderr so the child never blocks on a full pipe.
	go func() { _, _ = io.Copy(io.Discard, stderr) }()
	go c.readLoop(stdout)
	return c, nil
}

func (c *rpcConn) readLoop(r io.Reader) {
	br := bufio.NewReaderSize(r, 256*1024)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := strings.TrimSpace(string(line))
			if trimmed != "" && strings.HasPrefix(trimmed, "{") {
				c.handleLine([]byte(trimmed))
			}
		}
		if err != nil {
			break
		}
	}
	waitErr := c.cmd.Wait()
	c.mu.Lock()
	c.closed = true
	c.exitErr = waitErr
	for k, p := range c.pending {
		p.err = fmt.Errorf("agent process exited: %v", waitErr)
		close(p.done)
		delete(c.pending, k)
	}
	close(c.closeCh)
	c.mu.Unlock()
	if c.onExit != nil {
		c.onExit(waitErr)
	}
}

func (c *rpcConn) handleLine(line []byte) {
	var env rpcEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		return
	}
	hasID := len(env.ID) > 0 && string(env.ID) != "null"
	if env.Method != "" && hasID {
		// Server-initiated request: handle asynchronously so readLoop never blocks.
		go func() {
			var res any
			var err error
			if c.onRequest != nil {
				res, err = c.onRequest(env.ID, env.Method, env.Params)
			} else {
				err = errors.New("unsupported request")
			}
			c.sendResponse(env.ID, res, err)
		}()
		return
	}
	if env.Method != "" {
		if c.onNotify != nil {
			c.onNotify(env.Method, env.Params)
		}
		return
	}
	if hasID {
		key := string(env.ID)
		c.mu.Lock()
		p := c.pending[key]
		delete(c.pending, key)
		c.mu.Unlock()
		if p != nil {
			if env.Error != nil {
				p.err = env.Error
			} else {
				p.result = env.Result
			}
			close(p.done)
		}
	}
}

func (c *rpcConn) sendJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	c.writeM.Lock()
	defer c.writeM.Unlock()
	_, err = c.stdin.Write(b)
	return err
}

func (c *rpcConn) sendResponse(id json.RawMessage, result any, callErr error) {
	msg := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
	}
	if callErr != nil {
		msg["error"] = map[string]any{"code": -32000, "message": callErr.Error()}
	} else {
		if result == nil {
			result = map[string]any{}
		}
		msg["result"] = result
	}
	_ = c.sendJSON(msg)
}

func (c *rpcConn) Call(ctx context.Context, method string, params any, out any) error {
	id := c.nextID.Add(1)
	key := fmt.Sprintf("%d", id)
	p := &rpcPending{done: make(chan struct{})}

	c.mu.Lock()
	if c.closed {
		err := c.exitErr
		c.mu.Unlock()
		return fmt.Errorf("agent process already exited: %v", err)
	}
	c.pending[key] = p
	c.mu.Unlock()

	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
	}
	if params != nil {
		req["params"] = params
	}
	if err := c.sendJSON(req); err != nil {
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
		return err
	}

	select {
	case <-p.done:
		if p.err != nil {
			return p.err
		}
		if out != nil && len(p.result) > 0 {
			return json.Unmarshal(p.result, out)
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
		return ctx.Err()
	}
}

func (c *rpcConn) Notify(method string, params any) error {
	msg := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
	}
	if params != nil {
		msg["params"] = params
	}
	return c.sendJSON(msg)
}

func (c *rpcConn) Alive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.closed
}

func (c *rpcConn) Close() error {
	c.writeM.Lock()
	_ = c.stdin.Close()
	c.writeM.Unlock()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	<-c.closeCh
	return nil
}
