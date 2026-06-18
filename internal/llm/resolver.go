package llm

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ResolvedEndpoint holds the resolved LLM endpoint configuration.
type ResolvedEndpoint struct {
	URL        string
	Token      string
	Model      string
	Protocol   string         // "anthropic", "openai", "codex", or "claude"
	AuthHeader string         // Anthropic auth header: "x-api-key" or "authorization"
	Source     string         // human-readable config source label
	ExtraBody  map[string]any // vendor-specific request body fields
}

// Environment variable names for OCR-specific configuration.
const (
	envOCRLLMURL        = "OCR_LLM_URL"
	envOCRLLMToken      = "OCR_LLM_TOKEN"
	envOCRLLMModel      = "OCR_LLM_MODEL"
	envOCRLLMAuthHeader = "OCR_LLM_AUTH_HEADER"
	envOCRUseAnthropic  = "OCR_USE_ANTHROPIC"
	envOCRLLMProtocol   = "OCR_LLM_PROTOCOL"
	envOCRCodexRuntime  = "OCR_CODEX_RUNTIME"
	envOCRClaudeRuntime = "OCR_CLAUDE_RUNTIME"
)

// Environment variable names from Claude Code configuration.
const (
	envCCBaseURL = "ANTHROPIC_BASE_URL"
	envCCToken   = "ANTHROPIC_AUTH_TOKEN"
	envCCModel   = "ANTHROPIC_MODEL"
)

// ResolveEndpoint reads from 4 strategy sources in priority order.
// Each strategy requires all three fields (URL, Token, Model) to be non-empty.
// Returns the first valid strategy's result.
func ResolveEndpoint(configPath string) (ResolvedEndpoint, error) {
	return ResolveEndpointWithModelOverride(configPath, "")
}

// ResolveEndpointWithModelOverride resolves an endpoint like ResolveEndpoint,
// but uses modelOverride as the request model when it is non-empty. The override
// can also supply the otherwise required model for a configured endpoint.
func ResolveEndpointWithModelOverride(configPath, modelOverride string) (ResolvedEndpoint, error) {
	modelOverride = strings.TrimSpace(modelOverride)

	strategies := []struct {
		name string
		fn   func() (ResolvedEndpoint, bool, error)
	}{
		{"OCR config file", func() (ResolvedEndpoint, bool, error) { return tryOCRConfig(configPath, modelOverride) }},
		{"OCR environment", func() (ResolvedEndpoint, bool, error) { return tryOCREnv(modelOverride) }},
		{"Claude Code environment", func() (ResolvedEndpoint, bool, error) { return tryCCEnv(modelOverride) }},
		{"Shell rc file", func() (ResolvedEndpoint, bool, error) { return tryShellRC(modelOverride) }},
	}

	// An explicit OCR_LLM_PROTOCOL is a deliberate per-invocation override
	// (e.g. CI pipelines); it must not be shadowed by a persistent config
	// file, so the environment strategy is promoted ahead of it. For codex
	// the env endpoint is complete by itself; for openai/anthropic the env
	// strategy itself enforces that URL/token/model are also provided.
	if strings.TrimSpace(os.Getenv(envOCRLLMProtocol)) != "" {
		strategies[0], strategies[1] = strategies[1], strategies[0]
	}

	for _, s := range strategies {
		ep, ok, err := s.fn()
		if err != nil {
			return ResolvedEndpoint{}, fmt.Errorf("resolve %s: %w", s.name, err)
		}
		if ok && endpointComplete(ep) {
			if ep.Source == "" {
				ep.Source = s.name
			}
			ep.Model = stripModelSuffix(ep.Model)
			return ep, nil
		}
	}

	return ResolvedEndpoint{}, fmt.Errorf("no valid LLM endpoint configured; set OCR_LLM_URL/OCR_LLM_TOKEN/OCR_LLM_MODEL, ~/.opencodereview/config.json, ANTHROPIC_BASE_URL/ANTHROPIC_AUTH_TOKEN/ANTHROPIC_MODEL, or OCR_LLM_PROTOCOL=codex/claude")
}

