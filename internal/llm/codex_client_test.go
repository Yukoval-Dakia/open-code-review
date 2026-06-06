package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNewLLMClientReturnsCodexClient(t *testing.T) {
	client := NewLLMClient(ResolvedEndpoint{
		Protocol: "codex",
		Model:    "gpt-5.4",
	})
	if _, ok := client.(*CodexClient); !ok {
		t.Fatalf("NewLLMClient(codex) = %T, want *CodexClient", client)
	}
}

func TestBuildCodexExecArgsUsesOfficialCodexCLI(t *testing.T) {
	c := NewCodexClient(ClientConfig{
		Model: "gpt-5.4",
		ExtraBody: map[string]any{
			"repo_dir": "/tmp/repo",
		},
	})

	got := c.buildExecArgs("/tmp/schema.json", "/tmp/out.txt")
	want := []string{
		"exec",
		"--cd", "/tmp/repo",
		"--sandbox", "read-only",
		"--output-last-message", "/tmp/out.txt",
		"--output-schema", "/tmp/schema.json",
		"--model", "gpt-5.4",
		"--ephemeral",
		"-",
	}
	if len(got) != len(want) {
		t.Fatalf("len(args) = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("arg %d = %q, want %q; args=%#v", i, got[i], want[i], got)
		}
	}
}

func TestCodexRuntimeDefaultsToExec(t *testing.T) {
	c := NewCodexClient(ClientConfig{})
	if got := c.runtime(); got != "exec" {
		t.Fatalf("runtime = %q, want exec", got)
	}
}

func TestCodexRuntimeCanUseAppServer(t *testing.T) {
	c := NewCodexClient(ClientConfig{
		ExtraBody: map[string]any{
			"codex_runtime": "app_server",
		},
	})
	if got := c.runtime(); got != "app_server" {
		t.Fatalf("runtime = %q, want app_server", got)
	}
}

func TestBuildCodexAppServerThreadStartParams(t *testing.T) {
	c := NewCodexClient(ClientConfig{
		Model: "gpt-5.4",
		ExtraBody: map[string]any{
			"repo_dir": "/tmp/repo",
		},
	})

	params := c.appServerThreadStartParams("gpt-5.4")
	if params["model"] != "gpt-5.4" {
		t.Fatalf("model = %v, want gpt-5.4", params["model"])
	}
	if params["cwd"] != "/tmp/repo" {
		t.Fatalf("cwd = %v, want /tmp/repo", params["cwd"])
	}
	if params["sandbox"] != "read-only" {
		t.Fatalf("sandbox = %v, want read-only", params["sandbox"])
	}
	if params["ephemeral"] != true {
		t.Fatalf("ephemeral = %v, want true", params["ephemeral"])
	}
	if envs, ok := params["environments"].([]any); !ok || len(envs) != 0 {
		t.Fatalf("environments = %#v, want empty slice to disable Codex internal environment tools", params["environments"])
	}
}

func TestBuildCodexAppServerTurnStartParams(t *testing.T) {
	params := codexAppServerTurnStartParams("thread_1", "gpt-5.4", "/tmp/repo", "hello", []byte(codexProviderToolCallsSchema))

	if params["threadId"] != "thread_1" {
		t.Fatalf("threadId = %v, want thread_1", params["threadId"])
	}
	if params["model"] != "gpt-5.4" {
		t.Fatalf("model = %v, want gpt-5.4", params["model"])
	}
	if params["cwd"] != "/tmp/repo" {
		t.Fatalf("cwd = %v, want /tmp/repo", params["cwd"])
	}
	input := params["input"].([]map[string]string)
	if got := input[0]["text"]; got != "hello" {
		t.Fatalf("input text = %q, want hello", got)
	}
	if params["outputSchema"] == nil {
		t.Fatalf("outputSchema is nil")
	}
	if envs, ok := params["environments"].([]any); !ok || len(envs) != 0 {
		t.Fatalf("environments = %#v, want empty slice to disable Codex internal environment tools", params["environments"])
	}
}

