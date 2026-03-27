package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/lazy-diagnose-k8s/internal/domain"
	"github.com/lazy-diagnose-k8s/internal/playbook"
	k8sprovider "github.com/lazy-diagnose-k8s/internal/provider/kubernetes"
	"github.com/lazy-diagnose-k8s/internal/resolver"
	"github.com/lazy-diagnose-k8s/internal/webhook"
)

// Bot is the Telegram bot that handles diagnosis requests.
type Bot struct {
	api              *tgbotapi.BotAPI
	engine           *playbook.Engine
	resolver         *resolver.Resolver
	scanner          *k8sprovider.Provider
	defaultNamespace string
	alertFormat      webhook.AlertFormatConfig
	logger           *slog.Logger
	allowedChatIDs   map[int64]bool
	alertChatIDs     []int64 // chats to receive alert notifications
	inflight         sync.Map // tracks in-flight callback operations (key: "action:ns:name")
}

// NewBot creates a new Telegram bot.
func NewBot(token string, engine *playbook.Engine, resolver *resolver.Resolver, scanner *k8sprovider.Provider, defaultNs string, allowedChatIDs []int64, alertChatIDs []int64, alertFmt webhook.AlertFormatConfig, logger *slog.Logger) (*Bot, error) {
	api, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, fmt.Errorf("create telegram bot: %w", err)
	}

	allowed := make(map[int64]bool)
	for _, id := range allowedChatIDs {
		allowed[id] = true
	}

	logger.Info("telegram bot authorized", "username", api.Self.UserName)

	if defaultNs == "" {
		defaultNs = "prod"
	}

	return &Bot{
		api:              api,
		engine:           engine,
		resolver:         resolver,
		scanner:          scanner,
		defaultNamespace: defaultNs,
		alertFormat:      alertFmt,
		logger:           logger,
		allowedChatIDs:   allowed,
		alertChatIDs:     alertChatIDs,
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
			if update.CallbackQuery != nil {
				go b.handleCallback(ctx, update.CallbackQuery)
				continue
			}
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
	case "scan":
		go b.handleScan(ctx, chatID, parsed)
		return
	}

	if parsed.Target == "" {
		b.sendMessage(chatID, "Missing target. Example:\n  <code>/check checkout</code>\n  <code>/scan</code>")
		return
	}

	// Resolve namespace: flag > fuzzy search > default
	ns := parsed.Namespace

	// If no namespace specified, try fuzzy search to find the right namespace + pod
	if ns == "" && b.scanner != nil {
		matches, err := b.scanner.FuzzyFindPod(ctx, parsed.Target, "") // search all namespaces
		if err == nil && len(matches) > 0 {
			if matches[0].Score >= 60 {
				ns = matches[0].Namespace
			}
			// If low score, show candidates
			if matches[0].Score < 60 {
				var sb strings.Builder
				sb.WriteString(fmt.Sprintf("🔍 Multiple matches for <code>%s</code>:\n\n", esc(parsed.Target)))
				for _, m := range matches {
					sb.WriteString(fmt.Sprintf("  • <code>/check %s -n %s</code>  (%s)\n", esc(m.Name), esc(m.Namespace), esc(m.Phase)))
				}
				b.sendMessage(chatID, sb.String())
				return
			}
		}
	}
	if ns == "" {
		ns = b.defaultNamespace
	}

	// Resolve target
	target, err := b.resolver.Resolve(parsed.Target, ns)
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

	// Run diagnosis (static analysis by default for /check)
	result := b.engine.RunWithoutLLM(ctx, req, progress)

	// Send final result with action buttons
	formatted := FormatResult(result)
	keyboard := buildPostDiagnosisKeyboard(ns, target.ResourceName)
	if progressMsg != 0 {
		b.editMessageWithKeyboard(chatID, progressMsg, formatted, keyboard)
	} else {
		b.sendMessageWithKeyboard(chatID, formatted, keyboard)
	}
}

func (b *Bot) handleScan(ctx context.Context, chatID int64, parsed ParsedMessage) {
	ns := parsed.Namespace

	if b.scanner == nil {
		b.sendMessage(chatID, FormatError(fmt.Errorf("K8s provider not available, cannot scan")))
		return
	}

	scanAll := ns == ""
	label := ns
	if scanAll {
		label = "all namespaces"
	}

	progressMsg := b.sendMessage(chatID, fmt.Sprintf("🔍 Scanning <b>%s</b>...", esc(label)))
	start := time.Now()

	var unhealthy []k8sprovider.UnhealthyPod
	var err error
	if scanAll {
		unhealthy, err = b.scanner.ScanAllNamespaces(ctx)
		ns = "all"
	} else {
		unhealthy, err = b.scanner.ScanNamespace(ctx, ns)
	}
	if err != nil {
		b.sendMessage(chatID, FormatError(fmt.Errorf("scan %s: %w", label, err)))
		return
	}

	// Convert to ScanResult
	var results []ScanResult
	seen := make(map[string]bool) // dedupe by owner+namespace
	for _, u := range unhealthy {
		key := u.Namespace + "/" + u.OwnerName
		if u.OwnerName == "" {
			key = u.Namespace + "/" + u.Name
		}
		if seen[key] {
			continue
		}
		seen[key] = true

		results = append(results, ScanResult{
			Name:      u.Name,
			Namespace: u.Namespace,
			Reason:    u.Reason,
			Restarts:  u.Restarts,
			OwnerKind: u.OwnerKind,
			OwnerName: u.OwnerName,
		})
	}

	b.logger.Info("scan complete", "namespace", ns, "unhealthy", len(results), "duration", time.Since(start))

	formatted := FormatScanResult(ns, results, time.Since(start))
	if progressMsg != 0 {
		b.editMessage(chatID, progressMsg, formatted)
	} else {
		b.sendMessage(chatID, formatted)
	}
}

// sendReply sends a message as a reply to another message (native Telegram reply).
func (b *Bot) sendReply(chatID int64, replyToMsgID int, text string) int {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "HTML"
	msg.DisableWebPagePreview = true
	if replyToMsgID > 0 {
		msg.ReplyToMessageID = replyToMsgID
	}

	sent, err := b.api.Send(msg)
	if err != nil {
		msg.ParseMode = ""
		sent, err = b.api.Send(msg)
		if err != nil {
			b.logger.Error("failed to send reply", "error", err)
			return 0
		}
	}
	return sent.MessageID
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
