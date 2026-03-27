package webhook

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// AlertmanagerPayload is the webhook payload from Alertmanager.
// See: https://prometheus.io/docs/alerting/latest/configuration/#webhook_config
type AlertmanagerPayload struct {
	Version           string            `json:"version"`
	GroupKey          string            `json:"groupKey"`
	TruncatedAlerts   int               `json:"truncatedAlerts"`
	Status            string            `json:"status"` // "firing" or "resolved"
	Receiver          string            `json:"receiver"`
	GroupLabels       map[string]string  `json:"groupLabels"`
	CommonLabels      map[string]string  `json:"commonLabels"`
	CommonAnnotations map[string]string  `json:"commonAnnotations"`
	ExternalURL       string            `json:"externalURL"`
	Alerts            []Alert           `json:"alerts"`
}

// Alert is a single alert from Alertmanager.
type Alert struct {
	Status       string            `json:"status"` // "firing" or "resolved"
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
	Fingerprint  string            `json:"fingerprint"`
}

// ParseAlertmanagerPayload parses the JSON webhook body.
func ParseAlertmanagerPayload(body []byte) (*AlertmanagerPayload, error) {
	var payload AlertmanagerPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parse alertmanager payload: %w", err)
	}
	return &payload, nil
}

// AlertTarget extracts the K8s target from alert labels.
// Tries common label patterns used in kube-state-metrics and recording rules.
type AlertTarget struct {
	Name        string
	Namespace   string
	Kind        string // deployment, pod, statefulset, etc.
	PodName     string // specific pod name if available
	Container   string // container name if available
	AlertName   string
	Severity    string
	Summary     string
	Description string
	StartsAt    time.Time
}

// Fingerprint returns a short identifier for deduplication.
func (t AlertTarget) Fingerprint() string {
	return fmt.Sprintf("%s-%s-%s", t.Namespace, t.Kind, t.Name)
}

// ExtractTargets pulls diagnosis targets from an Alertmanager payload.
// Returns one AlertTarget per unique (namespace, name) pair.
func ExtractTargets(payload *AlertmanagerPayload) []AlertTarget {
	seen := make(map[string]bool)
	var targets []AlertTarget

	for _, alert := range payload.Alerts {
		if alert.Status != "firing" {
			continue
		}

		t := extractTarget(alert)
		if t.Name == "" {
			continue
		}

		key := t.Namespace + "/" + t.Name
		if seen[key] {
			continue
		}
		seen[key] = true
		targets = append(targets, t)
	}

	return targets
}

func extractTarget(alert Alert) AlertTarget {
	t := AlertTarget{
		AlertName: alert.Labels["alertname"],
		Severity:  alert.Labels["severity"],
		StartsAt:  alert.StartsAt,
	}

	// Summary + description from annotations
	t.Summary = firstOf(alert.Annotations, "summary")
	t.Description = firstOf(alert.Annotations, "description")

	// Container name
	t.Container = firstOf(alert.Labels, "container", "container_name")

	// Try to find the K8s target from labels
	// Priority: pod > deployment > statefulset > daemonset > container

	t.Namespace = firstOf(alert.Labels, "namespace", "exported_namespace")

	// Pod-level alerts (kube-state-metrics)
	if pod := firstOf(alert.Labels, "pod", "pod_name"); pod != "" {
		t.PodName = pod
		t.Name = pod
		t.Kind = "pod"
		// Try to get owner (deployment name)
		if deploy := firstOf(alert.Labels, "deployment", "created_by_name"); deploy != "" {
			t.Name = deploy
			t.Kind = "deployment"
		}
		return t
	}

	// Deployment-level alerts
	if deploy := firstOf(alert.Labels, "deployment"); deploy != "" {
		t.Name = deploy
		t.Kind = "deployment"
		return t
	}

	// StatefulSet
	if sts := firstOf(alert.Labels, "statefulset"); sts != "" {
		t.Name = sts
		t.Kind = "statefulset"
		return t
	}

	// DaemonSet
	if ds := firstOf(alert.Labels, "daemonset"); ds != "" {
		t.Name = ds
		t.Kind = "daemonset"
		return t
	}

	// Container-level (try to derive deployment)
	if container := firstOf(alert.Labels, "container", "container_name"); container != "" {
		t.Name = container
		t.Kind = "deployment" // guess
		return t
	}

	// Job
	if job := firstOf(alert.Labels, "job_name"); job != "" {
		t.Name = job
		t.Kind = "job"
		return t
	}

	return t
}