func endpointComplete(ep ResolvedEndpoint) bool {
	if ep.Protocol == "codex" || ep.Protocol == "claude" {
		return true
	}
	return ep.URL != "" && ep.Token != "" && ep.Model != ""
}

// tryOCREnv reads OCR-specific environment variables.
func tryOCREnv(modelOverride string) (ResolvedEndpoint, bool, error) {
	url := os.Getenv(envOCRLLMURL)
	token := os.Getenv(envOCRLLMToken)
	model := os.Getenv(envOCRLLMModel)
	protocol, err := normalizeProtocol(os.Getenv(envOCRLLMProtocol))
	if err != nil {
		return ResolvedEndpoint{}, false, fmt.Errorf("%s: %w", envOCRLLMProtocol, err)
	}
	if modelOverride != "" {
		model = modelOverride
	}
	if protocol == "codex" {
		extra, err := codexRuntimeExtraBody(os.Getenv(envOCRCodexRuntime), nil)
		if err != nil {
			return ResolvedEndpoint{}, false, fmt.Errorf("%s: %w", envOCRCodexRuntime, err)
		}
		return ResolvedEndpoint{Model: model, Protocol: "codex", Source: "OCR environment", ExtraBody: extra}, true, nil
	}
	if protocol == "claude" {
		extra, err := claudeRuntimeExtraBody(os.Getenv(envOCRClaudeRuntime), nil)
		if err != nil {
			return ResolvedEndpoint{}, false, fmt.Errorf("%s: %w", envOCRClaudeRuntime, err)
		}
		return ResolvedEndpoint{Model: model, Protocol: "claude", Source: "OCR environment", ExtraBody: extra}, true, nil
	}
	if url == "" || token == "" || model == "" {
		// An explicit API protocol is an override request that cannot be
		// satisfied without a full endpoint; silently falling through to the
		// config file would resolve a different protocol than the user asked
		// for, so fail fast instead.
		if protocol != "" {
			return ResolvedEndpoint{}, false, fmt.Errorf("%s=%s also requires %s, %s, and %s to be set", envOCRLLMProtocol, protocol, envOCRLLMURL, envOCRLLMToken, envOCRLLMModel)
		}
		return ResolvedEndpoint{}, false, nil
	}

	// An explicit protocol wins over the legacy use_anthropic toggle.
	if protocol == "" {
		useAnthropic := true // default true
		if v := os.Getenv(envOCRUseAnthropic); v != "" {
			lower := strings.ToLower(v)
			useAnthropic = lower == "true" || lower == "1" || lower == "yes"
		}
		protocol = "anthropic"
		if !useAnthropic {
			protocol = "openai"
		}
	}

	var authHeader string
	if protocol == "anthropic" {
		var err error
		authHeader, err = NormalizeAuthHeader(os.Getenv(envOCRLLMAuthHeader))
		if err != nil {
			return ResolvedEndpoint{}, false, fmt.Errorf("OCR environment: %w", err)
		}
		if authHeader == "" {
			authHeader = defaultAuthHeader(protocol)
		}
	}

	return ResolvedEndpoint{URL: url, Token: token, Model: model, Protocol: protocol, AuthHeader: authHeader, Source: "OCR environment"}, true, nil
}

// normalizeProtocol validates an explicit protocol selection. Empty means
// "not set" (fall back to legacy use_anthropic semantics).
func normalizeProtocol(raw string) (string, error) {
	protocol := strings.ToLower(strings.TrimSpace(raw))
	switch protocol {
	case "", "anthropic", "openai", "codex", "claude":
		return protocol, nil
	default:
		return "", fmt.Errorf("invalid protocol %q: must be 'anthropic', 'openai', 'codex', or 'claude'", raw)
	}
}