func TestCodexAppServerAccumulatorReturnsFinalAgentMessage(t *testing.T) {
	acc := newCodexAppServerTurnAccumulator("thread-1")
	acc.HandleNotification(map[string]any{
		"method": "item/completed",
		"params": map[string]any{
			"threadId": "thread-1",
			"item": map[string]any{
				"type":  "agentMessage",
				"text":  `{"tool_calls":[{"name":"task_done","arguments":"{\"state\":\"DONE\"}"}]}`,
				"phase": "final_answer",
			},
		},
	})

	got := acc.FinalText()
	if !strings.Contains(got, `"tool_calls"`) {
		t.Fatalf("FinalText() = %q, want final tool call JSON", got)
	}
}

func TestCodexAppServerAccumulatorIgnoresOtherThreads(t *testing.T) {
	acc := newCodexAppServerTurnAccumulator("thread-2")

	// Stale events from an earlier canceled turn on a different thread must
	// not contaminate this turn's state.
	acc.HandleNotification(map[string]any{
		"method": "item/completed",
		"params": map[string]any{
			"threadId": "thread-1",
			"item": map[string]any{
				"type":  "agentMessage",
				"text":  "stale answer",
				"phase": "final_answer",
			},
		},
	})
	acc.HandleNotification(map[string]any{
		"method": "turn/completed",
		"params": map[string]any{"threadId": "thread-1"},
	})

	if acc.Completed() {
		t.Fatalf("accumulator completed from another thread's turn/completed")
	}
	if got := acc.FinalText(); got != "" {
		t.Fatalf("FinalText() = %q, want empty (stale thread ignored)", got)
	}

	// Events without a recognizable thread id are still accepted.
	acc.HandleNotification(map[string]any{
		"method": "turn/completed",
		"params": map[string]any{},
	})
	if !acc.Completed() {
		t.Fatalf("accumulator ignored turn/completed without thread id")
	}
}

func TestCodexToolCallsToChatResponseEmitsRequestedToolCalls(t *testing.T) {
	resp, err := codexToolCallsToChatResponse([]byte(`{
		"tool_calls": [{
			"name": "file_read",
			"arguments": {
				"file_path": "src/app.go",
				"start_line": 1,
				"end_line": 20
			}
		}]
	}`), []ToolDef{testCodexTool("file_read")}, "gpt-5.4")
	if err != nil {
		t.Fatalf("codexToolCallsToChatResponse returned error: %v", err)
	}

	calls := resp.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(calls))
	}
	if calls[0].Function.Name != "file_read" {
		t.Fatalf("tool name = %q, want file_read", calls[0].Function.Name)
	}
	if !strings.Contains(calls[0].Function.Arguments, `"file_path":"src/app.go"`) {
		t.Fatalf("arguments missing file_path: %s", calls[0].Function.Arguments)
	}
}

func TestCodexToolCallsToChatResponseAcceptsJSONStringArguments(t *testing.T) {
	resp, err := codexToolCallsToChatResponse([]byte(`{
		"tool_calls": [{
			"name": "file_read",
			"arguments": "{\"file_path\":\"src/app.go\",\"start_line\":1,\"end_line\":20}"
		}]
	}`), []ToolDef{testCodexTool("file_read")}, "gpt-5.4")
	if err != nil {
		t.Fatalf("codexToolCallsToChatResponse returned error: %v", err)
	}

	calls := resp.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(calls))
	}
	if got := calls[0].Function.Arguments; !strings.Contains(got, `"file_path":"src/app.go"`) {
		t.Fatalf("arguments were not decoded into an object JSON string: %s", got)
	}
}

func TestCodexToolCallsSchemaUsesStrictJSONStringArguments(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal([]byte(codexProviderToolCallsSchema), &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}

	properties := schema["properties"].(map[string]any)
	toolCalls := properties["tool_calls"].(map[string]any)
	items := toolCalls["items"].(map[string]any)
	itemProps := items["properties"].(map[string]any)
	args := itemProps["arguments"].(map[string]any)

	if got := args["type"]; got != "string" {
		t.Fatalf("arguments schema type = %v, want string for Codex strict structured output compatibility", got)
	}
	if _, ok := args["additionalProperties"]; ok {
		t.Fatalf("arguments string schema must not use additionalProperties: %#v", args)
	}
}

