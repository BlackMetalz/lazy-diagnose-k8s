package telegram

import (
	"context"
	"fmt"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/lazy-diagnose-k8s/internal/domain"
)

// Callback data format: "action:cluster:ns:name"
// For backwards compatibility, if only 3 parts → treat as "action:ns:name" with default cluster.

// parseCallbackData extracts action, cluster, namespace, and name from callback data.
func parseCallbackData(data string) (action, cluster, ns, name string) {
	parts := strings.SplitN(data, ":", 4)
	switch len(parts) {
	case 4:
		return parts[0], parts[1], parts[2], parts[3]
	case 3:
		// backwards compat: action:ns:name (no cluster)
		return parts[0], "", parts[1], parts[2]
	case 2:
		return parts[0], "", parts[1], ""
	default:
		return data, "", "", ""
	}
}

// handleCallback processes inline keyboard button presses.
func (b *Bot) handleCallback(ctx context.Context, cb *tgbotapi.CallbackQuery) {
	chatID := cb.Message.Chat.ID
	alertMsgID := cb.Message.MessageID // the alert notification message — reply to this
	data := cb.Data

	b.logger.Info("callback received", "chat_id", chatID, "data", data, "user", cb.From.UserName)

	// Acknowledge the callback immediately
	b.api.Send(tgbotapi.NewCallback(cb.ID, ""))

	// Rate limit check
	if !b.limiter.allow(cb.From.ID) {
		b.logger.Warn("rate limited callback", "user_id", cb.From.ID, "user", cb.From.UserName)
		b.api.Send(tgbotapi.NewCallback(cb.ID, "Rate limited, try again shortly"))
		return
	}

	action, cluster, ns, name := parseCallbackData(data)

	// Dedup: skip if same operation is already in-flight for this chat
	inflightKey := fmt.Sprintf("%d:%s", chatID, data)
	if _, loaded := b.inflight.LoadOrStore(inflightKey, true); loaded {
		b.logger.Info("skipping duplicate callback", "key", inflightKey)
		return
	}

	switch action {
	case "ai":
		if name == "" {
			b.inflight.Delete(inflightKey)
			return
		}
		go func() {
			defer b.inflight.Delete(inflightKey)
			b.handleAIInvestigation(ctx, chatID, alertMsgID, cluster, ns, name)
		}()

	case "static":
		if name == "" {
			b.inflight.Delete(inflightKey)
			return
		}
		go func() {
			defer b.inflight.Delete(inflightKey)
			b.handleStaticAnalysis(ctx, chatID, alertMsgID, cluster, ns, name)
		}()

	case "logs":
		if name == "" {
			b.inflight.Delete(inflightKey)
			return
		}
		go func() {
			defer b.inflight.Delete(inflightKey)
			b.handleShowLogs(ctx, chatID, alertMsgID, cluster, ns, name)
		}()

	case "scan":
		go func() {
			defer b.inflight.Delete(inflightKey)
			b.handleScan(ctx, chatID, ParsedMessage{Command: "scan", Namespace: ns, Cluster: cluster})
		}()

	case "rerun":
		if name == "" {
			b.inflight.Delete(inflightKey)
			return
		}
		go func() {
			defer b.inflight.Delete(inflightKey)
			b.handleStaticAnalysis(ctx, chatID, alertMsgID, cluster, ns, name)
		}()

	default:
		b.inflight.Delete(inflightKey)
	}
}

// handleAIInvestigation collects evidence and sends to LLM for free-form analysis.
func (b *Bot) handleAIInvestigation(ctx context.Context, chatID int64, replyTo int, clusterName, ns, name string) {
	cluster := b.getCluster(clusterName)
	target := b.resolveOrFallback(ns, name)
	target.Cluster = cluster.Name
	progressMsg := b.sendReply(chatID, replyTo, fmt.Sprintf("🤖 AI investigating %s...", target.FullName()))

	bundle := b.collectEvidence(ctx, cluster, target, progressMsg)

	if cluster.Engine.HasSummarizer() {
		b.updateProgress(progressMsg, chatID, fmt.Sprintf("🤖 AI investigating %s...\n✓ Data collected, asking LLM...", target.FullName()))

		intent := domain.IntentCrashLoop
		summary, err := cluster.Engine.SummarizeWithLLM(ctx, intent, bundle)
		if err != nil {
			b.logger.Warn("AI investigation failed", "error", err)
			text := fmt.Sprintf("🤖 <b>AI Investigation</b>\n─────────────────────\n\n❌ LLM unavailable: %s\n\nUse 📊 Static Analysis instead.", esc(err.Error()))
			keyboard := buildPostDiagnosisKeyboard(cluster.Name, ns, name, "ai")
			b.editMessageWithKeyboard(chatID, progressMsg, text, keyboard)
			return
		}

		text := fmt.Sprintf("🤖 <b>AI Investigation</b>\n─────────────────────\n\n%s", esc(summary))
		keyboard := buildPostDiagnosisKeyboard(cluster.Name, ns, name, "ai")
		b.editMessageWithKeyboard(chatID, progressMsg, text, keyboard)
	} else {
		text := "🤖 <b>AI Investigation</b>\n─────────────────────\n\n❌ LLM not configured.\n\nSet <code>llm.enabled: true</code> in config.yaml or use 📊 Static Analysis."
		keyboard := buildPostDiagnosisKeyboard(cluster.Name, ns, name, "ai")
		b.editMessageWithKeyboard(chatID, progressMsg, text, keyboard)
	}
}

// handleStaticAnalysis runs the deterministic playbook scoring pipeline.
func (b *Bot) handleStaticAnalysis(ctx context.Context, chatID int64, replyTo int, clusterName, ns, name string) {
	cluster := b.getCluster(clusterName)
	target := b.resolveOrFallback(ns, name)
	target.Cluster = cluster.Name

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

	result := cluster.Engine.RunWithoutLLM(ctx, req, progress)
	formatted := FormatResultCompact(result)
	keyboard := buildPostDiagnosisKeyboard(cluster.Name, ns, name, "static")

	if progressMsg != 0 {
		b.editMessageWithKeyboard(chatID, progressMsg, formatted, keyboard)
	} else {
		b.sendMessageWithKeyboard(chatID, formatted, keyboard)
	}
}

// handleShowLogs queries VictoriaLogs and shows raw container logs.
func (b *Bot) handleShowLogs(ctx context.Context, chatID int64, replyTo int, clusterName, ns, name string) {
	cluster := b.getCluster(clusterName)
	target := b.resolveOrFallback(ns, name)
	target.Cluster = cluster.Name
	progressMsg := b.sendReply(chatID, replyTo, fmt.Sprintf("📜 Fetching logs for %s...", target.FullName()))

	timeRange := domain.TimeRange{
		From: time.Now().Add(-30 * time.Minute),
		To:   time.Now(),
	}

	// Try to get real logs from provider
	var logLines []string
	if cluster.Engine.HasLogsProvider() {
		facts, err := cluster.Engine.CollectLogs(ctx, target, timeRange)
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

	keyboard := buildPostDiagnosisKeyboard(cluster.Name, ns, name, "logs")
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

func (b *Bot) collectEvidence(ctx context.Context, cluster *ClusterEntry, target *domain.Target, progressMsg int) *domain.EvidenceBundle {
	timeRange := domain.TimeRange{
		From: time.Now().Add(-1 * time.Hour),
		To:   time.Now(),
	}
	collector := cluster.Engine.GetCollector()
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
