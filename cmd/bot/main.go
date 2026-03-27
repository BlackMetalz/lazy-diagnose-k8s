package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/lazy-diagnose-k8s/internal/adapter/telegram"
	"github.com/lazy-diagnose-k8s/internal/config"
	"github.com/lazy-diagnose-k8s/internal/diagnosis"
	"github.com/lazy-diagnose-k8s/internal/playbook"
	"github.com/lazy-diagnose-k8s/internal/provider"
	k8sprovider "github.com/lazy-diagnose-k8s/internal/provider/kubernetes"
	logsprovider "github.com/lazy-diagnose-k8s/internal/provider/logs"
	metricsprovider "github.com/lazy-diagnose-k8s/internal/provider/metrics"
	"github.com/lazy-diagnose-k8s/internal/resolver"
	"github.com/lazy-diagnose-k8s/internal/webhook"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Load config
	configPath := envOr("CONFIG_PATH", "configs/config.yaml")
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		logger.Error("failed to load config", "error", err, "path", configPath)
		os.Exit(1)
	}

	// Override telegram token from env if set
	if token := os.Getenv("TELEGRAM_BOT_TOKEN"); token != "" {
		cfg.Telegram.Token = token
	}

	if cfg.Telegram.Token == "" {
		logger.Error("TELEGRAM_BOT_TOKEN is required")
		os.Exit(1)
	}

	// Load service map
	serviceMapPath := envOr("SERVICE_MAP_PATH", "configs/service_map.yaml")
	serviceMap, err := config.LoadServiceMap(serviceMapPath)
	if err != nil {
		logger.Warn("failed to load service map, using empty", "error", err)
		serviceMap = &config.ServiceMap{}
	}

	// Load playbook rules
	playbookPath := envOr("PLAYBOOK_RULES_PATH", "configs/playbook_rules.yaml")
	playbookRules, err := config.LoadPlaybookRules(playbookPath)
	if err != nil {
		logger.Warn("failed to load playbook rules, using empty", "error", err)
		playbookRules = &config.PlaybookRules{}
	}

	// Build components
	targetResolver := resolver.New(serviceMap)

	// Build providers
	collector := &provider.Collector{}

	// K8s provider
	k8s, err := initK8sProvider(logger)
	if err != nil {
		logger.Warn("K8s provider unavailable, diagnosis will lack K8s data", "error", err)
	} else {
		collector.K8s = k8s
		logger.Info("K8s provider initialized")
	}

	// Metrics provider (VictoriaMetrics)
	// Config file → env var override → default
	vmURL := firstNonEmpty(os.Getenv("VICTORIA_METRICS_URL"), cfg.Providers.VictoriaMetricsURL, "http://localhost:8428")
	collector.Metrics = metricsprovider.New(vmURL)
	logger.Info("metrics provider initialized", "url", vmURL)

	// Logs provider (VictoriaLogs)
	vlURL := firstNonEmpty(os.Getenv("VICTORIA_LOGS_URL"), cfg.Providers.VictoriaLogsURL, "http://localhost:9428")
	collector.Logs = logsprovider.New(vlURL)
	logger.Info("logs provider initialized", "url", vlURL)

	diagEngine := diagnosis.New(playbookRules).WithLogger(logger)

	// LLM Summarizer (optional)
	// Priority: env var > config file > disabled
	llmCfg := resolveLLMConfig(cfg)
	if llmCfg.Backend != "" {
		summarizer := diagnosis.NewSummarizer(diagnosis.SummarizerConfig{
			Backend: llmCfg.Backend,
			BaseURL: llmCfg.BaseURL,
			APIKey:  llmCfg.APIKey,
			Model:   llmCfg.Model,
		})
		diagEngine.WithSummarizer(summarizer)
		logger.Info("LLM summarizer enabled", "backend", summarizer.Backend(), "model", summarizer.ModelName())
	} else {
		logger.Info("LLM summarizer disabled (configure llm section in config.yaml or set LLM_BACKEND env var)")
	}

	playbookEngine := playbook.New(collector, diagEngine)

	// Create Telegram bot
	defaultNs := envOr("DEFAULT_NAMESPACE", "prod")
	bot, err := telegram.NewBot(
		cfg.Telegram.Token,
		playbookEngine,
		targetResolver,
		k8s, // scanner (can be nil if K8s unavailable)
		defaultNs,
		cfg.Telegram.AllowedChatIDs,
		cfg.Telegram.AlertChatIDs,
		webhook.AlertFormatConfig{
			BotName:     cfg.Webhook.BotName,
			ClusterName: cfg.Webhook.ClusterName,
		},
		logger,
	)
	if err != nil {
		logger.Error("failed to create bot", "error", err)
		os.Exit(1)
	}

	// Graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		logger.Info("received signal, shutting down", "signal", sig)
		cancel()
	}()

	// Start webhook server (if enabled)
	if cfg.Webhook.Enabled {
		webhookAddr := cfg.Webhook.Addr
		if webhookAddr == "" {
			webhookAddr = ":8080"
		}
		webhookServer := webhook.NewServer(webhookAddr, cfg.Webhook.BearerToken, bot.HandleAlert, logger)
		go func() {
			if err := webhookServer.Run(ctx); err != nil {
				logger.Error("webhook server stopped", "error", err)
			}
		}()
		logger.Info("webhook server enabled", "addr", webhookAddr)
	}

	// Start Telegram bot (blocking)
	logger.Info("starting lazy-diagnose-k8s bot")
	if err := bot.Run(ctx); err != nil && err != context.Canceled {
		logger.Error("bot stopped with error", "error", err)
		os.Exit(1)
	}
}

func initK8sProvider(logger *slog.Logger) (*k8sprovider.Provider, error) {
	// Try in-cluster first
	p, err := k8sprovider.NewInCluster()
	if err == nil {
		return p, nil
	}

	// Fallback to kubeconfig
	kubeconfig := envOr("KUBECONFIG", filepath.Join(homeDir(), ".kube", "config"))
	p, err = k8sprovider.NewFromKubeconfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	logger.Info("using kubeconfig", "path", kubeconfig)
	return p, nil
}

// resolveLLMConfig merges config file + env var overrides.
// Env vars take priority over config file values.
func resolveLLMConfig(cfg *config.Config) config.LLMConfig {
	result := cfg.LLM

	// Env vars override config file
	if v := os.Getenv("LLM_BACKEND"); v != "" {
		result.Backend = v
		result.Enabled = true
	}
	if v := os.Getenv("LLM_BASE_URL"); v != "" {
		result.BaseURL = v
	}
	if v := os.Getenv("LLM_API_KEY"); v != "" {
		result.APIKey = v
	}
	if v := os.Getenv("LLM_MODEL"); v != "" {
		result.Model = v
	}

	// enabled: true in config but no backend → ignore
	if result.Enabled && result.Backend == "" {
		result.Backend = ""
	}
	// backend set → implicitly enabled
	if result.Backend != "" {
		result.Enabled = true
	}

	return result
}

// firstNonEmpty returns the first non-empty string from the arguments.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return os.Getenv("USERPROFILE")
}
