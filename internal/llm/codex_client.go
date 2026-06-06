package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// CodexClient adapts the official Codex CLI into OCR's LLMClient interface.
// It uses Codex's own ChatGPT/API-key authentication instead of extracting tokens.
type CodexClient struct {
	cfg         ClientConfig
	appServerMu sync.Mutex
	appServer   *codexAppServerClient
}

func NewCodexClient(cfg ClientConfig) *CodexClient {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Minute
	}
	return &CodexClient{cfg: cfg}
}

func (c *CodexClient) Completions(req ChatRequest) (*ChatResponse, error) {
	return c.CompletionsWithCtx(context.Background(), req)
}

func (c *CodexClient) CompletionsWithCtx(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if resp := c.responseAfterToolResult(req.Messages); resp != nil {
		return resp, nil
	}

	if len(req.Tools) > 0 {
		if c.runtime() == codexRuntimeAppServer {
			return c.appServerToolCompletion(ctx, req)
		}
		return c.toolCompletion(ctx, req)
	}
	prompt := codexPromptFromMessages(req.Messages)
	if c.runtime() == codexRuntimeAppServer {
		return c.appServerTextCompletion(ctx, req, prompt)
	}
	return c.textCompletion(ctx, req, prompt)
}

func (c *CodexClient) StreamCompletion(req ChatRequest, cb func(chunk []byte) error) error {
	// Tool completions return their payload in ToolCalls with empty content;
	// forwarding only content would silently drop the requested action.
	if len(req.Tools) > 0 {
		return fmt.Errorf("codex provider does not support streaming tool completions; use Completions instead")
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

func (c *CodexClient) runtime() string {
	runtime, _ := c.cfg.ExtraBody["codex_runtime"].(string)
	switch strings.ToLower(strings.TrimSpace(runtime)) {
	case "app_server", "app-server", "appserver":
		return codexRuntimeAppServer
	default:
		return codexRuntimeExec
	}
}

func (c *CodexClient) repoDir() string {
	repoDir, _ := c.cfg.ExtraBody["repo_dir"].(string)
	return repoDir
}

func (c *CodexClient) toolCompletion(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	tmpDir, err := os.MkdirTemp("", "ocr-codex-provider-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	schemaPath := filepath.Join(tmpDir, "tool-calls.schema.json")
	outputPath := filepath.Join(tmpDir, "last-message.json")
	if err := os.WriteFile(schemaPath, []byte(codexProviderToolCallsSchema), 0o600); err != nil {
		return nil, fmt.Errorf("write schema: %w", err)
	}

	if err := c.runCodex(ctx, req.Model, schemaPath, outputPath, c.toolPrompt(req)); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		return nil, fmt.Errorf("read codex output: %w", err)
	}
	return codexToolCallsToChatResponse(data, req.Tools, c.modelFor(req.Model))
}

func (c *CodexClient) appServerToolCompletion(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	data, err := c.runCodexAppServer(ctx, req.Model, c.toolPrompt(req), []byte(codexProviderToolCallsSchema))
	if err != nil {
		return nil, err
	}
	return codexToolCallsToChatResponse([]byte(data), req.Tools, c.modelFor(req.Model))
}

func (c *CodexClient) textCompletion(ctx context.Context, req ChatRequest, prompt string) (*ChatResponse, error) {
	tmpDir, err := os.MkdirTemp("", "ocr-codex-provider-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	outputPath := filepath.Join(tmpDir, "last-message.txt")
	if err := c.runCodex(ctx, req.Model, "", outputPath, prompt); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		return nil, fmt.Errorf("read codex output: %w", err)
	}
	content := strings.TrimSpace(string(data))
	return textChatResponse(c.modelFor(req.Model), content), nil
}

func (c *CodexClient) appServerTextCompletion(ctx context.Context, req ChatRequest, prompt string) (*ChatResponse, error) {
	content, err := c.runCodexAppServer(ctx, req.Model, prompt, nil)
	if err != nil {
		return nil, err
	}
	return textChatResponse(c.modelFor(req.Model), strings.TrimSpace(content)), nil
}

func (c *CodexClient) runCodexAppServer(ctx context.Context, model, prompt string, outputSchema []byte) (string, error) {
	// Apply the request timeout before acquiring the client so that app-server
	// startup (process spawn + initialize handshake) is also bounded by it.
	runCtx := ctx
	cancel := func() {}
	if c.cfg.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, c.cfg.Timeout)
	}
	defer cancel()

	client, err := c.appServerClient(runCtx)
	if err != nil {
		return "", err
	}

	text, err := client.Complete(runCtx, codexAppServerCompletion{
		Model:        c.modelFor(model),
		RepoDir:      c.repoDir(),
		Prompt:       prompt,
		OutputSchema: outputSchema,
	})
	if err != nil && client.Closed() {
		// The app-server process died; drop the cached client so the next
		// completion restarts it instead of reusing a dead pipe.
		c.dropAppServerClient(client)
	}
	return text, err
}

