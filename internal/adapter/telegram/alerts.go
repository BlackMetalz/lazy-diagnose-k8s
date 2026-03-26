package telegram

import (
	"context"
	"fmt"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/lazy-diagnose-k8s/internal/domain"
	"github.com/lazy-diagnose-k8s/internal/webhook"
)

// HandleAlert is called by the webhook server when Alertmanager fires.
// It runs diagnosis for each target and sends results to the configured chat.
func (b *Bot) HandleAlert(ctx context.Context, targets []webhook.AlertTarget, payload *webhook.AlertmanagerPayload) {
	for _, chatID := range b.alertChatIDs {
		for _, target := range targets {
			b.diagnoseAlert(ctx, chatID, target, len(payload.Alerts))
		}
	}
}

func (b *Bot) diagnoseAlert(ctx context.Context, chatID int64, alertTarget webhook.AlertTarget, alertCount int) {
	// Send alert notification first
	alertMsg := webhook.FormatAlertMessage(alertTarget, alertCount)
	b.sendMessage(chatID, alertMsg)

	// Resolve target
	ns := alertTarget.Namespace
	if ns == "" {
		ns = b.defaultNamespace
	}

	targetName := alertTarget.Name
	if alertTarget.Kind != "" && alertTarget.Kind != "pod" {
		targetName = alertTarget.Kind + "/" + alertTarget.Name
	}

	resolvedTarget, err := b.resolver.Resolve(targetName, ns)
	if err != nil {
		b.logger.Warn("failed to resolve alert target, trying raw name",
			"target", targetName, "error", err)
		// Fallback: construct target directly
		resolvedTarget = &domain.Target{
			Name:         alertTarget.Name,
			Namespace:    ns,
			Kind:         alertTarget.Kind,
			ResourceName: alertTarget.Name,
		}
	}

	// Classify intent from alert name
	intent := classifyAlertIntent(alertTarget.AlertName)

	req := &domain.DiagnosisRequest{
		ID:        fmt.Sprintf("alert-%s-%d", alertTarget.Fingerprint(), time.Now().UnixMilli()),
		ChatID:    chatID,
		RawText:   fmt.Sprintf("[Alert] %s: %s", alertTarget.AlertName, alertTarget.Summary),
		Intent:    intent,
		Target:    resolvedTarget,
		CreatedAt: time.Now(),
	}

	// Send progress
	progressMsg := b.sendMessage(chatID, fmt.Sprintf("🔍 Auto-diagnosing %s...", resolvedTarget.FullName()))

	progress := func(text string) {
		b.logger.Info("alert diagnosis progress", "alert", alertTarget.AlertName, "status", text)
		if progressMsg != 0 {
			b.editMessage(chatID, progressMsg, fmt.Sprintf("🔍 Auto-diagnosing %s...\n%s", resolvedTarget.FullName(), text))
		}
	}

	// Run diagnosis
	result := b.engine.Run(ctx, req, progress)

	// Format result with inline buttons
	formatted := FormatResult(result)

	// Add inline keyboard
	keyboard := buildDiagnosisKeyboard(resolvedTarget, ns)

	if progressMsg != 0 {
		b.editMessageWithKeyboard(chatID, progressMsg, formatted, keyboard)
	} else {
		b.sendMessageWithKeyboard(chatID, formatted, keyboard)
	}
}

func classifyAlertIntent(alertName string) domain.Intent {
	// Map common alert names to intents
	intent := domain.ClassifyIntent(alertName)
	if intent != domain.IntentUnknown {
		return intent
	}

	// Common kube-state-metrics alert patterns
	switch {
	case contains(alertName, "CrashLoopBackOff", "OOMKilled", "PodCrash", "ContainerRestart", "KubePodCrashLooping"):
		return domain.IntentCrashLoop
	case contains(alertName, "Pending", "Unschedulable", "FailedScheduling", "KubePodNotReady"):
		return domain.IntentPending
	case contains(alertName, "Rollout", "Deploy", "Revision", "KubeDeploymentReplicasMismatch"):
		return domain.IntentRolloutRegression
	default:
		return domain.IntentCrashLoop // default
	}
}

func contains(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if len(s) >= len(sub) {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}

func buildDiagnosisKeyboard(target *domain.Target, ns string) tgbotapi.InlineKeyboardMarkup {
	rerunData := fmt.Sprintf("rerun:%s:%s", ns, target.ResourceName)
	logsData := fmt.Sprintf("logs:%s:%s", ns, target.ResourceName)
	scanData := fmt.Sprintf("scan:%s", ns)

	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Rerun", rerunData),
			tgbotapi.NewInlineKeyboardButtonData("📜 Logs", logsData),
			tgbotapi.NewInlineKeyboardButtonData("🔍 Scan NS", scanData),
		),
	)
}
