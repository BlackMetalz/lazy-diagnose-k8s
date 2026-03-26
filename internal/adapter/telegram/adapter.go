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
		b.WriteString(fmt.Sprintf("<b>Nguyên nhân:</b> %s (%d%%)\n", esc(h.Name), pct))
	}

	// Show alternative only if score > 0
	for _, h := range result.AlternativeHypotheses {
		if h.Score > 0 {
			pct := 0
			if h.MaxScore > 0 {
				pct = h.Score * 100 / h.MaxScore
			}
			b.WriteString(fmt.Sprintf("<b>Cũng có thể:</b> %s (%d%%)\n", esc(h.Name), pct))
		}
	}
	b.WriteString("\n")

	// ── Evidence (compact, max 6 lines) ──
	if len(result.SupportingEvidence) > 0 {
		b.WriteString("<b>Bằng chứng:</b>\n")
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
		b.WriteString("<b>Làm gì tiếp:</b>\n")
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
	return `🤖 <b>lazy-diagnose-k8s Bot</b>

Hỗ trợ diagnosis Kubernetes qua Telegram.

<b>Commands:</b>
/check &lt;target&gt; — Kiểm tra tổng quát
/diag &lt;target&gt; &lt;mô tả&gt; — Diagnosis với context
/pod &lt;pod-name&gt; — Kiểm tra pod cụ thể
/deploy &lt;deployment&gt; — Kiểm tra deployment

<b>Ví dụ:</b>
• /check checkout
• /diag payment vừa deploy xong có lỗi 5xx
• /pod payment-api-7f8b9c-x4k2p
• /deploy checkout

<b>Target có thể là:</b>
• Tên service trong service_map (checkout, payment...)
• Tên deployment/pod chính xác
• Format: deployment/checkout hoặc prod/deployment/checkout

Bot sẽ tự thu thập dữ liệu từ K8s, logs, metrics và trả về kết luận + command gợi ý.`
}

// FormatError formats an error message for Telegram.
func FormatError(err error) string {
	return fmt.Sprintf("❌ <b>Lỗi:</b> %s", esc(err.Error()))
}

// esc escapes HTML special characters for Telegram HTML mode.
func esc(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}
