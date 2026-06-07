package llm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	claudeRuntimeExec      = "exec"
	claudeRuntimeAppServer = "app_server"
)

// ClaudeClient adapts the official Claude Code CLI into OCR's LLMClient
// interface. It uses Claude Code's own local authentication instead of
// extracting or converting subscription tokens.
type ClaudeClient struct {
	cfg ClientConfig

	appServerMu sync.Mutex
	appServers  map[string]*claudeAppServerClient
}

func NewClaudeClient(cfg ClientConfig) *ClaudeClient {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Minute
	}
	return &ClaudeClient{cfg: cfg}
}

func (c *ClaudeClient) Completions(req ChatRequest) (*ChatResponse, error) {
	return c.CompletionsWithCtx(context.Background(), req)
}

func (c *ClaudeClient) CompletionsWithCtx(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if resp := c.responseAfterToolResult(req.Messages); resp != nil {
		return resp, nil
	}

	if len(req.Tools) > 0 {
		resp, err := c.toolCompletionByRuntime(ctx, req)
		if err != nil && errors.Is(err, errEmptyCodexToolCalls) && ctx.Err() == nil {
			resp, err = c.toolCompletionByRuntime(ctx, req)
		}
		return resp, err
	}

	prompt := codexPromptFromMessages(req.Messages)
	if c.runtime() == claudeRuntimeAppServer {
		return c.appServerTextCompletion(ctx, req, prompt)
	}
	return c.textCompletion(ctx, req, prompt)
}

func (c *ClaudeClient) StreamCompletion(req ChatRequest, cb func(chunk []byte) error) error {
	if len(req.Tools) > 0 {
		return fmt.Errorf("claude provider does not support streaming tool completions; use Completions instead")
	}
	resp, err := c.Completions(req)
	if err != nil {
		return err
	}
	if content := resp.Content(); content != "" {
		return cb([]byte(content))
	}
	return nil
}

func (c *ClaudeClient) runtime() string {
	runtime, _ := c.cfg.ExtraBody["claude_runtime"].(string)
	switch strings.ToLower(strings.TrimSpace(runtime)) {
	case "app_server", "app-server", "appserver":
		return claudeRuntimeAppServer
	default:
		return claudeRuntimeExec
	}
}

func (c *ClaudeClient) repoDir() string {
	repoDir, _ := c.cfg.ExtraBody["repo_dir"].(string)
	return repoDir
}

func (c *ClaudeClient) toolCompletionByRuntime(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if c.runtime() == claudeRuntimeAppServer {
		return c.appServerToolCompletion(ctx, req)
	}
	return c.toolCompletion(ctx, req)
}

func (c *ClaudeClient) toolCompletion(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	prompt, err := c.toolPrompt(req)
	if err != nil {
		return nil, err
	}
	result, err := c.runClaude(ctx, req.Model, prompt)
	if err != nil {
		return nil, err
	}
	return claudeToolCallsToChatResponse(result, req.Tools, c.modelFor(req.Model))
}

func (c *ClaudeClient) appServerToolCompletion(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	prompt, err := c.toolPrompt(req)
	if err != nil {
		return nil, err
	}
	key := claudeConversationKey(req.Messages)
	result, err := c.runClaudeAppServer(ctx, req.Model, key, prompt)
	if err != nil {
		return nil, err
	}
	resp, err := claudeToolCallsToChatResponse(result, req.Tools, c.modelFor(req.Model))
	if err != nil {
		c.closeAppServerForKey(key)
		return nil, err
	}
	if responseHasTaskDone(resp) {
		c.closeAppServerForKey(key)
	}
	return resp, nil
}

func (c *ClaudeClient) textCompletion(ctx context.Context, req ChatRequest, prompt string) (*ChatResponse, error) {
	content, err := c.runClaude(ctx, req.Model, prompt)
	if err != nil {
		return nil, err
	}
	return textChatResponse(c.modelFor(req.Model), strings.TrimSpace(content)), nil
}

func (c *ClaudeClient) appServerTextCompletion(ctx context.Context, req ChatRequest, prompt string) (*ChatResponse, error) {
	key := claudeConversationKey(req.Messages)
	content, err := c.runClaudeAppServer(ctx, req.Model, key, prompt)
	c.closeAppServerForKey(key)
	if err != nil {
		return nil, err
	}
	return textChatResponse(c.modelFor(req.Model), strings.TrimSpace(content)), nil
}

func (c *ClaudeClient) runClaude(ctx context.Context, model, prompt string) (string, error) {
	runCtx := ctx
	cancel := func() {}
	if c.cfg.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, c.cfg.Timeout)
	}
	defer cancel()

	cmd := exec.CommandContext(runCtx, "claude", c.buildExecArgsForModel(c.modelFor(model))...)
	if repoDir := c.repoDir(); repoDir != "" {
		cmd.Dir = repoDir
	}
	cmd.Stdin = strings.NewReader(prompt)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("claude -p failed: %s", msg)
	}
	return parseClaudeJSONResult(stdout.Bytes(), stderr.String())
}

