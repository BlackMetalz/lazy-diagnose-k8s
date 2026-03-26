package telegram

import (
	"fmt"
	"strings"
	"time"

	"github.com/lazy-diagnose-k8s/internal/domain"
)

// ParsedMessage represents a parsed Telegram message.
type ParsedMessage struct {
	Command string // "diag", "pod", "deploy", "check", or empty for free text
	Target  string // the target resource or service name
	RawText string
}

// ParseMessage extracts command and target from a Telegram message.
// Supported formats:
//
//	/diag checkout vừa deploy xong
//	/pod payment-api-abc prod
//	/deploy checkout prod
//	/check checkout
//	check checkout    (without slash)
//	checkout bị crash (free text)
func ParseMessage(text string) ParsedMessage {
	text = strings.TrimSpace(text)
	if text == "" {
		return ParsedMessage{RawText: text}
	}

	// Handle slash commands
	if strings.HasPrefix(text, "/") {
		parts := strings.Fields(text)
		cmd := strings.TrimPrefix(parts[0], "/")

		switch cmd {
		case "diag", "check", "pod", "deploy":
			target := ""
			if len(parts) > 1 {
				target = parts[1]
			}
			return ParsedMessage{
				Command: cmd,
				Target:  target,
				RawText: text,
			}
		case "start", "help":
			return ParsedMessage{
				Command: cmd,
				RawText: text,
			}
		}
	}

	// Free text: try to extract target (first word that looks like a resource name)
	words := strings.Fields(text)
	target := ""
	for _, w := range words {
		// Skip common Vietnamese/English noise words
		if isNoiseWord(w) {
			continue
		}
		target = w
		break
	}

	return ParsedMessage{
		Command: "check", // default to generic check
		Target:  target,
		RawText: text,
	}
}

func isNoiseWord(w string) bool {
	noise := map[string]bool{
		"check": true, "kiểm": true, "tra": true, "xem": true,
		"sao": true, "rồi": true, "bị": true, "gì": true,
		"có": true, "vấn": true, "đề": true, "không": true,
		"the": true, "is": true, "a": true, "an": true,
		"what": true, "why": true, "how": true,
	}
	return noise[strings.ToLower(w)]
}

// FormatResult formats a DiagnosisResult into a Telegram HTML message.
// Designed for quick scanning during incidents.
func FormatResult(result *domain.DiagnosisResult) string {
	var b strings.Builder

	// ── Header: target + confidence badge ──
	badge := confidenceBadge(result.Confidence)
	b.WriteString(fmt.Sprintf("%s <b>%s</b>\n", badge, esc(result.Target.FullName())))
	b.WriteString("─────────────────────\n\n")

	// ── Summary (the most important part) ──
	b.WriteString(fmt.Sprintf("%s\n\n", esc(result.Summary)))

	// ── Key facts (compact) ──
	if result.PrimaryHypothesis != nil {
		h := result.PrimaryHypothesis
		pct := 0
		if h.MaxScore > 0 {
			pct = h.Score * 100 / h.MaxScore
		}
		b.WriteString(fmt.Sprintf("<b>Root cause:</b> %s (%d%%)\n", esc(h.Name), pct))
	}

	// Show alternative only if score > 0
	for _, h := range result.AlternativeHypotheses {
		if h.Score > 0 {
			pct := 0
			if h.MaxScore > 0 {
				pct = h.Score * 100 / h.MaxScore
			}
			b.WriteString(fmt.Sprintf("<b>Also possible:</b> %s (%d%%)\n", esc(h.Name), pct))
		}
	}
	b.WriteString("\n")

	// ── Evidence (compact, max 6 lines) ──
	if len(result.SupportingEvidence) > 0 {
		b.WriteString("<b>Evidence:</b>\n")
		limit := len(result.SupportingEvidence)
		if limit > 6 {
			limit = 6
		}
		for _, e := range result.SupportingEvidence[:limit] {
			b.WriteString(fmt.Sprintf("  <code>%s</code>\n", esc(truncate(e, 80))))
		}
		if len(result.SupportingEvidence) > 6 {
			b.WriteString(fmt.Sprintf("  <i>... +%d dòng</i>\n", len(result.SupportingEvidence)-6))
		}
		b.WriteString("\n")
	}

	// ── Next steps ──
	if len(result.RecommendedSteps) > 0 {
		b.WriteString("<b>Next steps:</b>\n")
		for i, step := range result.RecommendedSteps {
			b.WriteString(fmt.Sprintf("  %d. %s\n", i+1, esc(step)))
		}
		b.WriteString("\n")
	}

	// ── Commands (copy-paste ready) ──
	if len(result.SuggestedCommands) > 0 {
		b.WriteString("<b>Commands:</b>\n<pre>")
		for _, cmd := range result.SuggestedCommands {
			b.WriteString(esc(cmd) + "\n")
		}
		b.WriteString("</pre>\n")
	}

	// ── Notes (degraded mode warnings) ──
	if len(result.Notes) > 0 {
		for _, note := range result.Notes {
			b.WriteString(fmt.Sprintf("<i>%s</i>\n", esc(note)))
		}
	}

	// ── Footer ──
	b.WriteString(fmt.Sprintf("\n<i>%s · %s</i>", result.Duration.Round(time.Millisecond), result.Confidence))

	return b.String()
}

func confidenceBadge(c domain.Confidence) string {
	switch c {
	case domain.ConfidenceHigh:
		return "🔴"
	case domain.ConfidenceMedium:
		return "🟡"
	default:
		return "⚪"
	}
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// FormatHelpMessage returns the help text for the bot.
func FormatHelpMessage() string {
	return `🤖 <b>lazy-diagnose-k8s</b>

Kubernetes diagnosis via Telegram. Collects data from K8s, metrics, and logs — returns diagnosis + suggested commands.

<b>Commands:</b>
/check &lt;target&gt; — General health check
/diag &lt;target&gt; &lt;context&gt; — Diagnosis with description
/pod &lt;pod-name&gt; — Check a specific pod
/deploy &lt;deployment&gt; — Check rollout status
/help — This message

<b>Examples:</b>
• <code>/check checkout</code>
• <code>/diag payment just deployed, seeing 5xx</code>
• <code>/pod payment-api-7f8b9c-x4k2p</code>
• <code>/deploy checkout</code>

<b>What it detects:</b>
• CrashLoop — OOM, missing config, dependency fail, probe issue, bad image
• Pending — insufficient resources, taint/affinity, PVC, quota
• Rollout regression — failed deploy, image pull error, resource pressure

<b>Target can be:</b>
• Service name from service_map (checkout, payment, worker...)
• Exact deployment/pod name
• Path format: <code>deployment/checkout</code> or <code>prod/deployment/checkout</code>

<b>Reading results:</b>
🔴 High confidence — clear root cause found
🟡 Medium — likely cause, some data missing
⚪ Low — inconclusive, check manually`
}

// FormatError formats an error message for Telegram.
func FormatError(err error) string {
	return fmt.Sprintf("❌ <b>Error:</b> %s", esc(err.Error()))
}

// esc escapes HTML special characters for Telegram HTML mode.
func esc(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}
