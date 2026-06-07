package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
)

type claudeAppServerClient struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stderr  *lockedBuffer
	model   string
	repoDir string

	turnMu  sync.Mutex
	stateMu sync.Mutex
	pending chan claudeAppServerResult
	closed  bool
	readErr error
}

type claudeAppServerResult struct {
	Text    string
	IsError bool
	Error   string
}

func startClaudeAppServer(ctx context.Context, model, repoDir string) (*claudeAppServerClient, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	args := []string{"-p", "--output-format", "stream-json", "--input-format", "stream-json", "--verbose", "--max-turns", "1"}
	args = append(args, claudeProviderIsolationArgs()...)
	if model != "" {
		args = append(args, "--model", model)
	}
	cmd := exec.Command("claude", args...)
	if repoDir != "" {
		cmd.Dir = repoDir
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open claude stream-json stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open claude stream-json stdout: %w", err)
	}

	var stderr lockedBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start claude stream-json: %w", err)
	}

	c := &claudeAppServerClient{
		cmd:     cmd,
		stdin:   stdin,
		stderr:  &stderr,
		model:   model,
		repoDir: repoDir,
	}
	go c.readLoop(stdout)
	go c.waitLoop()
	return c, nil
}

func (c *claudeAppServerClient) Matches(model, repoDir string) bool {
	return c.model == model && c.repoDir == repoDir
}

func (c *claudeAppServerClient) Complete(ctx context.Context, prompt string) (string, error) {
	c.turnMu.Lock()
	defer c.turnMu.Unlock()

	if c.Closed() {
		return "", c.exitError()
	}

	ch := make(chan claudeAppServerResult, 1)
	c.stateMu.Lock()
	c.pending = ch
	c.stateMu.Unlock()
	defer func() {
		c.stateMu.Lock()
		if c.pending == ch {
			c.pending = nil
		}
		c.stateMu.Unlock()
	}()

	c.stateMu.Lock()
	stdin := c.stdin
	if c.closed || stdin == nil {
		err := c.exitErrorLocked()
		c.stateMu.Unlock()
		return "", err
	}
	if err := json.NewEncoder(stdin).Encode(claudeStreamJSONUserMessage(prompt)); err != nil {
		c.stateMu.Unlock()
		c.Close()
		return "", fmt.Errorf("write claude stream-json prompt: %w", err)
	}
	// Claude Code's stream-json input is JSONL over stdin. Closing stdin after
	// the user message marks the end of this print-mode turn; otherwise the
	// process can keep waiting for more input and never emit the final result.
	if err := stdin.Close(); err != nil {
		c.stdin = nil
		c.stateMu.Unlock()
		c.Close()
		return "", fmt.Errorf("close claude stream-json stdin: %w", err)
	}
	c.stdin = nil
	c.stateMu.Unlock()

	select {
	case result := <-ch:
		if result.IsError {
			msg := strings.TrimSpace(result.Error)
			if msg == "" {
				msg = strings.TrimSpace(result.Text)
			}
			if msg == "" {
				msg = c.exitError().Error()
			}
			return "", fmt.Errorf("claude stream-json failed: %s", msg)
		}
		return result.Text, nil
	case <-ctx.Done():
		c.Close()
		return "", ctx.Err()
	}
}

func (c *claudeAppServerClient) Closed() bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.closed
}

func (c *claudeAppServerClient) Close() {
	c.stateMu.Lock()
	if c.closed {
		c.stateMu.Unlock()
		return
	}
	c.closed = true
	c.stateMu.Unlock()

	if c.stdin != nil {
		stdin := c.stdin
		c.stdin = nil
		_ = stdin.Close()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
}

func (c *claudeAppServerClient) exitError() error {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.exitErrorLocked()
}

func (c *claudeAppServerClient) exitErrorLocked() error {
	if c.readErr != nil {
		return c.readErr
	}
	msg := strings.TrimSpace(c.stderr.String())
	if msg != "" {
		return fmt.Errorf("claude stream-json stopped: %s", msg)
	}
	return fmt.Errorf("claude stream-json stopped")
}

func (c *claudeAppServerClient) readLoop(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		if typ, _ := msg["type"].(string); typ != "result" {
			continue
		}
		c.deliver(claudeAppServerResultFromMessage(msg))
	}
	if err := scanner.Err(); err != nil {
		c.markClosed(fmt.Errorf("read claude stream-json output: %w", err))
		return
	}
	// stdout EOF is expected when the print-mode process exits. Let waitLoop
	// record the actual process exit status instead of masking it with a less
	// useful "closed output stream" error.
}

func (c *claudeAppServerClient) waitLoop() {
	if err := c.cmd.Wait(); err != nil {
		c.markClosed(fmt.Errorf("claude stream-json exited: %w", err))
		return
	}
	c.markClosed(fmt.Errorf("claude stream-json exited"))
}

func (c *claudeAppServerClient) deliver(result claudeAppServerResult) {
	c.stateMu.Lock()
	ch := c.pending
	c.stateMu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- result:
	default:
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- result:
		default:
		}
	}
}

func (c *claudeAppServerClient) markClosed(err error) {
	c.stateMu.Lock()
	if c.closed {
		c.stateMu.Unlock()
		return
	}
	c.closed = true
	c.readErr = err
	ch := c.pending
	c.stateMu.Unlock()

	if ch != nil {
		select {
		case ch <- claudeAppServerResult{IsError: true, Error: err.Error()}:
		default:
		}
	}
}

func claudeAppServerResultFromMessage(msg map[string]any) claudeAppServerResult {
	result := claudeAppServerResult{}
	if text, ok := msg["result"].(string); ok {
		result.Text = text
	}
	if isError, ok := msg["is_error"].(bool); ok {
		result.IsError = isError
	}
	if errText, ok := msg["error"].(string); ok {
		result.Error = errText
	}
	if subtype, ok := msg["subtype"].(string); ok && subtype == "error" {
		result.IsError = true
	}
	return result
}

func claudeStreamJSONUserMessage(prompt string) claudeStreamJSONInputMessage {
	return claudeStreamJSONInputMessage{
		Type: "user",
		Message: claudeStreamJSONMessage{
			Role: "user",
			Content: []claudeStreamJSONContent{
				{
					Type: "text",
					Text: prompt,
				},
			},
		},
	}
}

type claudeStreamJSONInputMessage struct {
	Type    string                  `json:"type"`
	Message claudeStreamJSONMessage `json:"message"`
}

type claudeStreamJSONMessage struct {
	Role    string                    `json:"role"`
	Content []claudeStreamJSONContent `json:"content"`
}

type claudeStreamJSONContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