func (c *ClaudeClient) runClaudeAppServer(ctx context.Context, model, key, prompt string) (string, error) {
	runCtx := ctx
	cancel := func() {}
	if c.cfg.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, c.cfg.Timeout)
	}
	defer cancel()

	client, err := c.appServerClient(runCtx, key, c.modelFor(model), c.repoDir())
	if err != nil {
		return "", err
	}
	text, err := client.Complete(runCtx, prompt)
	// The Claude stream-json process receives one JSONL turn and then stdin is
	// closed. Do not reuse it for later OCR turns; the full OCR conversation is
	// already represented in each prompt's message history.
	c.closeAppServerForKey(key)
	return text, err
}

func (c *ClaudeClient) appServerClient(ctx context.Context, key, model, repoDir string) (*claudeAppServerClient, error) {
	c.appServerMu.Lock()
	defer c.appServerMu.Unlock()
	if c.appServers == nil {
		c.appServers = make(map[string]*claudeAppServerClient)
	}
	if client := c.appServers[key]; client != nil && !client.Closed() && client.Matches(model, repoDir) {
		return client, nil
	}
	if client := c.appServers[key]; client != nil {
		client.Close()
		delete(c.appServers, key)
	}
	client, err := startClaudeAppServer(ctx, model, repoDir)
	if err != nil {
		return nil, err
	}
	c.appServers[key] = client
	return client, nil
}

func (c *ClaudeClient) dropAppServerClient(key string, client *claudeAppServerClient) {
	c.appServerMu.Lock()
	if c.appServers[key] == client {
		delete(c.appServers, key)
	}
	c.appServerMu.Unlock()
}

func (c *ClaudeClient) closeAppServerForKey(key string) {
	c.appServerMu.Lock()
	client := c.appServers[key]
	if client != nil {
		client.Close()
		delete(c.appServers, key)
	}
	c.appServerMu.Unlock()
}

func (c *ClaudeClient) buildExecArgsForModel(model string) []string {
	args := []string{"-p", "--output-format", "json", "--max-turns", "1"}
	args = append(args, claudeProviderIsolationArgs()...)
	if model != "" {
		args = append(args, "--model", model)
	}
	return args
}

func (c *ClaudeClient) modelFor(model string) string {
	if model != "" {
		return model
	}
	return c.cfg.Model
}

func (c *ClaudeClient) responseAfterToolResult(messages []Message) *ChatResponse {
	return (&CodexClient{}).responseAfterToolResult(messages)
}

func (c *ClaudeClient) toolPrompt(req ChatRequest) (string, error) {
	prompt, err := (&CodexClient{cfg: c.cfg}).toolPrompt(req)
	if err != nil {
		return "", err
	}
	return prompt + "\n\nReturn only a single JSON object. Do not wrap it in Markdown. It must match this JSON Schema:\n" + codexProviderToolCallsSchema, nil
}

func claudeConversationKey(messages []Message) string {
	var sb strings.Builder
	for _, msg := range messages {
		if msg.Role != "system" && msg.Role != "user" {
			continue
		}
		sb.WriteString(msg.Role)
		sb.WriteByte(0)
		sb.WriteString(msg.ExtractText())
		sb.WriteByte(0)
		if msg.Role == "user" {
			break
		}
	}
	if sb.Len() == 0 {
		sb.WriteString("default")
	}
	sum := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(sum[:])
}

func responseHasTaskDone(resp *ChatResponse) bool {
	for _, call := range resp.ToolCalls() {
		if call.Function.Name == "task_done" {
			return true
		}
	}
	return false
}

func claudeProviderIsolationArgs() []string {
	return []string{
		"--tools", "",
		"--disable-slash-commands",
		"--no-session-persistence",
		"--strict-mcp-config",
		"--setting-sources", "user",
	}
}

type claudeJSONResult struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
}

func parseClaudeJSONResult(data []byte, stderr string) (string, error) {
	var result claudeJSONResult
	if err := json.Unmarshal(data, &result); err != nil {
		msg := strings.TrimSpace(string(data))
		if msg == "" {
			msg = strings.TrimSpace(stderr)
		}
		return "", fmt.Errorf("parse claude JSON output: %w: %s", err, msg)
	}
	if result.IsError {
		msg := strings.TrimSpace(result.Result)
		if msg == "" {
			msg = strings.TrimSpace(stderr)
		}
		if msg == "" {
			msg = "claude returned an error result"
		}
		return "", fmt.Errorf("claude -p failed: %s", msg)
	}
	return result.Result, nil
}

func claudeToolCallsToChatResponse(result string, tools []ToolDef, model string) (*ChatResponse, error) {
	payload := extractClaudeJSONPayload(result)
	return codexToolCallsToChatResponse([]byte(payload), tools, model)
}

func extractClaudeJSONPayload(text string) string {
	trimmed := strings.TrimSpace(text)
	if json.Valid([]byte(trimmed)) {
		return trimmed
	}
	if payload, ok := firstJSONObject(trimmed); ok {
		return payload
	}
	return trimmed
}

func firstJSONObject(text string) (string, bool) {
	start := strings.Index(text, "{")
	if start < 0 {
		return "", false
	}

	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(text); i++ {
		ch := text[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == '"' {
				inString = false
			}
			continue
		}

		switch ch {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				candidate := text[start : i+1]
				if json.Valid([]byte(candidate)) {
					return candidate, true
				}
			}
		}
	}
	return "", false
}
