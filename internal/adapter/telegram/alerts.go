package telegram

import (
	"context"
	"fmt"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
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

	// Determine cluster from alert labels or use default
	clusterName := alertTarget.Cluster
	if clusterName == "" {
		clusterName = b.defaultCluster
	}

	// Format alert message
	msg := webhook.FormatAlertMessage(alertTarget, alertCount, b.alertFormat)

	// Build action buttons
	keyboard := buildAlertKeyboard(clusterName, ns, resource, b.getCluster(clusterName).Engine.HasHolmes())

	b.sendMessageWithKeyboard(chatID, msg, keyboard)

	b.logger.Info("alert notification sent",
		"chat_id", chatID,
		"alert", alertTarget.AlertName,
		"target", fmt.Sprintf("%s/%s/%s", ns, alertTarget.Kind, alertTarget.Name),
	)
}

// buildAlertKeyboard creates the action button rows for alerts.
// Callback data format: "action:cluster:ns:name"
func buildAlertKeyboard(cluster, ns, resource string, hasHolmes bool) tgbotapi.InlineKeyboardMarkup {
	aiData := fmt.Sprintf("ai:%s:%s:%s", cluster, ns, resource)
	staticData := fmt.Sprintf("static:%s:%s:%s", cluster, ns, resource)
	logsData := fmt.Sprintf("logs:%s:%s:%s", cluster, ns, resource)

	row := tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("🤖 AI", aiData),
		tgbotapi.NewInlineKeyboardButtonData("📊 Static", staticData),
		tgbotapi.NewInlineKeyboardButtonData("📜 Logs", logsData),
	)

	if hasHolmes {
		deepData := fmt.Sprintf("deep:%s:%s:%s", cluster, ns, resource)
		row = append(row, tgbotapi.NewInlineKeyboardButtonData("🔬 Deep", deepData))
	}

	return tgbotapi.NewInlineKeyboardMarkup(row)
}

// buildPostDiagnosisKeyboard creates follow-up buttons after a diagnosis result.
// Shows the actions the user hasn't just run, so they can try alternatives.
// completedAction: "ai", "static", "logs", or "deep" — the action that just finished.
func buildPostDiagnosisKeyboard(cluster, ns, resource, completedAction string, hasHolmes bool) tgbotapi.InlineKeyboardMarkup {
	aiData := fmt.Sprintf("ai:%s:%s:%s", cluster, ns, resource)
	staticData := fmt.Sprintf("static:%s:%s:%s", cluster, ns, resource)
	logsData := fmt.Sprintf("logs:%s:%s:%s", cluster, ns, resource)

	var buttons []tgbotapi.InlineKeyboardButton
	if completedAction != "ai" {
		buttons = append(buttons, tgbotapi.NewInlineKeyboardButtonData("🤖 AI", aiData))
	}
	if completedAction != "static" {
		buttons = append(buttons, tgbotapi.NewInlineKeyboardButtonData("📊 Static", staticData))
	}
	if completedAction != "logs" {
		buttons = append(buttons, tgbotapi.NewInlineKeyboardButtonData("📜 Logs", logsData))
	}
	if hasHolmes && completedAction != "deep" {
		deepData := fmt.Sprintf("deep:%s:%s:%s", cluster, ns, resource)
		buttons = append(buttons, tgbotapi.NewInlineKeyboardButtonData("🔬 Deep", deepData))
	}

	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(buttons...),
	)
}

