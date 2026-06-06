package main

import "testing"

func TestSetConfigValueSupportsCodexProtocol(t *testing.T) {
	cfg := &Config{}
	if err := setConfigValue(cfg, "llm.protocol", "codex"); err != nil {
		t.Fatalf("setConfigValue returned error: %v", err)
	}
	if cfg.Llm.Protocol != "codex" {
		t.Fatalf("protocol = %q, want codex", cfg.Llm.Protocol)
	}
}

func TestSetConfigValueSupportsCodexRuntime(t *testing.T) {
	cfg := &Config{}
	if err := setConfigValue(cfg, "llm.codex_runtime", "app_server"); err != nil {
		t.Fatalf("setConfigValue returned error: %v", err)
	}
	if cfg.Llm.CodexRuntime != "app_server" {
		t.Fatalf("codex_runtime = %q, want app_server", cfg.Llm.CodexRuntime)
	}
}

func TestSetConfigValueRejectsInvalidProtocol(t *testing.T) {
	cfg := &Config{}
	if err := setConfigValue(cfg, "llm.protocol", "gemini"); err == nil {
		t.Fatalf("expected error for invalid llm.protocol value, got nil")
	}
}

func TestSetConfigValueNormalizesProtocolCase(t *testing.T) {
	cfg := &Config{}
	if err := setConfigValue(cfg, "llm.protocol", " OpenAI "); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Llm.Protocol != "openai" {
		t.Fatalf("protocol = %q, want openai", cfg.Llm.Protocol)
	}
}

func TestSetConfigValueRejectsInvalidCodexRuntime(t *testing.T) {
	cfg := &Config{}
	if err := setConfigValue(cfg, "llm.codex_runtime", "websocket"); err == nil {
		t.Fatalf("expected error for invalid llm.codex_runtime value, got nil")
	}
}
