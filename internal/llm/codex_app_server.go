package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	codexRuntimeExec      = "exec"
	codexRuntimeAppServer = "app_server"

	// codexAppServerInitTimeout bounds process startup + initialize handshake
	// so a wedged app-server cannot block callers indefinitely.
	codexAppServerInitTimeout = 30 * time.Second

	// codexAppServerInterruptTimeout bounds the best-effort turn interrupt
	// sent when a caller cancels an in-flight completion.
	codexAppServerInterruptTimeout = 5 * time.Second
)

type codexAppServerCompletion struct {
	Model        string
	RepoDir      string
	Prompt       string
	OutputSchema []byte
}

type codexAppServerClient struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stderr  *lockedBuffer
	writeMu sync.Mutex

	// activeSlot serializes turns (capacity 1). A channel instead of a mutex
	// so that waiters can also observe context cancellation and process exit.
	activeSlot chan struct{}

	mu            sync.Mutex
	nextID        int64
	pending       map[int64]chan codexAppServerResponse
	notifications chan map[string]any
	// completions carries turn/completed events on a dedicated channel so the
	// completion signal can never be displaced by overflow in the general
	// notification buffer. Turns are serialized, so a small capacity suffices.
	completions chan map[string]any

	done    chan struct{} // closed when readLoop exits (process died or closed stdout)
	readErr error         // why readLoop exited; set before done is closed
}

