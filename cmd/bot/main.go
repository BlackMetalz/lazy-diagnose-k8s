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
	vmURL := envOr("VICTORIA_METRICS_URL", "http://localhost:8428")
	collector.Metrics = metricsprovider.New(vmURL)
	logger.Info("metrics provider initialized", "url", vmURL)

	// Logs provider (VictoriaLogs)
	vlURL := envOr("VICTORIA_LOGS_URL", "http://localhost:9428")
	collector.Logs = logsprovider.New(vlURL)
	logger.Info("logs provider initialized", "url", vlURL)

	diagEngine := diagnosis.New(playbookRules).WithLogger(logger)

	// LLM Summarizer (optional — needs ANTHROPIC_API_KEY)
	anthropicKey := os.Getenv("ANTHROPIC_API_KEY")
	if anthropicKey != "" {
		summarizer := diagnosis.NewSummarizer(anthropicKey, "claude-haiku-4-5")
		diagEngine.WithSummarizer(summarizer)
		logger.Info("LLM summarizer enabled (claude-haiku-4-5)")
	} else {
		logger.Info("LLM summarizer disabled (set ANTHROPIC_API_KEY to enable)")
	}

	playbookEngine := playbook.New(collector, diagEngine)

	// Create and run bot
	bot, err := telegram.NewBot(
		cfg.Telegram.Token,
		playbookEngine,
		targetResolver,
		cfg.Telegram.AllowedChatIDs,
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
