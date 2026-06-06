package llm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestStripModelSuffix(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"claude-opus-4-7[1m]", "claude-opus-4-7"},
		{"claude-sonnet-4-6[2m]", "claude-sonnet-4-6"},
		{"claude-opus-4-7[10m]", "claude-opus-4-7"},
		{"claude-opus-4-7", "claude-opus-4-7"},
		{"", ""},
		{"claude[1m]-extra", "claude[1m]-extra"},
		{"claude-opus-4-7[m]", "claude-opus-4-7[m]"},
		{"claude-opus-4-7[1M]", "claude-opus-4-7[1M]"},
		{"claude-opus-4-7[1]", "claude-opus-4-7[1]"},
	}

	for _, tt := range tests {
		got := stripModelSuffix(tt.input)
		if got != tt.want {
			t.Errorf("stripModelSuffix(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestResolveEndpoint_CCEnvStripsModelSuffix(t *testing.T) {
	t.Setenv("OCR_LLM_URL", "")
	t.Setenv("OCR_LLM_TOKEN", "")
	t.Setenv("OCR_LLM_MODEL", "")
	t.Setenv("ANTHROPIC_BASE_URL", "https://api.example.com")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "test-token")
	t.Setenv("ANTHROPIC_MODEL", "claude-opus-4-7[1m]")

	ep, err := ResolveEndpoint(filepath.Join(t.TempDir(), "nonexistent.json"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ep.Model != "claude-opus-4-7" {
		t.Errorf("expected model %q, got %q", "claude-opus-4-7", ep.Model)
	}
	if ep.Source != "Claude Code environment" {
		t.Errorf("expected source %q, got %q", "Claude Code environment", ep.Source)
	}
}

func TestResolveEndpoint_CCEnvCleanModelUnchanged(t *testing.T) {
	t.Setenv("OCR_LLM_URL", "")
	t.Setenv("OCR_LLM_TOKEN", "")
	t.Setenv("OCR_LLM_MODEL", "")
	t.Setenv("ANTHROPIC_BASE_URL", "https://api.example.com")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "test-token")
	t.Setenv("ANTHROPIC_MODEL", "claude-opus-4-7")

	ep, err := ResolveEndpoint(filepath.Join(t.TempDir(), "nonexistent.json"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ep.Model != "claude-opus-4-7" {
		t.Errorf("expected model %q, got %q", "claude-opus-4-7", ep.Model)
	}
}

func TestResolveEndpoint_OCREnvStripsModelSuffix(t *testing.T) {
	t.Setenv("OCR_LLM_URL", "https://api.example.com/v1/messages")
	t.Setenv("OCR_LLM_TOKEN", "test-token")
	t.Setenv("OCR_LLM_MODEL", "claude-haiku[2m]")

	ep, err := ResolveEndpoint(filepath.Join(t.TempDir(), "nonexistent.json"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ep.Model != "claude-haiku" {
		t.Errorf("expected model %q, got %q", "claude-haiku", ep.Model)
	}
	if ep.Source != "OCR environment" {
		t.Errorf("expected source %q, got %q", "OCR environment", ep.Source)
	}
}

func TestResolveEndpoint_ConfigFileStripsModelSuffix(t *testing.T) {
	t.Setenv("OCR_LLM_URL", "")
	t.Setenv("OCR_LLM_TOKEN", "")
	t.Setenv("OCR_LLM_MODEL", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_MODEL", "")

	cfg := configFile{
		Llm: llmFileConfig{
			URL:       "https://api.example.com/v1/messages",
			AuthToken: "test-token",
			Model:     "gpt-4[1m]",
		},
	}
	data, _ := json.Marshal(cfg)
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(cfgPath, data, 0644)

	ep, err := ResolveEndpoint(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ep.Model != "gpt-4" {
		t.Errorf("expected model %q, got %q", "gpt-4", ep.Model)
	}
	if ep.Source != "OCR config file" {
		t.Errorf("expected source %q, got %q", "OCR config file", ep.Source)
	}
}

func TestResolveEndpoint_ConfigFileCodexProtocolDoesNotRequireURLOrToken(t *testing.T) {
	t.Setenv("OCR_LLM_URL", "")
	t.Setenv("OCR_LLM_TOKEN", "")
	t.Setenv("OCR_LLM_MODEL", "")
	t.Setenv("OCR_LLM_PROTOCOL", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_MODEL", "")

	cfg := configFile{
		Llm: llmFileConfig{
			Protocol: "codex",
		},
	}
	data, _ := json.Marshal(cfg)
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(cfgPath, data, 0644)

	ep, err := ResolveEndpoint(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ep.Protocol != "codex" {
		t.Errorf("expected protocol %q, got %q", "codex", ep.Protocol)
	}
	if ep.Model != "" {
		t.Errorf("expected empty model for Codex default, got %q", ep.Model)
	}
}

func TestResolveEndpoint_ConfigFileCodexRuntimeAppServer(t *testing.T) {
	t.Setenv("OCR_LLM_URL", "")
	t.Setenv("OCR_LLM_TOKEN", "")
	t.Setenv("OCR_LLM_MODEL", "")
	t.Setenv("OCR_LLM_PROTOCOL", "")
	t.Setenv("OCR_CODEX_RUNTIME", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_MODEL", "")

	cfg := configFile{
		Llm: llmFileConfig{
			Protocol:     "codex",
			CodexRuntime: "app_server",
		},
	}
	data, _ := json.Marshal(cfg)
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(cfgPath, data, 0644)

	ep, err := ResolveEndpoint(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ep.ExtraBody["codex_runtime"] != "app_server" {
		t.Fatalf("codex_runtime = %#v, want app_server", ep.ExtraBody["codex_runtime"])
	}
}

func TestResolveEndpoint_OCREnvCodexProtocolDoesNotRequireURLOrToken(t *testing.T) {
	t.Setenv("OCR_LLM_PROTOCOL", "codex")
	t.Setenv("OCR_LLM_MODEL", "")
	t.Setenv("OCR_LLM_URL", "")
	t.Setenv("OCR_LLM_TOKEN", "")

	ep, err := ResolveEndpoint(filepath.Join(t.TempDir(), "nonexistent.json"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ep.Protocol != "codex" {
		t.Errorf("expected protocol %q, got %q", "codex", ep.Protocol)
	}
	if ep.Source != "OCR environment" {
		t.Errorf("expected source %q, got %q", "OCR environment", ep.Source)
	}
}

func TestResolveEndpoint_OCREnvCodexRuntimeAppServer(t *testing.T) {
	t.Setenv("OCR_LLM_PROTOCOL", "codex")
	t.Setenv("OCR_CODEX_RUNTIME", "app_server")
	t.Setenv("OCR_LLM_MODEL", "")
	t.Setenv("OCR_LLM_URL", "")
	t.Setenv("OCR_LLM_TOKEN", "")

	ep, err := ResolveEndpoint(filepath.Join(t.TempDir(), "nonexistent.json"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ep.ExtraBody["codex_runtime"] != "app_server" {
		t.Fatalf("codex_runtime = %#v, want app_server", ep.ExtraBody["codex_runtime"])
	}
}

func TestResolveEndpoint_OCREnvExplicitProtocolOverridesUseAnthropic(t *testing.T) {
	t.Setenv("OCR_LLM_URL", "https://api.example.com/v1/chat/completions")
	t.Setenv("OCR_LLM_TOKEN", "test-token")
	t.Setenv("OCR_LLM_MODEL", "gpt-5.4")
	t.Setenv("OCR_LLM_PROTOCOL", "openai")
	t.Setenv("OCR_USE_ANTHROPIC", "") // unset legacy toggle (would default to anthropic)

	ep, err := ResolveEndpoint(filepath.Join(t.TempDir(), "nonexistent.json"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ep.Protocol != "openai" {
		t.Errorf("protocol = %q, want openai (explicit llm.protocol must win over use_anthropic default)", ep.Protocol)
	}
}

func TestResolveEndpoint_OCREnvInvalidProtocolFails(t *testing.T) {
	t.Setenv("OCR_LLM_URL", "https://api.example.com")
	t.Setenv("OCR_LLM_TOKEN", "test-token")
	t.Setenv("OCR_LLM_MODEL", "gpt-5.4")
	t.Setenv("OCR_LLM_PROTOCOL", "gemini")

	if _, err := ResolveEndpoint(filepath.Join(t.TempDir(), "nonexistent.json")); err == nil {
		t.Fatalf("expected error for invalid OCR_LLM_PROTOCOL, got nil")
	}
}

func TestResolveEndpoint_ConfigFileExplicitProtocolHonored(t *testing.T) {
	t.Setenv("OCR_LLM_URL", "")
	t.Setenv("OCR_LLM_TOKEN", "")
	t.Setenv("OCR_LLM_MODEL", "")
	t.Setenv("OCR_LLM_PROTOCOL", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_MODEL", "")

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := map[string]any{"llm": map[string]any{
		"url":        "https://api.example.com/v1/chat/completions",
		"auth_token": "test-token",
		"model":      "gpt-5.4",
		"protocol":   "openai",
	}}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	ep, err := ResolveEndpoint(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ep.Protocol != "openai" {
		t.Errorf("protocol = %q, want openai (llm.protocol must not be silently ignored)", ep.Protocol)
	}
}

func TestResolveEndpoint_ConfigFileInvalidProtocolFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := map[string]any{"llm": map[string]any{
		"url":        "https://api.example.com",
		"auth_token": "test-token",
		"model":      "gpt-5.4",
		"protocol":   "gemini",
	}}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := ResolveEndpoint(path); err == nil {
		t.Fatalf("expected error for invalid llm.protocol in config file, got nil")
	}
}

func TestResolveEndpoint_EnvProtocolOverridesConfigFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := map[string]any{"llm": map[string]any{
		"url":        "https://api.example.com/v1/chat/completions",
		"auth_token": "test-token",
		"model":      "gpt-5.4",
	}}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OCR_LLM_PROTOCOL", "codex")
	t.Setenv("OCR_LLM_MODEL", "")
	t.Setenv("OCR_CODEX_RUNTIME", "")

	ep, err := ResolveEndpoint(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ep.Protocol != "codex" {
		t.Errorf("protocol = %q, want codex (explicit env override must beat config file)", ep.Protocol)
	}
	if ep.Source != "OCR environment" {
		t.Errorf("source = %q, want OCR environment", ep.Source)
	}
}

func TestResolveEndpoint_InvalidCodexRuntimeFails(t *testing.T) {
	t.Setenv("OCR_LLM_PROTOCOL", "codex")
	t.Setenv("OCR_CODEX_RUNTIME", "app_servr") // typo must error, not silently fall back

	if _, err := ResolveEndpoint(filepath.Join(t.TempDir(), "nonexistent.json")); err == nil {
		t.Fatalf("expected error for invalid OCR_CODEX_RUNTIME, got nil")
	}
}

func TestCodexRuntimeExtraBodyNormalizesAliases(t *testing.T) {
	for _, alias := range []string{"app_server", "app-server", "appserver", " App_Server "} {
		extra, err := codexRuntimeExtraBody(alias, nil)
		if err != nil {
			t.Fatalf("codexRuntimeExtraBody(%q) returned error: %v", alias, err)
		}
		if got := extra["codex_runtime"]; got != "app_server" {
			t.Fatalf("codexRuntimeExtraBody(%q) = %v, want app_server", alias, got)
		}
	}
}

func TestResolveEndpoint_EnvNonCodexProtocolRequiresFullEndpoint(t *testing.T) {
	t.Setenv("OCR_LLM_PROTOCOL", "openai")
	t.Setenv("OCR_LLM_URL", "")
	t.Setenv("OCR_LLM_TOKEN", "")
	t.Setenv("OCR_LLM_MODEL", "")

	// A protocol-only override cannot be satisfied; silently resolving the
	// config file's (different) protocol instead would betray the request.
	if _, err := ResolveEndpoint(filepath.Join(t.TempDir(), "nonexistent.json")); err == nil {
		t.Fatalf("expected error for OCR_LLM_PROTOCOL=openai without URL/token/model, got nil")
	}
}

func TestCodexRuntimeExtraBodyValidatesExtraBodyKey(t *testing.T) {
	if _, err := codexRuntimeExtraBody("", map[string]any{"codex_runtime": "app_servr"}); err == nil {
		t.Fatalf("expected error for typo codex_runtime inside extra_body, got nil")
	}
	if _, err := codexRuntimeExtraBody("", map[string]any{"codex_runtime": 42}); err == nil {
		t.Fatalf("expected error for non-string codex_runtime inside extra_body, got nil")
	}

	extra, err := codexRuntimeExtraBody("", map[string]any{"codex_runtime": "App-Server"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := extra["codex_runtime"]; got != "app_server" {
		t.Fatalf("extra_body codex_runtime = %v, want normalized app_server", got)
	}

	// The dedicated setting wins over extra_body when both are present.
	extra, err = codexRuntimeExtraBody("exec", map[string]any{"codex_runtime": "app_server"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := extra["codex_runtime"]; got != "exec" {
		t.Fatalf("codex_runtime = %v, want exec (dedicated setting wins)", got)
	}
}