func TestCodexToolCallsToChatResponseRejectsUnknownToolCalls(t *testing.T) {
	_, err := codexToolCallsToChatResponse([]byte(`{
		"tool_calls": [{
			"name": "shell_exec",
			"arguments": {"cmd": "echo nope"}
		}]
	}`), []ToolDef{testCodexTool("file_read")}, "gpt-5.4")
	if err == nil {
		t.Fatalf("codexToolCallsToChatResponse returned nil error for unknown tool")
	}
}

func TestCodexToolCallsToChatResponseRejectsEmptyToolCalls(t *testing.T) {
	// An empty array bypasses the explicit task_done contract (schema drift,
	// truncation, or malformed output) and must surface as an error so the
	// agent retry path runs instead of silently completing the review.
	_, err := codexToolCallsToChatResponse([]byte(`{"tool_calls":[]}`), []ToolDef{testCodexTool("task_done")}, "gpt-5.4")
	if err == nil {
		t.Fatalf("codexToolCallsToChatResponse returned nil error for empty tool_calls")
	}
}

func TestCodexToolCallsToChatResponseRejectsNonObjectArguments(t *testing.T) {
	for _, args := range []string{`[1,2]`, `"[]"`, `42`, `true`, `"\"text\""`} {
		_, err := codexToolCallsToChatResponse([]byte(`{
			"tool_calls": [{"name": "file_read", "arguments": `+args+`}]
		}`), []ToolDef{testCodexTool("file_read")}, "gpt-5.4")
		if err == nil {
			t.Fatalf("codexToolCallsToChatResponse accepted non-object arguments %s", args)
		}
	}
}

func TestNormalizeCodexArgumentsMapsNullVariantsToEmptyObject(t *testing.T) {
	for _, raw := range []string{``, `null`, `"null"`, `""`} {
		got, err := normalizeCodexArguments(json.RawMessage(raw))
		if err != nil {
			t.Fatalf("normalizeCodexArguments(%q) returned error: %v", raw, err)
		}
		if got != "{}" {
			t.Fatalf("normalizeCodexArguments(%q) = %q, want {}", raw, got)
		}
	}
}

func TestBuildCodexToolPromptIncludesToolDefinitionsAndResults(t *testing.T) {
	c := NewCodexClient(ClientConfig{Model: "gpt-5.4"})
	prompt := c.toolPrompt(ChatRequest{
		Messages: []Message{
			NewTextMessage("system", "Review this diff."),
			NewToolCallMessage("", []ToolCall{{
				ID:   "call_1",
				Type: "function",
				Function: FunctionCall{
					Name:      "file_read",
					Arguments: `{"file_path":"src/app.go"}`,
				},
			}}),
			NewToolResultMessage("call_1", "package main"),
		},
		Tools: []ToolDef{testCodexTool("file_read")},
	})

	for _, want := range []string{"Available OCR tools", `"name":"file_read"`, "TOOL RESULT (call_1)", "package main"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestCodexClientDoesNotAutoTaskDoneAfterToolResult(t *testing.T) {
	c := NewCodexClient(ClientConfig{Model: "gpt-5.4"})
	if resp := c.responseAfterToolResult([]Message{
		NewToolResultMessage("call_1", "Comment submitted successfully."),
	}); resp != nil {
		t.Fatalf("responseAfterToolResult = %#v, want nil so Codex can continue the OCR tool loop", resp)
	}
}

func testCodexTool(name string) ToolDef {
	return ToolDef{
		Type: "function",
		Function: FunctionDef{
			Name:        name,
			Description: name + " description",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"file_path": map[string]any{"type": "string"},
				},
			},
		},
	}
}
