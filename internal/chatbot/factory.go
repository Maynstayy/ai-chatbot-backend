package chatbot

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// ProviderConfig selects and configures the AI provider. It comes from
// AI_PROVIDER, AI_API_KEY, AI_MODEL, AI_BASE_URL, AI_TIMEOUT_SECONDS and
// AI_MAX_TOKENS.
type ProviderConfig struct {
	Provider  string
	APIKey    string
	Model     string
	BaseURL   string // Ollama server URL; unused by anthropic
	Timeout   time.Duration
	MaxTokens int
}

// NewProvider returns the configured provider, or (nil, nil) when the chatbot
// is switched off (AI_PROVIDER empty or "off", or anthropic without a key). Ollama runs
// locally and needs no key. Adding a vendor means a new case here plus one
// Provider implementation.
func NewProvider(cfg ProviderConfig) (Provider, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case "", "off", "none", "disabled":
		return nil, nil
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = 4096
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 25 * time.Second
	}
	switch cfg.Provider {
	case "anthropic", "claude":
		if cfg.APIKey == "" {
			return nil, nil
		}
		model := cfg.Model
		if model == "" {
			model = "claude-opus-5-5"
		}
		return newAnthropicProvider(cfg.APIKey, model, cfg.MaxTokens, cfg.Timeout), nil
	case "ollama":
		model := cfg.Model
		if model == "" {
			model = "qwen2.5:1.5b"
		}
		baseURL := strings.TrimSpace(cfg.BaseURL)
		if baseURL == "" {
			baseURL = "http://localhost:11434"
		}
		if u, err := url.Parse(baseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("AI_BASE_URL must be an http(s) URL such as http://localhost:11434")
		}
		return newOllamaProvider(baseURL, model, cfg.MaxTokens, cfg.Timeout), nil
	}
	return nil, fmt.Errorf("unsupported AI_PROVIDER %q (supported: anthropic, ollama)", cfg.Provider)
}
