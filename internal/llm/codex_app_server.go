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
)

const (
	codexRuntimeExec      = "exec"
	codexRuntimeAppServer = "app_server"
)

type codexAppServerCompletion struct {
	Model        string
	RepoDir      string
	Prompt       string
	OutputSchema []byte
}

type codexAppServerClient struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stderr   *bytes.Buffer
	writeMu  sync.Mutex
	activeMu sync.Mutex

	mu            sync.Mutex
	nextID        int64
	pending       map[int64]chan codexAppServerResponse
	notifications chan map[string]any
}

type codexAppServerResponse struct {
	Result map[string]any `json:"result,omitempty"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func startCodexAppServer() (*codexAppServerClient, error) {
	cmd := exec.Command("codex", "app-server")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open codex app-server stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open codex app-server stdout: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start codex app-server: %w", err)
	}

	c := &codexAppServerClient{
		cmd:           cmd,
		stdin:         stdin,
		stderr:        &stderr,
		pending:       make(map[int64]chan codexAppServerResponse),
		notifications: make(chan map[string]any, 128),
	}
	go c.readLoop(stdout)

	if err := c.initialize(context.Background()); err != nil {
		_ = cmd.Process.Kill()
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
	c.activeMu.Lock()
	defer c.activeMu.Unlock()

	threadResp, err := c.request(ctx, "thread/start", codexAppServerThreadStartParams(req.Model, req.RepoDir))
	if err != nil {
		return "", fmt.Errorf("codex app-server thread/start: %w", err)
	}
	threadID, err := codexThreadID(threadResp)
	if err != nil {
		return "", err
	}

	acc := newCodexAppServerTurnAccumulator()
	turnParams := codexAppServerTurnStartParams(threadID, req.Model, req.RepoDir, req.Prompt, req.OutputSchema)
	if _, err := c.request(ctx, "turn/start", turnParams); err != nil {
		return "", fmt.Errorf("codex app-server turn/start: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case msg := <-c.notifications:
			acc.HandleNotification(msg)
			if acc.Completed() {
				text := strings.TrimSpace(acc.FinalText())
				if text == "" {
					return "", fmt.Errorf("codex app-server turn completed without final assistant message")
				}
				return text, nil
			}
		}
	}
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
		if id, ok := jsonRPCID(msg["id"]); ok {
			var resp codexAppServerResponse
			data, _ := json.Marshal(msg)
			_ = json.Unmarshal(data, &resp)
			c.mu.Lock()
			ch := c.pending[id]
			c.mu.Unlock()
			if ch != nil {
				ch <- resp
			}
			continue
		}
		select {
		case c.notifications <- msg:
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

func codexAppServerTurnStartParams(threadID, model, repoDir, prompt string, outputSchema []byte) map[string]any {
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
		if err := json.Unmarshal(outputSchema, &schema); err == nil {
			params["outputSchema"] = schema
		}
	}
	return params
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
	finalText string
	lastText  string
	done      bool
}

func newCodexAppServerTurnAccumulator() *codexAppServerTurnAccumulator {
	return &codexAppServerTurnAccumulator{}
}

func (a *codexAppServerTurnAccumulator) HandleNotification(msg map[string]any) {
	method, _ := msg["method"].(string)
	params, _ := msg["params"].(map[string]any)
	switch method {
	case "item/completed":
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
		a.done = true
	}
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