// llmFileConfig represents the llm section in config.json.
type llmFileConfig struct {
	URL           string         `json:"url,omitempty"`
	AuthToken     string         `json:"auth_token,omitempty"`
	AuthHeader    string         `json:"auth_header,omitempty"`
	Model         string         `json:"model,omitempty"`
	Protocol      string         `json:"protocol,omitempty"`
	CodexRuntime  string         `json:"codex_runtime,omitempty"`
	ClaudeRuntime string         `json:"claude_runtime,omitempty"`
	UseAnthropic  *bool          `json:"use_anthropic,omitempty"` // pointer to distinguish unset from false
	ExtraBody     map[string]any `json:"extra_body,omitempty"`
}

// providerEntryConfig represents a single provider entry in config.json.
type providerEntryConfig struct {
	APIKey     string         `json:"api_key,omitempty"`
	URL        string         `json:"url,omitempty"`
	Protocol   string         `json:"protocol,omitempty"`
	Model      string         `json:"model,omitempty"`
	Models     []string       `json:"models,omitempty"`
	AuthHeader string         `json:"auth_header,omitempty"`
	ExtraBody  map[string]any `json:"extra_body,omitempty"`
}

type configFile struct {
	Provider        string                         `json:"provider,omitempty"`
	Model           string                         `json:"model,omitempty"`
	Providers       map[string]providerEntryConfig `json:"providers,omitempty"`
	CustomProviders map[string]providerEntryConfig `json:"custom_providers,omitempty"`
	Llm             llmFileConfig                  `json:"llm,omitempty"`
}

// tryOCRConfig reads the OCR config file.
func tryOCRConfig(path, modelOverride string) (ResolvedEndpoint, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ResolvedEndpoint{}, false, nil
		}
		return ResolvedEndpoint{}, false, err
	}

	var cfg configFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		return ResolvedEndpoint{}, false, fmt.Errorf("parse config: %w", err)
	}

	if cfg.Provider != "" {
		return tryProviderConfig(cfg, modelOverride)
	}

	return tryLegacyLlmConfig(cfg, modelOverride)
}

// tryProviderConfig resolves an endpoint from the provider-based configuration.
func tryProviderConfig(cfg configFile, modelOverride string) (ResolvedEndpoint, bool, error) {
	preset, isPreset := LookupProvider(cfg.Provider)

	var entry providerEntryConfig
	var ok bool
	if isPreset {
		entry, ok = cfg.Providers[cfg.Provider]
	} else {
		entry, ok = cfg.CustomProviders[cfg.Provider]
	}
	if !ok {
		section := "providers"
		if !isPreset {
			section = "custom_providers"
		}
		return ResolvedEndpoint{}, false, fmt.Errorf("provider %q is set but not configured in %s section", cfg.Provider, section)
	}

	apiKey := entry.APIKey
	if apiKey == "" {
		if isPreset && preset.EnvVar != "" {
			apiKey = os.Getenv(preset.EnvVar)
		}
	}
	if apiKey == "" {
		return ResolvedEndpoint{}, false, fmt.Errorf("provider %q has no api_key configured and no environment variable fallback found", cfg.Provider)
	}

	var url, protocol, authHeader, model string
	var extraBody map[string]any

	if isPreset {
		url = preset.BaseURL
		protocol = preset.Protocol
		authHeader = preset.AuthHeader
		if entry.URL != "" {
			url = entry.URL
		}
		if entry.Protocol != "" {
			protocol = strings.ToLower(entry.Protocol)
		}
	} else {
		// Custom provider: url and protocol are required; model can come from cfg.Model.
		if entry.URL == "" || entry.Protocol == "" {
			return ResolvedEndpoint{}, false, fmt.Errorf("custom provider %q requires url and protocol fields", cfg.Provider)
		}
		if !strings.EqualFold(entry.Protocol, "anthropic") && !strings.EqualFold(entry.Protocol, "openai") {
			return ResolvedEndpoint{}, false, fmt.Errorf("custom provider %q has invalid protocol %q: must be \"anthropic\" or \"openai\"", cfg.Provider, entry.Protocol)
		}
		url = entry.URL
		protocol = strings.ToLower(entry.Protocol)
	}

	if cfg.Model != "" {
		model = cfg.Model
	}
	if entry.Model != "" {
		model = entry.Model
	}

	// Build available model list for validation.
	var availableModels []string
	if isPreset {
		availableModels = append(availableModels, preset.Models...)
	}
	availableModels = append(availableModels, entry.Models...)

	// Apply model override with validation.
	if modelOverride != "" {
		if len(availableModels) > 0 {
			if !modelListContains(availableModels, modelOverride) {
				return ResolvedEndpoint{}, false, fmt.Errorf(
					"model %q is not available for provider %q; available models: %s",
					modelOverride,
					cfg.Provider,
					strings.Join(availableModels, ", "),
				)
			}
		}
		model = modelOverride
	}

	if model == "" {
		return ResolvedEndpoint{}, false, fmt.Errorf("provider %q has no model configured; run 'ocr config model' to select one or pass --model", cfg.Provider)
	}

	if protocol == "anthropic" {
		var err error
		ah := "authorization"
		if isPreset && authHeader != "" {
			ah = authHeader
		}
		if entry.AuthHeader != "" {
			ah = entry.AuthHeader
		}
		authHeader, err = NormalizeAuthHeader(ah)
		if err != nil {
			return ResolvedEndpoint{}, false, fmt.Errorf("provider %q: %w", cfg.Provider, err)
		}
		if authHeader == "" {
			authHeader = defaultAuthHeader(protocol)
		}
	} else {
		authHeader = ""
	}

	extraBody = entry.ExtraBody

	if protocol == "anthropic" {
		url = ensureMessagesSuffix(url)
	}

	return ResolvedEndpoint{
		URL:        url,
		Token:      apiKey,
		Model:      model,
		Protocol:   protocol,
		AuthHeader: authHeader,
		Source:     "provider:" + cfg.Provider,
		ExtraBody:  extraBody,
	}, true, nil
}

