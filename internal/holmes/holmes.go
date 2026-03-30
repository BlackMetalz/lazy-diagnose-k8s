package holmes

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/lazy-diagnose-k8s/internal/config"
)

// Client wraps the HolmesGPT CLI for deep investigation.
type Client struct {
	model   string
	baseURL string
	apiKey  string
	timeout time.Duration
}

// New creates a HolmesGPT client from config.
func New(cfg config.HolmesConfig) *Client {
	timeout := time.Duration(cfg.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &Client{
		model:   cfg.Model,
		baseURL: cfg.BaseURL,
		apiKey:  cfg.APIKey,
		timeout: timeout,
	}
}

// Investigate runs a deep investigation using the HolmesGPT CLI.
// Returns the investigation result as text.
func (c *Client) Investigate(ctx context.Context, question string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	args := []string{"ask", question, "--no-interactive"}
	if c.model != "" {
		args = append(args, "--model", c.model)
	}

	cmd := exec.CommandContext(ctx, "holmes", args...)

	// Set env vars for OpenAI-compatible endpoint
	cmd.Env = append(cmd.Environ(),
		fmt.Sprintf("OPENAI_API_KEY=%s", c.apiKey),
	)
	if c.baseURL != "" {
		cmd.Env = append(cmd.Env, fmt.Sprintf("OPENAI_API_BASE=%s", c.baseURL))
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	slog.Info("holmes investigation started", "question", question, "model", c.model)

	err := cmd.Run()
	duration := time.Since(start).Round(time.Millisecond)

	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			slog.Warn("holmes investigation timed out", "timeout", c.timeout, "duration", duration)
			return "", fmt.Errorf("investigation timed out after %s", c.timeout)
		}
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg == "" {
			errMsg = err.Error()
		}
		slog.Error("holmes investigation failed", "error", errMsg, "duration", duration)
		return "", fmt.Errorf("holmes: %s", errMsg)
	}

	result := cleanOutput(stdout.String())
	slog.Info("holmes investigation complete", "duration", duration, "result_len", len(result))

	if result == "" {
		return "", fmt.Errorf("holmes returned empty result")
	}

	return result, nil
}

// cleanOutput strips holmes CLI progress/setup noise from output,
// keeping only the actual investigation result.
func cleanOutput(raw string) string {
	var lines []string
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		// Skip toolset status lines
		if strings.HasPrefix(trimmed, "✅ Toolset") || strings.HasPrefix(trimmed, "❌ Toolset") {
			continue
		}
		// Skip setup/progress lines
		if strings.HasPrefix(trimmed, "Loaded models:") ||
			strings.HasPrefix(trimmed, "Refreshing available") ||
			strings.HasPrefix(trimmed, "Toolset statuses are cached") ||
			strings.HasPrefix(trimmed, "Using selected model:") ||
			strings.HasPrefix(trimmed, "User:") {
			continue
		}
		lines = append(lines, line)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// Available checks if the holmes CLI is installed.
func Available() bool {
	_, err := exec.LookPath("holmes")
	return err == nil
}
