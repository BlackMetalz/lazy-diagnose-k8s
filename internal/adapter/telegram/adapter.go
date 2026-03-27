package telegram

import (
	"fmt"
	"strings"
	"time"

	"github.com/lazy-diagnose-k8s/internal/domain"
)

// ParsedMessage represents a parsed Telegram message.
type ParsedMessage struct {
	Command   string // "diag", "pod", "deploy", "check", "scan", or empty for free text
	Target    string // the target resource or service name
	Namespace string // optional namespace override (-n flag)
	RawText   string
}

// ParseMessage extracts command, target, and namespace from a Telegram message.
// Supported formats:
//
//	/check checkout
//	/check checkout -n staging
//	/diag payment just deployed, seeing 5xx
//	/pod payment-api-7f8b9c-x4k2p
//	/deploy checkout
//	/scan                          (scan default namespace)
//	/scan prod                     (scan specific namespace)
//	checkout is crashing           (free text)
func ParseMessage(text string) ParsedMessage {
	text = strings.TrimSpace(text)
	if text == "" {
		return ParsedMessage{RawText: text}
	}

	// Handle slash commands
	if strings.HasPrefix(text, "/") {
		parts := strings.Fields(text)
		cmd := strings.TrimPrefix(parts[0], "/")
		// Strip @botname suffix (e.g. /check@mybot → check)
		if idx := strings.Index(cmd, "@"); idx > 0 {
			cmd = cmd[:idx]
		}

		switch cmd {
		case "scan":
			ns := ""
			if len(parts) > 1 {
				ns = parts[1]
			}
			return ParsedMessage{
				Command:   "scan",
				Namespace: ns,
				RawText:   text,
			}
		case "diag", "check", "pod", "deploy":
			target, ns := extractTargetAndNamespace(parts[1:])
			return ParsedMessage{
				Command:   cmd,
				Target:    target,
				Namespace: ns,
				RawText:   text,
			}
		case "start", "help":
			return ParsedMessage{
				Command: cmd,
				RawText: text,
			}
		}
	}

	// Free text: try to extract target (first word that looks like a resource name)
	words := strings.Fields(text)
	target := ""
	for _, w := range words {
		// Skip common Vietnamese/English noise words
		if isNoiseWord(w) {
			continue
		}
		target = w
		break
	}

	return ParsedMessage{
		Command: "check", // default to generic check
		Target:  target,
		RawText: text,
	}
}

// extractTargetAndNamespace parses [target] [-n namespace] from args.
func extractTargetAndNamespace(args []string) (target, namespace string) {
	for i := 0; i < len(args); i++ {
		if (args[i] == "-n" || args[i] == "--namespace") && i+1 < len(args) {
			namespace = args[i+1]
			i++ // skip next
			continue
		}
		if target == "" {
			target = args[i]
		}
	}
	return target, namespace
}

func isNoiseWord(w string) bool {
	noise := map[string]bool{
		"check": true, "kiểm": true, "tra": true, "xem": true,
		"sao": true, "rồi": true, "bị": true, "gì": true,
		"có": true, "vấn": true, "đề": true, "không": true,
		"the": true, "is": true, "a": true, "an": true,
		"what": true, "why": true, "how": true,
	}
	return noise[strings.ToLower(w)]
}

// FormatResult formats a DiagnosisResult with a header (for /check commands).
func FormatResult(result *domain.DiagnosisResult) string {
	// Header: badge + first pod name (not deployment path)
	badge := confidenceBadge(result.Confidence)
	podName := firstPodName(result)
	header := fmt.Sprintf("%s <b>%s</b>\n─────────────────────\n\n", badge, esc(podName))
	return header + formatResultBody(result)
}

// FormatResultCompact formats a DiagnosisResult without header (for callbacks where Re: is prepended).
func FormatResultCompact(result *domain.DiagnosisResult) string {
	badge := confidenceBadge(result.Confidence)
	return badge + " " + formatResultBody(result)
}

// firstPodName extracts the first pod name from evidence, or falls back to target name.
func firstPodName(result *domain.DiagnosisResult) string {
	for _, e := range result.SupportingEvidence {
		// Evidence lines start with "Pod <name>:"
		if len(e) > 4 && e[:4] == "Pod " {
			if idx := strings.Index(e, ":"); idx > 0 {
				return e[4:idx]
			}
		}
	}
	// Fallback to target resource name
	if result.Target != nil {
		return result.Target.ResourceName
	}
	return "unknown"
}