// tryLegacyLlmConfig resolves an endpoint from the legacy llm config block,
// including the codex/claude CLI protocols.
func tryLegacyLlmConfig(cfg configFile, modelOverride string) (ResolvedEndpoint, bool, error) {
	protocol, err := normalizeProtocol(cfg.Llm.Protocol)
	if err != nil {
		return ResolvedEndpoint{}, false, fmt.Errorf("llm.protocol: %w", err)
	}

	model := cfg.Llm.Model
	if modelOverride != "" {
		model = modelOverride
	}

	if protocol == "codex" {
		extra, err := codexRuntimeExtraBody(cfg.Llm.CodexRuntime, cfg.Llm.ExtraBody)
		if err != nil {
			return ResolvedEndpoint{}, false, fmt.Errorf("llm.codex_runtime: %w", err)
		}
		return ResolvedEndpoint{Model: model, Protocol: "codex", Source: "OCR config file", ExtraBody: extra}, true, nil
	}
	if protocol == "claude" {
		extra, err := claudeRuntimeExtraBody(cfg.Llm.ClaudeRuntime, cfg.Llm.ExtraBody)
		if err != nil {
			return ResolvedEndpoint{}, false, fmt.Errorf("llm.claude_runtime: %w", err)
		}
		return ResolvedEndpoint{Model: model, Protocol: "claude", Source: "OCR config file", ExtraBody: extra}, true, nil
	}

	if cfg.Llm.URL == "" || cfg.Llm.AuthToken == "" || model == "" {
		// Same fail-fast contract as OCR_LLM_PROTOCOL: an explicit API
		// protocol cannot be satisfied without a full endpoint, and silently
		// falling through to Claude env / shell rc would route reviews to a
		// different provider than the config file requested.
		if protocol != "" {
			return ResolvedEndpoint{}, false, fmt.Errorf("llm.protocol=%s also requires llm.url, llm.auth_token, and llm.model to be set", protocol)
		}
		return ResolvedEndpoint{}, false, nil
	}

	// An explicit protocol wins over the legacy use_anthropic toggle.
	if protocol == "" {
		useAnthropic := true // default true
		if cfg.Llm.UseAnthropic != nil {
			useAnthropic = *cfg.Llm.UseAnthropic
		}
		protocol = "anthropic"
		if !useAnthropic {
			protocol = "openai"
		}
	}

	var authHeader string
	if protocol == "anthropic" {
		authHeader, err = NormalizeAuthHeader(cfg.Llm.AuthHeader)
		if err != nil {
			return ResolvedEndpoint{}, false, fmt.Errorf("OCR config file: %w", err)
		}
		if authHeader == "" {
			authHeader = defaultAuthHeader(protocol)
		}
	}

	return ResolvedEndpoint{URL: cfg.Llm.URL, Token: cfg.Llm.AuthToken, Model: model, Protocol: protocol, AuthHeader: authHeader, Source: "OCR config file", ExtraBody: cfg.Llm.ExtraBody}, true, nil
}

