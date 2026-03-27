package telegram

import (
	"context"
	"fmt"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/lazy-diagnose-k8s/internal/domain"
	"github.com/lazy-diagnose-k8s/internal/webhook"
)

// HandleAlert is called by the webhook server when Alertmanager fires.
// Sends alert notification with action buttons — does NOT auto-diagnose.
func (b *Bot) HandleAlert(ctx context.Context, targets []webhook.AlertTarget, payload *webhook.AlertmanagerPayload) {
	for _, chatID := range b.alertChatIDs {
		for _, target := range targets {
			b.sendAlertNotification(chatID, target, len(payload.Alerts))
		}
	}
}

func (b *Bot) sendAlertNotification(chatID int64, alertTarget webhook.AlertTarget, alertCount int) {
	ns := alertTarget.Namespace
	if ns == "" {
		ns = b.defaultNamespace
	}

	// Resource name for callback data
	resource := alertTarget.Name
	if alertTarget.Kind == "deployment" || alertTarget.Kind == "statefulset" || alertTarget.Kind == "daemonset" {
		resource = alertTarget.Name
	}

	// Format alert message
	msg := webhook.FormatAlertMessage(alertTarget, alertCount, b.alertFormat)

	// Build action buttons
	keyboard := buildAlertKeyboard(ns, resource)

	b.sendMessageWithKeyboard(chatID, msg, keyboard)

	b.logger.Info("alert notification sent",
		"chat_id", chatID,
		"alert", alertTarget.AlertName,
		"target", fmt.Sprintf("%s/%s/%s", ns, alertTarget.Kind, alertTarget.Name),
	)
}

// buildAlertKeyboard creates the 3-action button row for alerts.
func buildAlertKeyboard(ns, resource string) tgbotapi.InlineKeyboardMarkup {
	aiData := fmt.Sprintf("ai:%s:%s", ns, resource)
	staticData := fmt.Sprintf("static:%s:%s", ns, resource)
	logsData := fmt.Sprintf("logs:%s:%s", ns, resource)

	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🤖 AI Investigation", aiData),
			tgbotapi.NewInlineKeyboardButtonData("📊 Static Analysis", staticData),
			tgbotapi.NewInlineKeyboardButtonData("📜 Logs", logsData),
		),
	)
}

// buildPostDiagnosisKeyboard creates buttons shown after a diagnosis result.
// Only AI + Logs — Static already ran, Scan NS is separate concern.
func buildPostDiagnosisKeyboard(ns, resource string) tgbotapi.InlineKeyboardMarkup {
	aiData := fmt.Sprintf("ai:%s:%s", ns, resource)
	logsData := fmt.Sprintf("logs:%s:%s", ns, resource)

	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🤖 AI Investigation", aiData),
			tgbotapi.NewInlineKeyboardButtonData("📜 Logs", logsData),
		),
	)
}

func classifyAlertIntent(alertName string) domain.Intent {
	intent := domain.ClassifyIntent(alertName)
	if intent != domain.IntentUnknown {
		return intent
	}

	switch {
	case containsAny(alertName, "CrashLoopBackOff", "OOMKilled", "PodCrash", "ContainerRestart", "KubePodCrashLooping"):
		return domain.IntentCrashLoop
	case containsAny(alertName, "Pending", "Unschedulable", "FailedScheduling", "KubePodNotReady"):
		return domain.IntentPending
	case containsAny(alertName, "Rollout", "Deploy", "Revision", "KubeDeploymentReplicasMismatch"):
		return domain.IntentRolloutRegression
	default:
		return domain.IntentCrashLoop
	}
}

func containsAny(s string, substrs ...string) bool {
	lower := strings.ToLower(s)
	for _, sub := range substrs {
		if strings.Contains(lower, strings.ToLower(sub)) {
			return true
		}
	}
	return false
}