func (c *CodexClient) appServerClient(ctx context.Context) (*codexAppServerClient, error) {
	c.appServerMu.Lock()
	defer c.appServerMu.Unlock()
	if c.appServer != nil && !c.appServer.Closed() {
		return c.appServer, nil
	}
	c.appServer = nil
	client, err := startCodexAppServer(ctx)
	if err != nil {
		return nil, err
	}
	c.appServer = client
	return client, nil
}

// dropAppServerClient clears the cached client if it is still the given one,
// forcing the next call to start a fresh app-server process.
func (c *CodexClient) dropAppServerClient(client *codexAppServerClient) {
	c.appServerMu.Lock()
	if c.appServer == client {
		c.appServer = nil
	}
	c.appServerMu.Unlock()
}

func (c *CodexClient) runCodex(ctx context.Context, model, schemaPath, outputPath, prompt string) error {
	if model == "" {
		model = c.cfg.Model
	}
	runCtx := ctx
	cancel := func() {}
	if c.cfg.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, c.cfg.Timeout)
	}
	defer cancel()

	cmd := exec.CommandContext(runCtx, "codex", c.buildExecArgsForModel(model, schemaPath, outputPath)...)
	cmd.Stdin = strings.NewReader(prompt)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("codex exec failed: %s", msg)
	}
	return nil
}

func (c *CodexClient) buildExecArgs(schemaPath, outputPath string) []string {
	return c.buildExecArgsForModel(c.cfg.Model, schemaPath, outputPath)
}

func (c *CodexClient) buildExecArgsForModel(model, schemaPath, outputPath string) []string {
	args := []string{"exec"}
	if repoDir, ok := c.cfg.ExtraBody["repo_dir"].(string); ok && repoDir != "" {
		args = append(args, "--cd", repoDir)
	}
	args = append(args, "--sandbox", "read-only")
	args = append(args, "--output-last-message", outputPath)
	if schemaPath != "" {
		args = append(args, "--output-schema", schemaPath)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	args = append(args, "--ephemeral")
	args = append(args, "-")
	return args
}

func (c *CodexClient) modelFor(model string) string {
	if model != "" {
		return model
	}
	return c.cfg.Model
}

func (c *CodexClient) responseAfterToolResult(messages []Message) *ChatResponse {
	return nil
}

func codexPromptFromMessages(messages []Message) string {
	var sb strings.Builder
	for _, m := range messages {
		switch {
		case len(m.ToolCalls) > 0:
			sb.WriteString("ASSISTANT TOOL CALLS:\n")
			for _, call := range m.ToolCalls {
				sb.WriteString("- ")
				sb.WriteString(call.Function.Name)
				if call.ID != "" {
					sb.WriteString(" (")
					sb.WriteString(call.ID)
					sb.WriteString(")")
				}
				if call.Function.Arguments != "" {
					sb.WriteString(": ")
					sb.WriteString(call.Function.Arguments)
				}
				sb.WriteString("\n")
			}
			sb.WriteString("\n")
		case m.Role == "tool":
			text := m.ExtractText()
			if text == "" {
				continue
			}
			sb.WriteString("TOOL RESULT")
			if m.ToolCallID != "" {
				sb.WriteString(" (")
				sb.WriteString(m.ToolCallID)
				sb.WriteString(")")
			}
			sb.WriteString(":\n")
			sb.WriteString(text)
			sb.WriteString("\n\n")
		default:
			text := m.ExtractText()
			if text == "" {
				continue
			}
			sb.WriteString(strings.ToUpper(m.Role))
			sb.WriteString(":\n")
			sb.WriteString(text)
			sb.WriteString("\n\n")
		}
	}
	return strings.TrimSpace(sb.String())
}

func (c *CodexClient) toolPrompt(req ChatRequest) string {
	var sb strings.Builder
	prompt := codexPromptFromMessages(req.Messages)
	if prompt != "" {
		sb.WriteString(prompt)
		sb.WriteString("\n\n")
	}
	sb.WriteString(codexProviderToolCallInstruction)
	sb.WriteString("\n\nAvailable OCR tools:\n")
	sb.WriteString(formatCodexToolDefs(req.Tools))
	return strings.TrimSpace(sb.String())
}

func formatCodexToolDefs(tools []ToolDef) string {
	data, err := json.MarshalIndent(tools, "", "  ")
	if err != nil {
		return "[]"
	}
	return string(data)
}

func codexToolCallsToChatResponse(raw []byte, tools []ToolDef, model string) (*ChatResponse, error) {
	var out codexToolCallsOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse codex tool calls: %w", err)
	}
	if len(out.ToolCalls) == 0 {
		// The provider instruction requires an explicit task_done call when the
		// review is complete. An empty array indicates schema drift, truncation,
		// or a malformed response — surface it so the agent's retry path runs
		// instead of silently marking the review done.
		return nil, fmt.Errorf("parse codex tool calls: empty tool_calls; expected an explicit task_done call")
	}

	allowed := allowedCodexTools(tools)
	calls := make([]ToolCall, 0, len(out.ToolCalls))
	for i, item := range out.ToolCalls {
		name := strings.TrimSpace(item.Name)
		if name == "" {
			return nil, fmt.Errorf("parse codex tool calls: tool call %d has empty name", i)
		}
		if !allowed[name] {
			return nil, fmt.Errorf("parse codex tool calls: tool %q is not available", name)
		}
		args, err := normalizeCodexArguments(item.Arguments)
		if err != nil {
			return nil, fmt.Errorf("parse codex tool calls: arguments for %q: %w", name, err)
		}
		calls = append(calls, ToolCall{
			ID:   fmt.Sprintf("codex_tool_%d", i+1),
			Type: "function",
			Function: FunctionCall{
				Name:      name,
				Arguments: args,
			},
		})
	}
	return toolCallsChatResponse(model, calls), nil
}

