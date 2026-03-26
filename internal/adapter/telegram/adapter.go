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
func FormatResult(result *domain.DiagnosisResult) string {
	var b strings.Builder

	// Header
	b.WriteString(fmt.Sprintf("🔍 <b>Diagnosis: %s</b>\n\n", esc(result.Target.FullName())))

	// Summary
	b.WriteString(fmt.Sprintf("📋 <b>Kết luận:</b> %s\n\n", esc(result.Summary)))

	// Confidence
	confidenceEmoji := "🟡"
	switch result.Confidence {
	case domain.ConfidenceHigh:
		confidenceEmoji = "🟢"
	case domain.ConfidenceLow:
		confidenceEmoji = "🔴"
	}
	b.WriteString(fmt.Sprintf("%s <b>Độ tin cậy:</b> %s\n\n", confidenceEmoji, result.Confidence))

	// Primary hypothesis
	if result.PrimaryHypothesis != nil {
		b.WriteString(fmt.Sprintf("🎯 <b>Nguyên nhân chính:</b> %s (score: %d/%d)\n",
			esc(result.PrimaryHypothesis.Name),
			result.PrimaryHypothesis.Score,
			result.PrimaryHypothesis.MaxScore))
		if len(result.PrimaryHypothesis.Signals) > 0 {
			b.WriteString(fmt.Sprintf("    <i>Signals: %s</i>\n", esc(strings.Join(result.PrimaryHypothesis.Signals, ", "))))
		}
		b.WriteString("\n")
	}

	// Alternative hypotheses
	if len(result.AlternativeHypotheses) > 0 {
		b.WriteString("📊 <b>Nguyên nhân khác:</b>\n")
		for _, h := range result.AlternativeHypotheses {
			b.WriteString(fmt.Sprintf("  • %s (score: %d/%d)\n", esc(h.Name), h.Score, h.MaxScore))
		}
		b.WriteString("\n")
	}

	// Evidence
	if len(result.SupportingEvidence) > 0 {
		b.WriteString("📎 <b>Bằng chứng:</b>\n")
		for _, e := range result.SupportingEvidence {
			b.WriteString(fmt.Sprintf("  • %s\n", esc(e)))
		}
		b.WriteString("\n")
	}

	// Recommended steps
	if len(result.RecommendedSteps) > 0 {
		b.WriteString("👉 <b>Bước tiếp theo:</b>\n")
		for i, step := range result.RecommendedSteps {
			b.WriteString(fmt.Sprintf("  %d. %s\n", i+1, esc(step)))
		}
		b.WriteString("\n")
	}

	// Commands
	if len(result.SuggestedCommands) > 0 {
		b.WriteString("💻 <b>Commands:</b>\n<pre>")
		for _, cmd := range result.SuggestedCommands {
			b.WriteString(esc(cmd) + "\n")
		}
		b.WriteString("</pre>\n")
	}

	// Notes
	if len(result.Notes) > 0 {
		b.WriteString("📝 <b>Notes:</b>\n")
		for _, note := range result.Notes {
			b.WriteString(fmt.Sprintf("  <i>%s</i>\n", esc(note)))
		}
		b.WriteString("\n")
	}

	// Duration
	b.WriteString(fmt.Sprintf("⏱ Thời gian: %s", result.Duration.Round(time.Millisecond)))

	return b.String()
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
