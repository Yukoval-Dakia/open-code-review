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

	if err := json.NewEncoder(c.stdin).Encode(claudeStreamJSONUserMessage(prompt)); err != nil {
		c.Close()
		return "", fmt.Errorf("write claude stream-json prompt: %w", err)
	}
	// Claude Code's stream-json input is JSONL over stdin. Closing stdin after
	// the user message marks the end of this print-mode turn; otherwise the
	// process can keep waiting for more input and never emit the final result.
	if c.stdin != nil {
		if err := c.stdin.Close(); err != nil {
			c.Close()
			return "", fmt.Errorf("close claude stream-json stdin: %w", err)
		}
		c.stdin = nil
	}

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
		_ = c.stdin.Close()
		c.stdin = nil
	}
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
}

func (c *claudeAppServerClient) exitError() error {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
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
	c.markClosed(fmt.Errorf("claude stream-json closed its output stream"))
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

func claudeStreamJSONUserMessage(prompt string) map[string]any {
	return map[string]any{
		"type": "user",
		"message": map[string]any{
			"role": "user",
			"content": []map[string]string{
				{
					"type": "text",
					"text": prompt,
				},
			},
		},
	}
}