func codexRuntimeExtraBody(runtime string, base map[string]any) (map[string]any, error) {
	extra := make(map[string]any, len(base)+1)
	for k, v := range base {
		extra[k] = v
	}
	// A codex_runtime carried inside extra_body reaches CodexClient.runtime()
	// through the same key, so it must pass the same validation as the
	// dedicated setting. The dedicated setting wins when both are present.
	runtime = strings.TrimSpace(runtime)
	if runtime == "" {
		if fromExtra, ok := extra["codex_runtime"]; ok {
			s, isString := fromExtra.(string)
			if !isString {
				return nil, fmt.Errorf("invalid codex runtime %#v in extra_body: must be a string", fromExtra)
			}
			runtime = s
		}
	}
	switch normalized := strings.ToLower(strings.TrimSpace(runtime)); normalized {
	case "":
		delete(extra, "codex_runtime")
	case codexRuntimeExec:
		extra["codex_runtime"] = codexRuntimeExec
	case "app_server", "app-server", "appserver":
		extra["codex_runtime"] = codexRuntimeAppServer
	default:
		// A typo like "app_servr" would otherwise be stored verbatim and the
		// client would silently select the exec runtime.
		return nil, fmt.Errorf("invalid codex runtime %q: must be 'exec' or 'app_server'", runtime)
	}
	return extra, nil
}

func claudeRuntimeExtraBody(runtime string, base map[string]any) (map[string]any, error) {
	extra := make(map[string]any, len(base)+1)
	for k, v := range base {
		extra[k] = v
	}
	// A claude_runtime carried inside extra_body reaches ClaudeClient.runtime()
	// through the same key, so it must pass the same validation as the
	// dedicated setting. The dedicated setting wins when both are present.
	runtime = strings.TrimSpace(runtime)
	if runtime == "" {
		if fromExtra, ok := extra["claude_runtime"]; ok {
			s, isString := fromExtra.(string)
			if !isString {
				return nil, fmt.Errorf("invalid claude runtime %#v in extra_body: must be a string", fromExtra)
			}
			runtime = s
		}
	}
	switch normalized := strings.ToLower(strings.TrimSpace(runtime)); normalized {
	case "":
		delete(extra, "claude_runtime")
	case claudeRuntimeExec:
		extra["claude_runtime"] = claudeRuntimeExec
	case "app_server", "app-server", "appserver":
		extra["claude_runtime"] = claudeRuntimeAppServer
	default:
		return nil, fmt.Errorf("invalid claude runtime %q: must be 'exec' or 'app_server'", runtime)
	}
	return extra, nil
}

// tryCCEnv reads Claude Code environment variables.
func tryCCEnv(modelOverride string) (ResolvedEndpoint, bool, error) {
	baseURL := os.Getenv(envCCBaseURL)
	token := os.Getenv(envCCToken)
	model := os.Getenv(envCCModel)
	if modelOverride != "" {
		model = modelOverride
	}
	if baseURL == "" || token == "" || model == "" {
		return ResolvedEndpoint{}, false, nil
	}

	url := ensureMessagesSuffix(baseURL)

	// Claude Code environment tokens are OAuth/Bearer-style credentials.
	return ResolvedEndpoint{URL: url, Token: token, Model: model, Protocol: "anthropic", AuthHeader: "authorization", Source: "Claude Code environment"}, true, nil
}

