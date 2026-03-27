package telegram

import (
	"context"
	"fmt"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/lazy-diagnose-k8s/internal/domain"
)

// handleCallback processes inline keyboard button presses.
func (b *Bot) handleCallback(ctx context.Context, cb *tgbotapi.CallbackQuery) {
	chatID := cb.Message.Chat.ID
	alertMsgID := cb.Message.MessageID // the alert notification message — reply to this
	data := cb.Data

	b.logger.Info("callback received", "chat_id", chatID, "data", data, "user", cb.From.UserName)

	// Acknowledge the callback immediately
	b.api.Send(tgbotapi.NewCallback(cb.ID, ""))

	parts := strings.SplitN(data, ":", 3)
	if len(parts) < 2 {
		return
	}

	action := parts[0]

	switch action {
	case "ai":
		if len(parts) < 3 {
			return
		}
		go b.handleAIInvestigation(ctx, chatID, alertMsgID, parts[1], parts[2])

	case "static":
		if len(parts) < 3 {
			return
		}
		go b.handleStaticAnalysis(ctx, chatID, alertMsgID, parts[1], parts[2])

	case "logs":
		if len(parts) < 3 {
			return
		}
		go b.handleShowLogs(ctx, chatID, alertMsgID, parts[1], parts[2])

	case "scan":
		ns := parts[1]
		go b.handleScan(ctx, chatID, ParsedMessage{Command: "scan", Namespace: ns})

	case "rerun":
		if len(parts) < 3 {
			return
		}
		go b.handleStaticAnalysis(ctx, chatID, alertMsgID, parts[1], parts[2])
	}
}

// handleAIInvestigation collects evidence and sends to LLM for free-form analysis.
func (b *Bot) handleAIInvestigation(ctx context.Context, chatID int64, replyTo int, ns, name string) {
	target := b.resolveOrFallback(ns, name)
	progressMsg := b.sendReply(chatID, replyTo, fmt.Sprintf("🤖 AI investigating %s...", target.FullName()))

	bundle := b.collectEvidence(ctx, target, progressMsg)

	if b.engine.HasSummarizer() {
		b.updateProgress(progressMsg, chatID, fmt.Sprintf("🤖 AI investigating %s...\n✓ Data collected, asking LLM...", target.FullName()))

		intent := domain.IntentCrashLoop
		summary, err := b.engine.SummarizeWithLLM(ctx, intent, bundle)
		if err != nil {
			b.logger.Warn("AI investigation failed", "error", err)
			text := fmt.Sprintf("🤖 <b>AI Investigation</b>\n─────────────────────\n\n❌ LLM unavailable: %s\n\nUse 📊 Static Analysis instead.", esc(err.Error()))
			keyboard := buildPostDiagnosisKeyboard(ns, name)
			b.editMessageWithKeyboard(chatID, progressMsg, text, keyboard)
			return
		}

		text := fmt.Sprintf("🤖 <b>AI Investigation</b>\n─────────────────────\n\n%s", esc(summary))
		keyboard := buildPostDiagnosisKeyboard(ns, name)
		b.editMessageWithKeyboard(chatID, progressMsg, text, keyboard)
	} else {
		text := "🤖 <b>AI Investigation</b>\n─────────────────────\n\n❌ LLM not configured.\n\nSet <code>llm.enabled: true</code> in config.yaml or use 📊 Static Analysis."
		keyboard := buildPostDiagnosisKeyboard(ns, name)
		b.editMessageWithKeyboard(chatID, progressMsg, text, keyboard)
	}
}

// handleStaticAnalysis runs the deterministic playbook scoring pipeline.
func (b *Bot) handleStaticAnalysis(ctx context.Context, chatID int64, replyTo int, ns, name string) {
	target := b.resolveOrFallback(ns, name)

	intent := domain.ClassifyIntent(name)
	if intent == domain.IntentUnknown {
		intent = domain.IntentCrashLoop
	}

	req := &domain.DiagnosisRequest{
		ID:        fmt.Sprintf("static-%d-%d", chatID, time.Now().UnixMilli()),
		ChatID:    chatID,
		RawText:   fmt.Sprintf("[Static] %s/%s", ns, name),
		Intent:    intent,
		Target:    target,
		CreatedAt: time.Now(),
	}

	progressMsg := b.sendReply(chatID, replyTo, fmt.Sprintf("📊 Analyzing %s...", target.FullName()))

	progress := func(text string) {
		b.updateProgress(progressMsg, chatID, fmt.Sprintf("📊 Analyzing %s...\n%s", target.FullName(), text))
	}

	result := b.engine.RunWithoutLLM(ctx, req, progress)
	formatted := FormatResultCompact(result)
	keyboard := buildPostDiagnosisKeyboard(ns, name)

	if progressMsg != 0 {
		b.editMessageWithKeyboard(chatID, progressMsg, formatted, keyboard)
	} else {
		b.sendMessageWithKeyboard(chatID, formatted, keyboard)
	}
}