func allowedCodexTools(tools []ToolDef) map[string]bool {
	allowed := make(map[string]bool, len(tools))
	for _, tool := range tools {
		if tool.Function.Name != "" {
			allowed[tool.Function.Name] = true
		}
	}
	return allowed
}

func normalizeCodexArguments(raw json.RawMessage) (string, error) {
	args := strings.TrimSpace(string(raw))
	if args == "" || args == "null" {
		args = "{}"
	} else if strings.HasPrefix(args, `"`) {
		var decoded string
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return "", err
		}
		args = strings.TrimSpace(decoded)
		if args == "" || args == "null" {
			args = "{}"
		}
	}
	// Downstream tool execution unmarshals arguments into a JSON object, so
	// reject arrays/strings/numbers here at the provider boundary rather than
	// letting them fail later as tool errors and retry loops.
	var obj map[string]any
	if err := json.Unmarshal([]byte(args), &obj); err != nil {
		return "", fmt.Errorf("tool arguments must be a JSON object: %w", err)
	}
	return compactJSON(args)
}

func compactJSON(s string) (string, error) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(s)); err != nil {
		return "", err
	}
	return buf.String(), nil
}

type codexToolCallsOutput struct {
	ToolCalls []codexToolCallOutput `json:"tool_calls"`
}

type codexToolCallOutput struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func toolCallChatResponse(model string, call ToolCall) *ChatResponse {
	return toolCallsChatResponse(model, []ToolCall{call})
}

func toolCallsChatResponse(model string, calls []ToolCall) *ChatResponse {
	content := ""
	return &ChatResponse{
		Model: model,
		Choices: []Choice{{
			Message: ResponseMessage{
				Role:      "assistant",
				Content:   &content,
				ToolCalls: calls,
			},
			FinishReason: "tool_calls",
		}},
	}
}

func textChatResponse(model, content string) *ChatResponse {
	return &ChatResponse{
		Model: model,
		Choices: []Choice{{
			Message: ResponseMessage{
				Role:    "assistant",
				Content: &content,
			},
			FinishReason: "stop",
		}},
	}
}

const codexProviderToolCallInstruction = `You are running inside OpenCodeReview's native review loop.
Return only JSON matching this shape:
{"tool_calls":[{"name":"file_read","arguments":"{\"file_path\":\"relative/file\",\"start_line\":1,\"end_line\":80}"}]}

Use only the available OCR tools listed below.
Use file_read, file_read_diff, and code_search when more repository context is needed.
Use code_comment when you have concrete review comments to submit.
Use task_done when the review is complete.
The arguments value must be a JSON string containing the tool arguments object.
If you do not need any more tool calls, return {"tool_calls":[{"name":"task_done","arguments":"{\"state\":\"DONE\"}"}]}.`

const codexProviderToolCallsSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "tool_calls": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "object",
        "additionalProperties": false,
        "properties": {
          "name": {"type": "string"},
          "arguments": {
            "type": "string"
          }
        },
        "required": ["name", "arguments"]
      }
    }
  },
  "required": ["tool_calls"]
}`
