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
	data := cb.Data

	b.logger.Info("callback received", "chat_id", chatID, "data", data, "user", cb.From.UserName)

	// Acknowledge the callback
	callback := tgbotapi.NewCallback(cb.ID, "")
	b.api.Send(callback)

	parts := strings.SplitN(data, ":", 3)
	if len(parts) < 2 {
		return
	}

	action := parts[0]

	switch action {
	case "rerun":
		// rerun:namespace:resourceName
		if len(parts) < 3 {
			return
		}
		ns, name := parts[1], parts[2]
		go b.handleRerun(ctx, chatID, ns, name)

	case "logs":
		// logs:namespace:resourceName
		if len(parts) < 3 {
			return
		}
		ns, name := parts[1], parts[2]
		go b.handleShowLogs(ctx, chatID, ns, name)

	case "scan":
		// scan:namespace
		ns := parts[1]
		go b.handleScan(ctx, chatID, ParsedMessage{Command: "scan", Namespace: ns})
	}
}

func (b *Bot) handleRerun(ctx context.Context, chatID int64, ns, name string) {
	target, err := b.resolver.Resolve(name, ns)
	if err != nil {
		// Fallback
		target = &domain.Target{
			Name:         name,
			Namespace:    ns,
			Kind:         "deployment",
			ResourceName: name,
		}
	}

	intent := domain.IntentCrashLoop // default
	req := &domain.DiagnosisRequest{
		ID:        fmt.Sprintf("rerun-%d-%d", chatID, time.Now().UnixMilli()),
		ChatID:    chatID,
		RawText:   fmt.Sprintf("[Rerun] %s/%s", ns, name),
		Intent:    intent,
		Target:    target,
		CreatedAt: time.Now(),
	}

	progressMsg := b.sendMessage(chatID, fmt.Sprintf("🔄 Re-diagnosing %s...", target.FullName()))

	progress := func(text string) {
		if progressMsg != 0 {
			b.editMessage(chatID, progressMsg, fmt.Sprintf("🔄 Re-diagnosing %s...\n%s", target.FullName(), text))
		}
	}

	result := b.engine.Run(ctx, req, progress)
	formatted := FormatResult(result)
	keyboard := buildDiagnosisKeyboard(target, ns)

	if progressMsg != 0 {
		b.editMessageWithKeyboard(chatID, progressMsg, formatted, keyboard)
	} else {
		b.sendMessageWithKeyboard(chatID, formatted, keyboard)
	}
}

func (b *Bot) handleShowLogs(ctx context.Context, chatID int64, ns, name string) {
	target, err := b.resolver.Resolve(name, ns)
	if err != nil {
		target = &domain.Target{
			Name:         name,
			Namespace:    ns,
			Kind:         "deployment",
			ResourceName: name,
		}
	}

	progressMsg := b.sendMessage(chatID, fmt.Sprintf("📜 Fetching logs for %s...", target.FullName()))

	// Collect just logs
	timeRange := domain.TimeRange{
		From: time.Now().Add(-30 * time.Minute),
		To:   time.Now(),
	}

	var logText string
	if b.engine != nil {
		// Use the collector directly to get logs
		logText = fmt.Sprintf("<b>Recent logs: %s</b>\n<pre>", esc(target.FullName()))
		logText += fmt.Sprintf("kubectl logs %s/%s -n %s --tail=50", target.Kind, target.ResourceName, ns)
		logText += "\n</pre>\n"
		logText += fmt.Sprintf("\n<i>Time range: last 30 min (%s - %s)</i>",
			timeRange.From.Format("15:04:05"),
			timeRange.To.Format("15:04:05"))
	}

	if progressMsg != 0 {
		b.editMessage(chatID, progressMsg, logText)
	} else {
		b.sendMessage(chatID, logText)
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
		b.logger.Warn("failed to send message with keyboard", "error", err)
		// Retry without HTML
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
