package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config holds all application configuration.
type Config struct {
	Telegram   TelegramConfig   `yaml:"telegram"`
	Webhook    WebhookConfig    `yaml:"webhook"`
	LLM        LLMConfig        `yaml:"llm"`
	MCP        MCPConfig        `yaml:"mcp"`
	Providers  ProvidersConfig  `yaml:"providers"`
	Playbooks  PlaybookRules    `yaml:"playbooks"`
	Redaction  RedactionRules   `yaml:"redaction"`
}

// LLMConfig configures the LLM summarizer backend.
type LLMConfig struct {
	Enabled bool   `yaml:"enabled"`
	Backend string `yaml:"backend"`  // ollama, gemini, openrouter, openai, or custom
	BaseURL string `yaml:"base_url"` // auto-set per backend if empty
	APIKey  string `yaml:"api_key"`  // not needed for ollama
	Model   string `yaml:"model"`    // auto-set per backend if empty
}

// ProvidersConfig configures data source endpoints.
type ProvidersConfig struct {
	VictoriaMetricsURL string `yaml:"victoria_metrics_url"`
	VictoriaLogsURL    string `yaml:"victoria_logs_url"`
}

type TelegramConfig struct {
	Token          string   `yaml:"token"`
	AllowedChatIDs []int64  `yaml:"allowed_chat_ids,omitempty"`
	AlertChatIDs   []int64  `yaml:"alert_chat_ids,omitempty"` // chats to receive alert notifications
}

type WebhookConfig struct {
	Enabled     bool   `yaml:"enabled"`
	Addr        string `yaml:"addr"`         // e.g. ":8080"
	BearerToken string `yaml:"bearer_token"` // optional auth for incoming webhooks
	ClusterName string `yaml:"cluster_name"` // shown in alert header
	BotName     string `yaml:"bot_name"`     // shown in alert header, e.g. "lazy-diagnose-k8s"
}

type MCPConfig struct {
	Kubernetes MCPServerConfig `yaml:"kubernetes"`
	Logs       MCPServerConfig `yaml:"logs"`
	Metrics    MCPServerConfig `yaml:"metrics"`
}

type MCPServerConfig struct {
	Command string   `yaml:"command"`
	Args    []string `yaml:"args,omitempty"`
	Env     map[string]string `yaml:"env,omitempty"`
	Timeout int      `yaml:"timeout"` // seconds
}

// PlaybookRules holds scoring configuration for each playbook.
type PlaybookRules struct {
	CrashLoop         []HypothesisRule `yaml:"crashloop"`
	Pending           []HypothesisRule `yaml:"pending"`
	RolloutRegression []HypothesisRule `yaml:"rollout_regression"`
}

type HypothesisRule struct {
	ID       string        `yaml:"id"`
	Name     string        `yaml:"name"`
	Signals  []SignalRule  `yaml:"signals"`
}

type SignalRule struct {
	Name      string `yaml:"name"`
	Match     string `yaml:"match"`     // what to look for
	Source    string `yaml:"source"`     // k8s, logs, metrics
	Weight    int    `yaml:"weight"`
}

type RedactionRules struct {
	Patterns []RedactionPattern `yaml:"patterns"`
}

type RedactionPattern struct {
	Name    string `yaml:"name"`
	Pattern string `yaml:"pattern"` // regex
	Replace string `yaml:"replace"`
}

// LoadConfig loads the main app config from a YAML file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	return &cfg, nil
}

// LoadPlaybookRules loads playbook scoring rules from a YAML file.
func LoadPlaybookRules(path string) (*PlaybookRules, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read playbook rules %s: %w", path, err)
	}
	var pr PlaybookRules
	if err := yaml.Unmarshal(data, &pr); err != nil {
		return nil, fmt.Errorf("parse playbook rules %s: %w", path, err)
	}
	return &pr, nil
}
