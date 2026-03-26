package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/lazy-diagnose-k8s/internal/domain"
	"github.com/lazy-diagnose-k8s/internal/playbook"
	"github.com/lazy-diagnose-k8s/internal/resolver"
)

// Bot is the Telegram bot that handles diagnosis requests.
type Bot struct {
	api      *tgbotapi.BotAPI
	engine   *playbook.Engine
	resolver *resolver.Resolver
	logger   *slog.Logger
	allowedChatIDs map[int64]bool
}

// NewBot creates a new Telegram bot.
func NewBot(token string, engine *playbook.Engine, resolver *resolver.Resolver, allowedChatIDs []int64, logger *slog.Logger) (*Bot, error) {
	api, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, fmt.Errorf("create telegram bot: %w", err)
	}

	allowed := make(map[int64]bool)
	for _, id := range allowedChatIDs {
		allowed[id] = true
	}

	logger.Info("telegram bot authorized", "username", api.Self.UserName)

	return &Bot{
		api:            api,
		engine:         engine,
		resolver:       resolver,
		logger:         logger,
		allowedChatIDs: allowed,
	}, nil
}

// Run starts the bot and blocks until context is cancelled.
func (b *Bot) Run(ctx context.Context) error {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := b.api.GetUpdatesChan(u)

	b.logger.Info("bot started, listening for messages")

	for {
		select {
		case <-ctx.Done():
			b.logger.Info("bot shutting down")
			b.api.StopReceivingUpdates()
			return ctx.Err()
		case update := <-updates:
			if update.Message == nil {
				continue
			}
			go b.handleMessage(ctx, update.Message)
		}
	}
}

func (b *Bot) handleMessage(ctx context.Context, msg *tgbotapi.Message) {
	chatID := msg.Chat.ID

	// Check allowed chat IDs (if configured)
	if len(b.allowedChatIDs) > 0 && !b.allowedChatIDs[chatID] {
		b.logger.Warn("unauthorized chat", "chat_id", chatID, "user", msg.From.UserName)
		return
	}

	b.logger.Info("received message",
		"chat_id", chatID,
		"user", msg.From.UserName,
		"text", msg.Text,
	)

	parsed := ParseMessage(msg.Text)

	// Handle special commands
	switch parsed.Command {
	case "start", "help":
		b.sendMessage(chatID, FormatHelpMessage())
		return
	}

	if parsed.Target == "" {
		b.sendMessage(chatID, "Missing target. Example: /check checkout or /diag payment seeing errors")
		return
	}

	// Resolve target
	target, err := b.resolver.Resolve(parsed.Target, "prod")
	if err != nil {
		b.sendMessage(chatID, FormatError(err))
		return
	}

	// Classify intent
	intent := domain.ClassifyIntent(parsed.RawText)

	// Override intent based on command
	switch parsed.Command {
	case "deploy":
		intent = domain.IntentRolloutRegression
	case "pod":
		if intent == domain.IntentUnknown {
			intent = domain.IntentCrashLoop
		}
	}

	// Create request
	req := &domain.DiagnosisRequest{
		ID:        fmt.Sprintf("%d-%d", chatID, time.Now().UnixMilli()),
		ChatID:    chatID,
		RawText:   parsed.RawText,
		Intent:    intent,
		Target:    target,
		CreatedAt: time.Now(),
	}

	// Send initial progress message
	progressMsg := b.sendMessage(chatID, fmt.Sprintf("🔍 Diagnosing %s...\nIntent: %s", target.FullName(), intent))

	// Progress callback updates the message
	progress := func(text string) {
		b.logger.Info("progress", "request_id", req.ID, "status", text)
		if progressMsg != 0 {
			b.editMessage(chatID, progressMsg, fmt.Sprintf("🔍 Diagnosing %s...\n%s", target.FullName(), text))
		}
	}

	// Run diagnosis
	result := b.engine.Run(ctx, req, progress)

	// Send final result (replace progress message)
	formatted := FormatResult(result)
	if progressMsg != 0 {
		b.editMessage(chatID, progressMsg, formatted)
	} else {
		b.sendMessage(chatID, formatted)
	}
}

func (b *Bot) sendMessage(chatID int64, text string) int {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "HTML"
	msg.DisableWebPagePreview = true

	sent, err := b.api.Send(msg)
	if err != nil {
		// Retry without HTML if parsing fails
		b.logger.Warn("failed to send HTML message, retrying plain", "error", err)
		msg.ParseMode = ""
		sent, err = b.api.Send(msg)
		if err != nil {
			b.logger.Error("failed to send message", "error", err, "chat_id", chatID)
			return 0
		}
	}
	return sent.MessageID
}

func (b *Bot) editMessage(chatID int64, messageID int, text string) {
	// Telegram has a 4096 char limit for messages
	if len(text) > 4000 {
		text = text[:4000] + "\n... (truncated)"
	}

	edit := tgbotapi.NewEditMessageText(chatID, messageID, text)
	edit.ParseMode = "HTML"
	edit.DisableWebPagePreview = true

	_, err := b.api.Send(edit)
	if err != nil {
		// Retry without markdown
		if strings.Contains(err.Error(), "can't parse entities") || strings.Contains(err.Error(), "Bad Request") {
			edit.ParseMode = ""
			_, err = b.api.Send(edit)
		}
		if err != nil {
			b.logger.Warn("failed to edit message", "error", err)
		}
	}
}
