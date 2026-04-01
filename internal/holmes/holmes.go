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

	// Holmes CLI (litellm) requires "openai/" prefix for OpenAI-compatible endpoints.
	// Auto-add it so users just set the model name (e.g. "gpt-oss-120b").
	model := cfg.Model
	if model != "" && !strings.Contains(model, "/") {
		model = "openai/" + model
	}

	return &Client{
		model:   model,
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

	args := []string{"ask", question, "--no-interactive", "--max-steps", "15", "--fast-mode"}
	if c.model != "" {
		args = append(args, "--model", c.model)
	}

	cmd := exec.CommandContext(ctx, "holmes", args...)

	// Set env vars for OpenAI-compatible endpoint
	cmd.Env = append(cmd.Environ(),
		fmt.Sprintf("OPENAI_API_KEY=%s", c.apiKey),
		// Suppress litellm warnings for unknown models
		"OVERRIDE_MAX_CONTENT_SIZE=128000",
		"OVERRIDE_MAX_OUTPUT_TOKEN=8192",
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
			slog.Warn("holmes investigation timed out", "timeout", c.timeout, "duration", fmt.Sprintf("%.1fs", duration.Seconds()))
			return "", fmt.Errorf("investigation timed out after %s", c.timeout)
		}
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg == "" {
			errMsg = err.Error()
		}
		slog.Error("holmes investigation failed", "error", errMsg, "duration", fmt.Sprintf("%.1fs", duration.Seconds()))
		return "", fmt.Errorf("holmes: %s", errMsg)
	}

	rawOutput := stdout.String()
	tools := extractToolCalls(rawOutput)
	result := cleanOutput(rawOutput)
	slog.Info("deep investigation complete",
		"model", c.model,
		"duration", fmt.Sprintf("%.1fs", duration.Seconds()),
		"tool_calls", len(tools),
		"tools", tools,
		"raw_len", len(rawOutput),
		"result_len", len(result),
	)

	if result == "" {
		return "", fmt.Errorf("holmes returned empty result")
	}

	return result, nil
}

// cleanOutput extracts the final investigation result from holmes CLI output.
// Holmes outputs setup noise, tool calls, reasoning blocks, and finally
// the actual result in the last "AI:" block. We extract only that.
func cleanOutput(raw string) string {
	// Strategy: split by "AI:" blocks, take the last meaningful one.
	// The last AI block is typically the conclusion/summary.
	blocks := splitAIBlocks(raw)
	if len(blocks) == 0 {
		// Fallback: filter line by line
		return filterNoise(raw)
	}

	// Take the last block — that's the conclusion
	result := filterNoise(blocks[len(blocks)-1])
	if result == "" && len(blocks) > 1 {
		// If last block was empty after filtering, try second to last
		result = filterNoise(blocks[len(blocks)-2])
	}
	return result
}

// splitAIBlocks splits holmes output into sections starting with "AI:".
// Returns content of each AI block (without the "AI:" prefix).
func splitAIBlocks(raw string) []string {
	var blocks []string
	var current []string
	inBlock := false

	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "AI:") || strings.HasPrefix(trimmed, "AI reasoning:") {
			if inBlock && len(current) > 0 {
				blocks = append(blocks, strings.Join(current, "\n"))
			}
			// Start new block, include text after "AI:" prefix
			after := strings.TrimPrefix(trimmed, "AI reasoning:")
			after = strings.TrimPrefix(after, "AI:")
			after = strings.TrimSpace(after)
			current = nil
			if after != "" {
				current = append(current, after)
			}
			inBlock = true
			continue
		}

		if inBlock {
			current = append(current, line)
		}
	}
	if inBlock && len(current) > 0 {
		blocks = append(blocks, strings.Join(current, "\n"))
	}
	return blocks
}

// filterNoise removes holmes/litellm internal lines from text.
func filterNoise(raw string) string {
	var lines []string
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if isNoiseLine(trimmed) {
			continue
		}
		lines = append(lines, line)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// isNoiseLine returns true for lines that are holmes internal noise.
func isNoiseLine(s string) bool {
	if s == "" {
		return false
	}

	noisePrefix := []string{
		// litellm setup
		"Loaded models:", "Refreshing available", "Toolset statuses are cached",
		"Using selected model:", "Using model:", "Couldn't find model",
		// holmes tool execution
		"Running tool #", "Executing bash command:", "Finished #",
		"The AI requested", "User:",
		// task table
		"Task List:", "+----+", "| ID |", "| id |",
		// toolset status
		"✅", "❌",
	}
	for _, p := range noisePrefix {
		if strings.HasPrefix(s, p) {
			return true
		}
	}

	noiseContains := []string{
		"was not set", "returned 1",
		"OVERRIDE_MAX_", "ENABLE_INSPEKTOR",
		"max_input_tokens", "max_output_tokens", "To override, set",
		"/show ", "to view contents",
		"tool call(s)",
	}
	for _, c := range noiseContains {
		if strings.Contains(s, c) {
			return true
		}
	}

	// Task table rows: | 1  | Check deployment... | [~] in_progress |
	if strings.HasPrefix(s, "|") && strings.HasSuffix(s, "|") {
		return true
	}

	// Short fragments from split warnings (e.g. "required.", "set")
	if len(s) < 15 && !strings.Contains(s, " ") {
		return true
	}

	return false
}

// extractToolCalls extracts tool call descriptions from Holmes output.
// Holmes outputs lines like "Running tool #1 bash: kubectl get deployment..."
// Returns e.g. ["bash: kubectl get deployment...", "bash: kubectl describe pod..."]
func extractToolCalls(raw string) []string {
	var tools []string
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "Running tool #") {
			continue
		}
		// Extract after "Running tool #N " — e.g. "Running tool #1 bash: kubectl get..."
		// Find the space after the number
		rest := strings.TrimPrefix(trimmed, "Running tool #")
		if idx := strings.Index(rest, " "); idx >= 0 {
			tool := strings.TrimSpace(rest[idx+1:])
			// Truncate long commands
			if len(tool) > 80 {
				tool = tool[:80] + "..."
			}
			tools = append(tools, tool)
		}
	}
	return tools
}

// Available checks if the holmes CLI is installed.
func Available() bool {
	_, err := exec.LookPath("holmes")
	return err == nil
}