func formatResultBody(result *domain.DiagnosisResult) string {
	var b strings.Builder

	// ── Summary (the most important part) ──
	b.WriteString(fmt.Sprintf("%s\n\n", esc(result.Summary)))

	// ── Target info (where to fix) ──
	b.WriteString(formatTargetInfo(result))

	// ── Key facts (compact) ──
	if result.PrimaryHypothesis != nil {
		h := result.PrimaryHypothesis
		pct := 0
		if h.MaxScore > 0 {
			pct = h.Score * 100 / h.MaxScore
		}
		b.WriteString(fmt.Sprintf("<b>Root cause:</b> %s (%d%%)\n", esc(h.Name), pct))
	}

	// Show alternative only if score > 0
	for _, h := range result.AlternativeHypotheses {
		if h.Score > 0 {
			pct := 0
			if h.MaxScore > 0 {
				pct = h.Score * 100 / h.MaxScore
			}
			b.WriteString(fmt.Sprintf("<b>Also possible:</b> %s (%d%%)\n", esc(h.Name), pct))
		}
	}
	b.WriteString("\n")

	// ── Evidence (pre-formatted block) ──
	if len(result.SupportingEvidence) > 0 {
		b.WriteString("<b>Evidence:</b>\n<pre>")
		for _, e := range result.SupportingEvidence {
			b.WriteString(esc(e) + "\n")
		}
		b.WriteString("</pre>\n")
	}

	// ── Next steps ──
	if len(result.RecommendedSteps) > 0 {
		b.WriteString("<b>Next steps:</b>\n")
		for i, step := range result.RecommendedSteps {
			b.WriteString(fmt.Sprintf("  %d. %s\n", i+1, esc(step)))
		}
		b.WriteString("\n")
	}

	// ── Commands (copy-paste ready) ──
	if len(result.SuggestedCommands) > 0 {
		b.WriteString("<b>Commands:</b>\n<pre>")
		for _, cmd := range result.SuggestedCommands {
			b.WriteString(esc(cmd) + "\n")
		}
		b.WriteString("</pre>\n")
	}

	// ── Notes (degraded mode warnings) ──
	if len(result.Notes) > 0 {
		for _, note := range result.Notes {
			b.WriteString(fmt.Sprintf("<i>%s</i>\n", esc(note)))
		}
	}

	// ── Footer ──
	b.WriteString(fmt.Sprintf("\n<i>Confidence: %s · Analyzed in %s</i>", result.Confidence, result.Duration.Round(time.Millisecond)))

	return b.String()
}

// ScanResult holds one unhealthy pod found during scan.
type ScanResult struct {
	Name      string
	Namespace string
	Reason    string
	Restarts  int
	OwnerKind string
	OwnerName string
}

// FormatScanResult formats the scan output for Telegram.
func FormatScanResult(namespace string, results []ScanResult, duration time.Duration) string {
	var b strings.Builder

	if len(results) == 0 {
		b.WriteString(fmt.Sprintf("✅ <b>Scan: %s</b>\n", esc(namespace)))
		b.WriteString("─────────────────────\n\n")
		b.WriteString("All pods healthy. No issues found.\n")
		b.WriteString(fmt.Sprintf("\n<i>%s</i>", duration.Round(time.Millisecond)))
		return b.String()
	}

	b.WriteString(fmt.Sprintf("🔍 <b>Scan: %s</b> — %d issue(s)\n", esc(namespace), len(results)))
	b.WriteString("─────────────────────\n\n")

	showNs := namespace == "all" // show namespace per pod when scanning all

	for _, r := range results {
		icon := reasonIcon(r.Reason)
		podLabel := r.Name
		if showNs {
			podLabel = r.Namespace + "/" + r.Name
		}
		restartInfo := ""
		if r.Restarts > 0 {
			restartInfo = fmt.Sprintf(" · %dx restarts", r.Restarts)
		}
		b.WriteString(fmt.Sprintf("%s <code>%s</code>\n   %s%s\n\n",
			icon, esc(podLabel), esc(r.Reason), esc(restartInfo)))
	}

	// Suggest check commands
	b.WriteString("<b>Diagnose:</b>\n")
	suggested := 0
	seen := make(map[string]bool)
	for _, r := range results {
		name := r.Name
		ns := r.Namespace
		if ns == "" {
			ns = namespace
		}
		key := ns + "/" + name
		if seen[key] {
			continue
		}
		seen[key] = true
		b.WriteString(fmt.Sprintf("  <code>/check %s -n %s</code>\n", esc(name), esc(ns)))
		suggested++
		if suggested >= 3 {
			break
		}
	}

	b.WriteString(fmt.Sprintf("\n<i>%s</i>", duration.Round(time.Millisecond)))
	return b.String()
}

