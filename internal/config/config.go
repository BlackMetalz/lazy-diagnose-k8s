package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config holds all application configuration.
type Config struct {
	Telegram   TelegramConfig   `yaml:"telegram"`
	MCP        MCPConfig        `yaml:"mcp"`
	ServiceMap ServiceMap       `yaml:"service_map"`
	Playbooks  PlaybookRules    `yaml:"playbooks"`
	Redaction  RedactionRules   `yaml:"redaction"`
}

type TelegramConfig struct {
	Token          string   `yaml:"token"`
	AllowedChatIDs []int64  `yaml:"allowed_chat_ids,omitempty"`
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

// ServiceMap maps logical service names to K8s resources.
type ServiceMap struct {
	Services []ServiceEntry `yaml:"services"`
}

type ServiceEntry struct {
	Name            string            `yaml:"name"`
	Aliases         []string          `yaml:"aliases,omitempty"`
	Namespace       string            `yaml:"namespace"`
	PrimaryResource string            `yaml:"primary_resource"` // e.g. "deployment/checkout"
	Selectors       map[string]string `yaml:"selectors,omitempty"`
	MetricsJob      string            `yaml:"metrics_job,omitempty"`
	RolloutTarget   string            `yaml:"rollout_target,omitempty"`
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

// Lookup finds a service entry by name or alias.
func (sm *ServiceMap) Lookup(name string) *ServiceEntry {
	for i := range sm.Services {
		s := &sm.Services[i]
		if s.Name == name {
			return s
		}
		for _, alias := range s.Aliases {
			if alias == name {
				return s
			}
		}
	}
	return nil
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

// LoadServiceMap loads service map from a YAML file.
func LoadServiceMap(path string) (*ServiceMap, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read service map %s: %w", path, err)
	}
	var sm ServiceMap
	if err := yaml.Unmarshal(data, &sm); err != nil {
		return nil, fmt.Errorf("parse service map %s: %w", path, err)
	}
	return &sm, nil
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
