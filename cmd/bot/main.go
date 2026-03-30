package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
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

	// Override alert chat IDs from env (comma-separated)
	if chatIDs := os.Getenv("TELEGRAM_CHAT_ID"); chatIDs != "" {
		var ids []int64
		for _, s := range strings.Split(chatIDs, ",") {
			s = strings.TrimSpace(s)
			if id, err := strconv.ParseInt(s, 10, 64); err == nil {
				ids = append(ids, id)
			}
		}
		cfg.Telegram.AlertChatIDs = ids
	}

	if cfg.Telegram.Token == "" {
		logger.Error("TELEGRAM_BOT_TOKEN is required")
		os.Exit(1)
	}

	// Build components
	targetResolver := resolver.New()

	// LLM Summarizer (optional — shared across clusters)
	var summarizer *diagnosis.Summarizer
	llmCfg := resolveLLMConfig(cfg)
	if llmCfg.Backend != "" {
		summarizer = diagnosis.NewSummarizer(diagnosis.SummarizerConfig{
			Backend: llmCfg.Backend,
			BaseURL: llmCfg.BaseURL,
			APIKey:  llmCfg.APIKey,
			Model:   llmCfg.Model,
		})
		logger.Info("LLM summarizer enabled", "backend", summarizer.Backend(), "model", summarizer.ModelName())
	} else {
		logger.Info("LLM summarizer disabled (configure llm section in config.yaml or set LLM_BACKEND env var)")
	}

	// Victoria endpoints (shared across clusters)
	vmURL := firstNonEmpty(os.Getenv("VICTORIA_METRICS_URL"), cfg.Providers.VictoriaMetricsURL, "http://localhost:8428")
	vlURL := firstNonEmpty(os.Getenv("VICTORIA_LOGS_URL"), cfg.Providers.VictoriaLogsURL, "http://localhost:9428")

	defaultNs := envOr("DEFAULT_NAMESPACE", "prod")

	// Build per-cluster components
	clusters := make(map[string]*telegram.ClusterEntry)
	var defaultCluster string
	var firstK8s *k8sprovider.Provider // for single-cluster backwards compat

	if len(cfg.Clusters) > 0 {
		// Multi-cluster mode: build from config
		kubeconfigDefault := envOr("KUBECONFIG", filepath.Join(homeDir(), ".kube", "config"))

		for _, cc := range cfg.Clusters {
			kubeconfigPath := cc.Kubeconfig
			if kubeconfigPath == "" {
				kubeconfigPath = kubeconfigDefault
			}

			k8s, err := k8sprovider.NewFromContext(kubeconfigPath, cc.Context)
			if err != nil {
				logger.Warn("K8s provider unavailable for cluster", "cluster", cc.Name, "context", cc.Context, "error", err)
				continue
			}

			collector := &provider.Collector{
				K8s:     k8s,
				Metrics: metricsprovider.NewWithCluster(vmURL, cc.Name),
				Logs:    logsprovider.NewWithCluster(vlURL, cc.Name),
			}

			engine := playbook.New(collector, summarizer, logger)
			clusters[cc.Name] = &telegram.ClusterEntry{
				Name:    cc.Name,
				Engine:  engine,
				Scanner: k8s,
			}

			if cc.Default {
				defaultCluster = cc.Name
			}
			if firstK8s == nil {
				firstK8s = k8s
			}

			logger.Info("cluster initialized", "name", cc.Name, "context", cc.Context)
		}

		if defaultCluster == "" && len(clusters) > 0 {
			// Pick first cluster as default
			for name := range clusters {
				defaultCluster = name
				break
			}
		}
	}

	// Fallback: single-cluster mode (no clusters in config)
	if len(clusters) == 0 {
		k8s, err := initK8sProvider(logger)
		if err != nil {
			logger.Warn("K8s provider unavailable, diagnosis will lack K8s data", "error", err)
		} else {
			logger.Info("K8s provider initialized (single-cluster mode)")
			firstK8s = k8s
		}

		collector := &provider.Collector{
			K8s:     firstK8s,
			Metrics: metricsprovider.New(vmURL),
			Logs:    logsprovider.New(vlURL),
		}

		// Use webhook.cluster_name as cluster key so alert callbacks match
		clusterName := cfg.Webhook.ClusterName
		if clusterName == "" {
			clusterName = "default"
		}

		engine := playbook.New(collector, summarizer, logger)
		clusters[clusterName] = &telegram.ClusterEntry{
			Name:    clusterName,
			Engine:  engine,
			Scanner: firstK8s,
		}
		defaultCluster = clusterName
	}

	// Create Telegram bot (single-cluster compat via first cluster)
	firstEntry := clusters[defaultCluster]
	bot, err := telegram.NewBot(
		cfg.Telegram.Token,
		firstEntry.Engine,
		targetResolver,
		firstEntry.Scanner,
		defaultNs,
		cfg.Telegram.AllowedChatIDs,
		cfg.Telegram.AlertChatIDs,
		webhook.AlertFormatConfig{
			BotName:     cfg.Webhook.BotName,
			ClusterName: cfg.Webhook.ClusterName,
		},
		cfg.Telegram.RateLimit,
		logger,
	)
	if err != nil {
		logger.Error("failed to create bot", "error", err)
		os.Exit(1)
	}

	// Register all clusters (overrides the single "default" entry from NewBot)
	bot.SetClusters(clusters, defaultCluster)
	logger.Info("bot configured", "clusters", len(clusters), "default", defaultCluster)

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