func reasonIcon(reason string) string {
	switch {
	case strings.Contains(reason, "CrashLoop") || strings.Contains(reason, "OOMKilled"):
		return "🔴"
	case strings.Contains(reason, "ImagePull") || strings.Contains(reason, "ErrImage"):
		return "🟠"
	case strings.Contains(reason, "Pending") || strings.Contains(reason, "Unschedulable"):
		return "🟡"
	case strings.Contains(reason, "Restarting"):
		return "🟡"
	default:
		return "⚪"
	}
}

// formatTargetInfo shows where to fix — namespace, deployment, container, image, resources.
func formatTargetInfo(result *domain.DiagnosisResult) string {
	if result.Target == nil {
		return ""
	}

	var b strings.Builder
	b.WriteString("<b>Target:</b>\n")
	b.WriteString(fmt.Sprintf("  Namespace:  <code>%s</code>\n", esc(result.Target.Namespace)))
	if result.Target.Kind != "" {
		b.WriteString(fmt.Sprintf("  %s: <code>%s</code>\n", esc(capitalize(result.Target.Kind)), esc(result.Target.ResourceName)))
	}

	// Extract container + image + resources from evidence bundle (stored in SupportingEvidence)
	for _, e := range result.SupportingEvidence {
		// Container line: "Container checkout [polinux/stress]: state=..."
		if strings.HasPrefix(e, "  Container ") {
			// Extract container name and image
			if idx := strings.Index(e, "["); idx > 0 {
				if end := strings.Index(e[idx:], "]"); end > 0 {
					image := e[idx+1 : idx+end]
					container := strings.TrimSpace(e[len("  Container "):idx])
					b.WriteString(fmt.Sprintf("  Container: <code>%s</code>\n", esc(container)))
					b.WriteString(fmt.Sprintf("  Image:     <code>%s</code>\n", esc(image)))
				}
			}
			break
		}
	}

	// Resources line from evidence
	for _, e := range result.SupportingEvidence {
		if strings.HasPrefix(e, "  Resources:") || strings.HasPrefix(e, "Resources:") {
			b.WriteString(fmt.Sprintf("  %s\n", esc(strings.TrimSpace(e))))
			break
		}
	}

	b.WriteString("\n")
	return b.String()
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func confidenceBadge(c domain.Confidence) string {
	switch c {
	case domain.ConfidenceHigh:
		return "🔴"
	case domain.ConfidenceMedium:
		return "🟡"
	default:
		return "⚪"
	}
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// FormatHelpMessage returns the help text for the bot.
func FormatHelpMessage() string {
	return `🤖 <b>lazy-diagnose-k8s</b>

Kubernetes diagnosis via Telegram. Collects data from K8s, metrics, and logs — returns diagnosis + suggested commands.

<b>Commands:</b>
/scan [namespace] — Find all unhealthy pods
/check &lt;target&gt; [-n ns] — General health check
/diag &lt;target&gt; &lt;context&gt; — Diagnosis with description
/pod &lt;pod-name&gt; — Check a specific pod
/deploy &lt;deployment&gt; — Check rollout status
/help — This message

<b>Examples:</b>
• <code>/scan</code> — scan default namespace
• <code>/scan prod</code> — scan specific namespace
• <code>/check checkout</code>
• <code>/check checkout -n staging</code>
• <code>/diag payment just deployed, seeing 5xx</code>
• <code>/deploy checkout</code>

<b>What it detects:</b>
• CrashLoop — OOM, missing config, dependency fail, probe issue, bad image
• Pending — insufficient resources, taint/affinity, PVC, quota
• Rollout regression — failed deploy, image pull error, resource pressure

<b>Target can be:</b>
• Service name from service_map (checkout, payment, worker...)
• Exact deployment/pod name
• Path format: <code>deployment/checkout</code> or <code>prod/deployment/checkout</code>

<b>Reading results:</b>
🔴 High confidence — clear root cause found
🟡 Medium — likely cause, some data missing
⚪ Low — inconclusive, check manually`
}

// FormatError formats an error message for Telegram.
func FormatError(err error) string {
	return fmt.Sprintf("❌ <b>Error:</b> %s", esc(err.Error()))
}

// esc escapes HTML special characters for Telegram HTML mode.
func esc(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}