// lockedBuffer is a goroutine-safe bytes.Buffer: the exec stderr copier
// writes concurrently with error paths that read the captured output.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type codexAppServerResponse struct {
	Result map[string]any `json:"result,omitempty"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func startCodexAppServer(ctx context.Context) (*codexAppServerClient, error) {
	cmd := exec.Command("codex", "app-server")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open codex app-server stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open codex app-server stdout: %w", err)
	}
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start codex app-server: %w", err)
	}

	c := &codexAppServerClient{
		cmd:           cmd,
		stdin:         stdin,
		stderr:        stderr,
		activeSlot:    make(chan struct{}, 1),
		pending:       make(map[int64]chan codexAppServerResponse),
		notifications: make(chan map[string]any, 128),
		completions:   make(chan map[string]any, 8),
		done:          make(chan struct{}),
	}
	go c.readLoop(stdout)

	// Bound the handshake so a started-but-unresponsive app-server fails fast
	// instead of hanging before the caller's request timeout applies.
	initCtx, cancel := context.WithTimeout(ctx, codexAppServerInitTimeout)
	defer cancel()
	if err := c.initialize(initCtx); err != nil {
		// Killing the process closes stdout; readLoop then exits and reaps it
		// via cmd.Wait, so no zombie is left behind on this path either.
		_ = cmd.Process.Kill()
		<-c.done
		return nil, err
	}
	return c, nil
}

func (c *codexAppServerClient) initialize(ctx context.Context) error {
	_, err := c.request(ctx, "initialize", map[string]any{
		"clientInfo": map[string]string{
			"name":    "open_code_review",
			"title":   "OpenCodeReview",
			"version": AppVersion,
		},
		"capabilities": map[string]any{
			"experimentalApi": true,
		},
	})
	if err != nil {
		return fmt.Errorf("initialize codex app-server: %w", err)
	}
	return c.notify("initialized", map[string]any{})
}

func (c *codexAppServerClient) Complete(ctx context.Context, req codexAppServerCompletion) (string, error) {
	// Acquire the single turn slot without outliving the caller's deadline:
	// a waiter whose context fires must not keep blocking behind a slow turn.
	select {
	case c.activeSlot <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	case <-c.done:
		return "", c.exitError()
	}
	defer func() { <-c.activeSlot }()

	// Discard notifications left over from earlier canceled/timed-out turns so
	// they cannot contaminate this completion.
	c.drainNotifications()

	threadResp, err := c.request(ctx, "thread/start", codexAppServerThreadStartParams(req.Model, req.RepoDir))
	if err != nil {
		return "", fmt.Errorf("codex app-server thread/start: %w", err)
	}
	threadID, err := codexThreadID(threadResp)
	if err != nil {
		return "", err
	}

	acc := newCodexAppServerTurnAccumulator(threadID)
	turnParams, err := codexAppServerTurnStartParams(threadID, req.Model, req.RepoDir, req.Prompt, req.OutputSchema)
	if err != nil {
		return "", err
	}
	turnResp, err := c.request(ctx, "turn/start", turnParams)
	if err != nil {
		// The server may have accepted the turn even though the response never
		// reached us (e.g. cancellation in flight); stop it so it cannot keep
		// running and emitting notifications in the background. The turn id is
		// unknown on this path, so the interrupt carries only the thread id.
		if ctx.Err() != nil {
			c.interruptTurn(threadID, "")
		}
		return "", fmt.Errorf("codex app-server turn/start: %w", err)
	}
	turnID := codexTurnID(turnResp)

	for {
		var msg map[string]any
		select {
		case <-ctx.Done():
			c.interruptTurn(threadID, turnID)
			return "", ctx.Err()
		case <-c.done:
			return "", c.exitError()
		case msg = <-c.notifications:
		case msg = <-c.completions:
		}
		acc.HandleNotification(msg)
		if acc.Completed() {
			// Item events are delivered before turn/completed, but the two
			// channels are independent; drain remaining items so a final
			// answer still in the general buffer is not missed.
			c.consumePendingItems(acc)
			text := strings.TrimSpace(acc.FinalText())
			if text == "" {
				return "", fmt.Errorf("codex app-server turn completed without final assistant message")
			}
			return text, nil
		}
	}
}

// consumePendingItems applies notifications already buffered at completion
// time, without blocking for new ones.
func (c *codexAppServerClient) consumePendingItems(acc *codexAppServerTurnAccumulator) {
	for {
		select {
		case msg := <-c.notifications:
			acc.HandleNotification(msg)
		default:
			return
		}
	}
}

// interruptTurn asks the app-server to stop the active turn so it does not
// keep running (and emitting notifications) after the caller gave up.
// Best-effort: it runs detached from the caller's already-canceled context.
// The turn id (observed in turn/start responses as result.turn.id) is included
// when known, since interrupt handling may require both identifiers.
func (c *codexAppServerClient) interruptTurn(threadID, turnID string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), codexAppServerInterruptTimeout)
		defer cancel()
		params := map[string]any{"threadId": threadID}
		if turnID != "" {
			params["turnId"] = turnID
		}
		_, _ = c.request(ctx, "turn/interrupt", params)
	}()
}

// codexTurnID extracts result.turn.id from a turn/start response.
func codexTurnID(resp map[string]any) string {
	turn, _ := resp["turn"].(map[string]any)
	id, _ := turn["id"].(string)
	return id
}

// drainNotifications empties both notification channels without blocking.
func (c *codexAppServerClient) drainNotifications() {
	for {
		select {
		case <-c.notifications:
		case <-c.completions:
		default:
			return
		}
	}
}

// Closed reports whether the app-server process is no longer usable.
func (c *codexAppServerClient) Closed() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// exitError describes why the app-server stopped, including captured stderr.
func (c *codexAppServerClient) exitError() error {
	c.mu.Lock()
	err := c.readErr
	c.mu.Unlock()
	if err == nil {
		err = fmt.Errorf("codex app-server stopped")
	}
	if msg := strings.TrimSpace(c.stderr.String()); msg != "" {
		return fmt.Errorf("%w: %s", err, msg)
	}
	return err
}

func (c *codexAppServerClient) request(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	id := c.nextRequestID()
	ch := make(chan codexAppServerResponse, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	if err := c.write(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, c.exitError()
	case resp := <-ch:
		if resp.Error != nil {
			return nil, fmt.Errorf("%s: %s", method, resp.Error.Message)
		}
		return resp.Result, nil
	}
}

func (c *codexAppServerClient) notify(method string, params map[string]any) error {
	return c.write(map[string]any{"method": method, "params": params})
}

// rejectServerRequest answers an unsupported server-initiated JSON-RPC request
// with a standard method-not-found error, preserving the original id value.
func (c *codexAppServerClient) rejectServerRequest(id any, method string) {
	_ = c.write(map[string]any{
		"id": id,
		"error": map[string]any{
			"code":    -32601,
			"message": fmt.Sprintf("method %q is not supported by this client", method),
		},
	})
}

func (c *codexAppServerClient) write(msg map[string]any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.stdin.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write codex app-server message: %w", err)
	}
	return nil
}

func (c *codexAppServerClient) readLoop(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var msg map[string]any
		dec := json.NewDecoder(strings.NewReader(scanner.Text()))
		dec.UseNumber()
		if err := dec.Decode(&msg); err != nil {
			continue
		}
		if rawID, hasID := msg["id"]; hasID && rawID != nil {
			// A message carrying both id and method is a server-initiated
			// request (e.g. an approval prompt), not a response. We run with
			// approvalPolicy=never and support no server->client methods, so
			// answer immediately instead of leaving the server blocked on us.
			// Routing keys off the raw id: JSON-RPC permits string ids, which
			// must still receive a rejection even though our own outgoing
			// request ids are always numeric.
			if method, _ := msg["method"].(string); method != "" {
				go c.rejectServerRequest(rawID, method)
				continue
			}
			if id, ok := jsonRPCID(rawID); ok {
				var resp codexAppServerResponse
				data, _ := json.Marshal(msg)
				_ = json.Unmarshal(data, &resp)
				c.mu.Lock()
				ch := c.pending[id]
				c.mu.Unlock()
				if ch != nil {
					ch <- resp
				}
			}
			// A response with an id we never issued is not ours to handle.
			continue
		}
		c.publishNotification(msg)
	}

	// The app-server closed stdout (process exit or pipe failure). Reap the
	// process exactly once so it cannot linger as a zombie, then record why
	// and signal everyone blocked on responses or notifications.
	_ = c.stdin.Close()
	waitErr := c.cmd.Wait()

	c.mu.Lock()
	if err := scanner.Err(); err != nil {
		c.readErr = fmt.Errorf("read codex app-server output: %w", err)
	} else if waitErr != nil {
		c.readErr = fmt.Errorf("codex app-server exited: %w", waitErr)
	} else {
		c.readErr = fmt.Errorf("codex app-server closed its output stream")
	}
	c.mu.Unlock()
	close(c.done)
}

// publishNotification enqueues a protocol notification. turn/completed goes
// to the dedicated completions channel, so the completion signal can never be
// displaced; for the rest, the oldest entry is dropped on overflow instead of
// the newest, because late events (such as the final answer) matter most.
func (c *codexAppServerClient) publishNotification(msg map[string]any) {
	if method, _ := msg["method"].(string); method == "turn/completed" {
		select {
		case c.completions <- msg:
		case <-c.done:
		}
		return
	}
	for {
		select {
		case c.notifications <- msg:
			return
		default:
		}
		select {
		case <-c.notifications:
		default:
		}
	}
}

func (c *codexAppServerClient) nextRequestID() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	return c.nextID
}

func (c *CodexClient) appServerThreadStartParams(model string) map[string]any {
	return codexAppServerThreadStartParams(c.modelFor(model), c.repoDir())
}

func codexAppServerThreadStartParams(model, repoDir string) map[string]any {
	params := map[string]any{
		"ephemeral":      true,
		"sandbox":        "read-only",
		"approvalPolicy": "never",
		"environments":   []any{},
	}
	if model != "" {
		params["model"] = model
	}
	if repoDir != "" {
		params["cwd"] = repoDir
		params["runtimeWorkspaceRoots"] = []string{repoDir}
	}
	return params
}

func codexAppServerTurnStartParams(threadID, model, repoDir, prompt string, outputSchema []byte) (map[string]any, error) {
	params := map[string]any{
		"threadId":       threadID,
		"input":          []map[string]string{{"type": "text", "text": prompt}},
		"approvalPolicy": "never",
		"environments":   []any{},
		"sandboxPolicy": map[string]any{
			"type":          "readOnly",
			"networkAccess": false,
		},
	}
	if model != "" {
		params["model"] = model
	}
	if repoDir != "" {
		params["cwd"] = repoDir
		params["runtimeWorkspaceRoots"] = []string{repoDir}
	}
	if len(outputSchema) > 0 {
		var schema map[string]any
		if err := json.Unmarshal(outputSchema, &schema); err != nil {
			// Dropping the schema silently would yield unconstrained text that
			// only fails later during parsing, far from the actual cause.
			return nil, fmt.Errorf("codex app-server output schema is not valid JSON: %w", err)
		}
		params["outputSchema"] = schema
	}
	return params, nil
}

func codexThreadID(resp map[string]any) (string, error) {
	thread, _ := resp["thread"].(map[string]any)
	id, _ := thread["id"].(string)
	if id == "" {
		return "", fmt.Errorf("codex app-server thread/start response missing thread.id")
	}
	return id, nil
}

func jsonRPCID(v any) (int64, bool) {
	switch id := v.(type) {
	case json.Number:
		n, err := id.Int64()
		return n, err == nil
	case float64:
		return int64(id), true
	case int64:
		return id, true
	case int:
		return int64(id), true
	case string:
		n, err := strconv.ParseInt(id, 10, 64)
		return n, err == nil
	default:
		return 0, false
	}
}

type codexAppServerTurnAccumulator struct {
	threadID  string
	finalText string
	lastText  string
	done      bool
}

func newCodexAppServerTurnAccumulator(threadID string) *codexAppServerTurnAccumulator {
	return &codexAppServerTurnAccumulator{threadID: threadID}
}

func (a *codexAppServerTurnAccumulator) HandleNotification(msg map[string]any) {
	method, _ := msg["method"].(string)
	params, _ := msg["params"].(map[string]any)
	id := codexNotificationThreadID(params)
	switch method {
	case "item/completed":
		// Items require positive thread correlation, like turn/completed:
		// live protocol traces (codex-cli 0.134.0) show item events always
		// carry threadId at the top level, and accepting anonymous text would
		// let stragglers from a canceled turn (arriving after the pre-turn
		// drain) be returned as this turn's answer.
		if a.threadID != "" && id != a.threadID {
			return
		}
		item, _ := params["item"].(map[string]any)
		if item["type"] != "agentMessage" {
			return
		}
		text, _ := item["text"].(string)
		if text == "" {
			return
		}
		a.lastText = text
		if phase, _ := item["phase"].(string); phase == "final_answer" {
			a.finalText = text
		}
	case "turn/completed":
		// The completion signal requires positive correlation: an anonymous or
		// stale turn/completed (e.g. from an interrupted previous turn) must
		// never finish this turn with possibly stale text.
		if a.threadID != "" && id != a.threadID {
			return
		}
		a.done = true
	}
}

func codexNotificationThreadID(params map[string]any) string {
	if params == nil {
		return ""
	}
	for _, key := range []string{"threadId", "thread_id"} {
		if id, _ := params[key].(string); id != "" {
			return id
		}
	}
	if thread, _ := params["thread"].(map[string]any); thread != nil {
		if id, _ := thread["id"].(string); id != "" {
			return id
		}
	}
	if item, _ := params["item"].(map[string]any); item != nil {
		for _, key := range []string{"threadId", "thread_id"} {
			if id, _ := item[key].(string); id != "" {
				return id
			}
		}
	}
	return ""
}

func (a *codexAppServerTurnAccumulator) Completed() bool {
	return a.done
}

func (a *codexAppServerTurnAccumulator) FinalText() string {
	if a.finalText != "" {
		return a.finalText
	}
	return a.lastText
}
