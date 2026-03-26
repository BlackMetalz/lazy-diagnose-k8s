package domain

import "strings"

// ClassifyIntent classifies user message into a diagnosis intent.
// MVP: rule-based keyword matching.
func ClassifyIntent(text string) Intent {
	lower := strings.ToLower(text)

	// CrashLoop signals
	crashKeywords := []string{
		"crash", "crashloop", "restart", "oom", "killed",
		"backoff", "crashloopbackoff", "exit", "dying",
		"keep restarting", "bị restart", "restart liên tục",
	}
	for _, kw := range crashKeywords {
		if strings.Contains(lower, kw) {
			return IntentCrashLoop
		}
	}

	// Pending signals
	pendingKeywords := []string{
		"pending", "stuck", "scheduling", "unschedulable",
		"not starting", "không start", "bị pending",
		"schedule", "taint", "toleration", "pvc",
	}
	for _, kw := range pendingKeywords {
		if strings.Contains(lower, kw) {
			return IntentPending
		}
	}

	// Rollout regression signals
	rolloutKeywords := []string{
		"deploy", "rollout", "release", "regression",
		"sau deploy", "vừa deploy", "after deploy",
		"rollback", "revision", "5xx", "error rate",
		"vừa release", "mới deploy",
	}
	for _, kw := range rolloutKeywords {
		if strings.Contains(lower, kw) {
			return IntentRolloutRegression
		}
	}

	// Generic diagnosis — check/diag without specific keywords → default crashloop
	genericKeywords := []string{
		"check", "diag", "diagnose", "what's wrong",
		"có vấn đề", "bị gì", "lỗi gì", "sao rồi",
	}
	for _, kw := range genericKeywords {
		if strings.Contains(lower, kw) {
			return IntentCrashLoop // default to most common case
		}
	}

	return IntentUnknown
}