// AlertFormatConfig holds display settings for alert messages.
type AlertFormatConfig struct {
	BotName     string
	ClusterName string
}

// FormatAlertMessage formats an alert for Telegram display.
func FormatAlertMessage(target AlertTarget, alertCount int, cfg AlertFormatConfig) string {
	var b strings.Builder

	icon := severityIcon(target.Severity)
	severity := strings.ToUpper(target.Severity)
	if severity == "" {
		severity = "ALERT"
	}

	botName := cfg.BotName
	if botName == "" {
		botName = "lazy-diagnose-k8s"
	}

	// Header: icon + bot name + severity badge
	b.WriteString(fmt.Sprintf("%s <b>%s</b> · <code>%s</code>\n", icon, esc(botName), severity))
	b.WriteString("─────────────────────\n")

	// Alert name (prominent)
	b.WriteString(fmt.Sprintf("<b>%s</b>\n\n", esc(target.AlertName)))

	// Target info block (compact key-value with code formatting)
	if cfg.ClusterName != "" {
		b.WriteString(fmt.Sprintf("Cluster:    <code>%s</code>\n", esc(cfg.ClusterName)))
	}
	if target.Namespace != "" {
		b.WriteString(fmt.Sprintf("Namespace:  <code>%s</code>\n", esc(target.Namespace)))
	}
	if target.PodName != "" {
		b.WriteString(fmt.Sprintf("Pod:        <code>%s</code>\n", esc(target.PodName)))
	} else if target.Name != "" {
		b.WriteString(fmt.Sprintf("%-11s <code>%s</code>\n", capitalize(target.Kind)+":", esc(target.Name)))
	}
	if target.Container != "" {
		b.WriteString(fmt.Sprintf("Container:  <code>%s</code>\n", esc(target.Container)))
	}

	// Message (cleaned, in a separate block)
	msg := target.Description
	if msg == "" {
		msg = target.Summary
	}
	if msg != "" {
		msg = cleanMessage(msg, target)
		if msg != "" {
			b.WriteString(fmt.Sprintf("\n<i>%s</i>\n", esc(msg)))
		}
	}

	// Footer: firing duration + alert count
	var footer []string
	if !target.StartsAt.IsZero() {
		dur := time.Since(target.StartsAt).Round(time.Second)
		footer = append(footer, fmt.Sprintf("🔥 %s", formatDuration(dur)))
	}
	if alertCount > 1 {
		footer = append(footer, fmt.Sprintf("%d alerts", alertCount))
	}
	if len(footer) > 0 {
		b.WriteString(fmt.Sprintf("\n%s\n", strings.Join(footer, " · ")))
	}

	return b.String()
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// AlertRef returns a short one-line reference for quoting in investigation results.
func AlertRef(target AlertTarget) string {
	ref := target.AlertName
	if target.Namespace != "" && target.Name != "" {
		ref += " · " + target.Namespace + "/" + target.Name
	}
	return ref
}

// cleanMessage removes redundant namespace/pod info that's already shown in the target line.
func cleanMessage(msg string, target AlertTarget) string {
	// Remove patterns like "Pod namespace/podname" since already displayed
	if target.PodName != "" {
		msg = strings.ReplaceAll(msg, target.Namespace+"/"+target.PodName, target.PodName)
		msg = strings.ReplaceAll(msg, "Pod "+target.PodName+" ", "")
		msg = strings.ReplaceAll(msg, "pod "+target.PodName+" ", "")
	}
	if target.Container != "" {
		msg = strings.ReplaceAll(msg, "Container "+target.Container+" in ", "")
		msg = strings.ReplaceAll(msg, "container "+target.Container+" in ", "")
	}
	msg = strings.TrimSpace(msg)
	// Remove leading "in pod xyz" if it starts with that
	if strings.HasPrefix(msg, "in pod ") || strings.HasPrefix(msg, "in Pod ") {
		if idx := strings.Index(msg, " "); idx > 0 {
			if idx2 := strings.Index(msg[idx+1:], " "); idx2 > 0 {
				msg = strings.TrimSpace(msg[idx+1+idx2:])
			}
		}
	}
	return msg
}

func severityIcon(severity string) string {
	switch strings.ToLower(severity) {
	case "critical":
		return "🚨"
	case "warning":
		return "⚠️"
	case "info":
		return "ℹ️"
	default:
		return "🔔"
	}
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	hours := int(d.Hours())
	mins := int(d.Minutes()) % 60
	if mins == 0 {
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dh%dm", hours, mins)
}

func firstOf(m map[string]string, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != "" {
			return v
		}
	}
	return ""
}

func esc(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}