// tryShellRC parses ~/.zshrc and ~/.bashrc for ANTHROPIC_* exports.
func tryShellRC(modelOverride string) (ResolvedEndpoint, bool, error) {
	files := shellRCFiles()
	for _, f := range files {
		ep, ok, err := parseShellRC(f, modelOverride)
		if err != nil || ok {
			return ep, ok, err
		}
	}
	return ResolvedEndpoint{}, false, nil
}

func shellRCFiles() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	candidates := []string{
		filepath.Join(home, ".zshrc"),
		filepath.Join(home, ".bashrc"),
		filepath.Join(home, ".bash_profile"),
		filepath.Join(home, ".profile"),
	}
	var valid []string
	for _, f := range candidates {
		if _, err := os.Stat(f); err == nil {
			valid = append(valid, f)
		}
	}
	return valid
}

var exportRe = regexp.MustCompile(`^export\s+(ANTHROPIC_\w+)\s*=\s*(?:"([^"]*)"|'([^']*)'|(.+))\s*$`)

var modelSuffixRe = regexp.MustCompile(`\[\d+m\]$`)

func stripModelSuffix(model string) string {
	return modelSuffixRe.ReplaceAllString(model, "")
}

func parseShellRC(path, modelOverride string) (ResolvedEndpoint, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ResolvedEndpoint{}, false, nil
	}

	var baseURL, token, model string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		matches := exportRe.FindStringSubmatch(line)
		if matches == nil {
			continue
		}
		key := matches[1]
		value := matches[2]
		if value == "" {
			value = matches[3]
		}
		if value == "" {
			value = matches[4]
		}
		value = strings.TrimSpace(value)

		switch key {
		case "ANTHROPIC_BASE_URL":
			baseURL = value
		case "ANTHROPIC_AUTH_TOKEN":
			token = value
		case "ANTHROPIC_MODEL":
			model = value
		}
	}
	if modelOverride != "" {
		model = modelOverride
	}

	if baseURL == "" || token == "" || model == "" {
		return ResolvedEndpoint{}, false, nil
	}

	url := ensureMessagesSuffix(baseURL)

	// Claude Code shell rc tokens are OAuth/Bearer-style credentials.
	return ResolvedEndpoint{URL: url, Token: token, Model: model, Protocol: "anthropic", AuthHeader: "authorization", Source: "Shell rc file"}, true, nil
}

func defaultAuthHeader(protocol string) string {
	// auth_header is Anthropic-only; OpenAI-compatible clients keep API key auth.
	if protocol == "anthropic" {
		return "authorization"
	}
	return ""
}

// modelListContains checks if a model exists in the available models list.
func modelListContains(models []string, target string) bool {
	target = strings.TrimSpace(target)
	for _, model := range models {
		if strings.TrimSpace(model) == target {
			return true
		}
	}
	return false
}

// NormalizeAuthHeader normalizes an auth header value to a canonical form.
// It returns an error for unrecognized values.
func NormalizeAuthHeader(header string) (string, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return "", nil
	}
	switch strings.ToLower(header) {
	case "x-api-key":
		return "x-api-key", nil
	case "authorization", "bearer":
		return "authorization", nil
	default:
		return "", fmt.Errorf("unsupported auth_header value %q; expected \"x-api-key\" or \"authorization\"", header)
	}
}

// ensureMessagesSuffix appends /v1/messages to base URLs that lack a versioned path.
func ensureMessagesSuffix(rawURL string) string {
	u := strings.TrimRight(rawURL, "/")
	if strings.Contains(u, "/v1/") {
		// Already has versioned path — don't modify.
		return rawURL
	}
	return u + "/v1/messages"
}
