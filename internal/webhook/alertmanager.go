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
	Name      string
	Namespace string
	Kind      string // deployment, pod, statefulset, etc.
	AlertName string
	Severity  string
	Summary   string
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
	}

	// Summary from annotations
	if v, ok := alert.Annotations["summary"]; ok {
		t.Summary = v
	} else if v, ok := alert.Annotations["description"]; ok {
		t.Summary = v
	}

	// Try to find the K8s target from labels
	// Priority: pod > deployment > statefulset > daemonset > container

	t.Namespace = firstOf(alert.Labels, "namespace", "exported_namespace")

	// Pod-level alerts (kube-state-metrics)
	if pod := firstOf(alert.Labels, "pod", "pod_name"); pod != "" {
		t.Name = pod
		t.Kind = "pod"
		// Try to get owner (deployment name) from pod name
		// e.g. checkout-7f8b9c-x4k2p → checkout
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

// FormatAlertMessage formats an alert for Telegram display.
func FormatAlertMessage(target AlertTarget, alertCount int) string {
	var b strings.Builder

	severity := strings.ToUpper(target.Severity)
	if severity == "" {
		severity = "ALERT"
	}

	icon := "🔔"
	switch strings.ToLower(target.Severity) {
	case "critical":
		icon = "🚨"
	case "warning":
		icon = "⚠️"
	}

	b.WriteString(fmt.Sprintf("%s <b>[%s] %s</b>\n", icon, severity, esc(target.AlertName)))

	if target.Namespace != "" {
		b.WriteString(fmt.Sprintf("Target: <code>%s/%s/%s</code>\n", esc(target.Namespace), esc(target.Kind), esc(target.Name)))
	} else {
		b.WriteString(fmt.Sprintf("Target: <code>%s/%s</code>\n", esc(target.Kind), esc(target.Name)))
	}

	if target.Summary != "" {
		b.WriteString(fmt.Sprintf("\n%s\n", esc(target.Summary)))
	}

	if alertCount > 1 {
		b.WriteString(fmt.Sprintf("\n<i>(%d alerts in this group)</i>\n", alertCount))
	}

	return b.String()
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