// handleShowLogs queries VictoriaLogs and shows raw container logs.
func (b *Bot) handleShowLogs(ctx context.Context, chatID int64, replyTo int, ns, name string) {
	target := b.resolveOrFallback(ns, name)
	progressMsg := b.sendReply(chatID, replyTo, fmt.Sprintf("📜 Fetching logs for %s...", target.FullName()))

	timeRange := domain.TimeRange{
		From: time.Now().Add(-30 * time.Minute),
		To:   time.Now(),
	}

	// Try to get real logs from provider
	var logLines []string
	if b.engine.HasLogsProvider() {
		facts, err := b.engine.CollectLogs(ctx, target, timeRange)
		if err != nil {
			b.logger.Warn("failed to fetch logs", "target", target.FullName(), "error", err)
		} else if facts != nil {
			logLines = facts.RecentLines
		}
	}

	var text string
	if len(logLines) > 0 {
		var lb strings.Builder
		lb.WriteString("📜 <b>Logs</b> (last 30 min)\n─────────────────────\n\n<pre>")
		limit := len(logLines)
		if limit > 30 {
			limit = 30
		}
		for _, line := range logLines[:limit] {
			if len(line) > 200 {
				line = line[:200] + "..."
			}
			lb.WriteString(esc(line) + "\n")
		}
		lb.WriteString("</pre>")
		if len(logLines) > 30 {
			lb.WriteString(fmt.Sprintf("\n<i>... showing 30/%d lines</i>", len(logLines)))
		}
		lb.WriteString(fmt.Sprintf("\n\n<i>%s — %s</i>", timeRange.From.Format("15:04"), timeRange.To.Format("15:04")))
		text = lb.String()
	} else {
		text = fmt.Sprintf("📜 <b>Logs</b>\n─────────────────────\n\nNo logs found in last 30 minutes.\n\nTry manually:\n<pre>kubectl logs %s/%s -n %s --tail=50</pre>",
			esc(target.Kind), esc(target.ResourceName), esc(ns))
	}

	keyboard := buildPostDiagnosisKeyboard(ns, name)
	if progressMsg != 0 {
		b.editMessageWithKeyboard(chatID, progressMsg, text, keyboard)
	} else {
		b.sendMessageWithKeyboard(chatID, text, keyboard)
	}
}

// --- helpers ---

func (b *Bot) resolveOrFallback(ns, name string) *domain.Target {
	target, err := b.resolver.Resolve(name, ns)
	if err != nil {
		return &domain.Target{
			Name:         name,
			Namespace:    ns,
			Kind:         "deployment",
			ResourceName: name,
		}
	}
	return target
}

func (b *Bot) collectEvidence(ctx context.Context, target *domain.Target, progressMsg int) *domain.EvidenceBundle {
	timeRange := domain.TimeRange{
		From: time.Now().Add(-1 * time.Hour),
		To:   time.Now(),
	}
	collector := b.engine.GetCollector()
	if collector == nil {
		return &domain.EvidenceBundle{Target: target}
	}
	return collector.Collect(ctx, target, timeRange)
}

func (b *Bot) updateProgress(msgID int, chatID int64, text string) {
	if msgID != 0 {
		b.editMessage(chatID, msgID, text)
	}
}

// sendMessageWithKeyboard sends a message with inline keyboard.
func (b *Bot) sendMessageWithKeyboard(chatID int64, text string, keyboard tgbotapi.InlineKeyboardMarkup) int {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "HTML"
	msg.DisableWebPagePreview = true
	msg.ReplyMarkup = keyboard

	sent, err := b.api.Send(msg)
	if err != nil {
		b.logger.Warn("failed to send message with keyboard, retrying plain", "error", err)
		msg.ParseMode = ""
		sent, err = b.api.Send(msg)
		if err != nil {
			b.logger.Error("failed to send message", "error", err)
			return 0
		}
	}
	return sent.MessageID
}

// editMessageWithKeyboard edits a message with new text and inline keyboard.
func (b *Bot) editMessageWithKeyboard(chatID int64, messageID int, text string, keyboard tgbotapi.InlineKeyboardMarkup) {
	if len(text) > 4000 {
		text = text[:4000] + "\n... (truncated)"
	}

	edit := tgbotapi.NewEditMessageText(chatID, messageID, text)
	edit.ParseMode = "HTML"
	edit.DisableWebPagePreview = true
	replyMarkup := keyboard
	edit.ReplyMarkup = &replyMarkup

	_, err := b.api.Send(edit)
	if err != nil {
		if strings.Contains(err.Error(), "can't parse entities") || strings.Contains(err.Error(), "Bad Request") {
			edit.ParseMode = ""
			b.api.Send(edit)
		}
	}
}
