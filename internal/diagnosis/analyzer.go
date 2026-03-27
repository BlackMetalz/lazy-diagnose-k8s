package diagnosis

import (
	"fmt"
	"strings"

	"github.com/lazy-diagnose-k8s/internal/domain"
)

// AnalyzeEvidence performs evidence-first diagnosis by reading K8s facts directly.
// This replaces text-pattern matching with structured fact analysis.
//
// Flow:
//  1. Read container state (CrashLoop? OOM? ImagePull? Pending?)
//  2. Read events (FailedScheduling? Probe failed? Image error details?)
//  3. Read logs — current + previous (fatal errors? missing config? connection refused?)
//  4. Read metrics (memory near limit? high restart rate?)
//  5. Correlate all sources → determine root cause
func AnalyzeEvidence(bundle *domain.EvidenceBundle) *domain.DiagnosisResult {
	result := &domain.DiagnosisResult{Target: bundle.Target}

	// Step 1: Analyze container state
	containerFinding := analyzeContainerState(bundle)

	// Step 2: Analyze events
	eventFinding := analyzeEvents(bundle)

	// Step 3: Analyze logs (K8s direct logs from container, not VictoriaLogs)
	logFinding := analyzeContainerLogs(bundle)

	// Step 4: Analyze VictoriaLogs (if K8s logs empty)
	if logFinding.ID == "" && bundle.LogsFacts != nil {
		logFinding = analyzeVictoriaLogs(bundle)
	}

	// Step 5: Analyze metrics
	metricsFinding := analyzeMetrics(bundle)

	// Step 6: Correlate — pick the best root cause
	result.PrimaryHypothesis = correlate(containerFinding, eventFinding, logFinding, metricsFinding)

	// Collect all findings as evidence lines
	result.SupportingEvidence = RedactSlice(collectDetailedEvidence(bundle))

	// Confidence based on how much data we have
	result.Confidence = calculateDetailedConfidence(result.PrimaryHypothesis, bundle)

	// Notes about missing data
	result.Notes = collectDetailedNotes(bundle)

	// Summary from the correlated finding
	result.Summary = generateSummary(result, bundle)

	return result
}

type finding struct {
	ID       string
	Name     string
	Score    int
	MaxScore int
	Signals  []string
	Detail   string // specific error message from the source
}

func (f finding) toHypothesis() *domain.HypothesisScore {
	if f.ID == "" {
		return nil
	}
	return &domain.HypothesisScore{
		ID:       f.ID,
		Name:     f.Name,
		Score:    f.Score,
		MaxScore: f.MaxScore,
		Signals:  f.Signals,
	}
}

// --- Step 1: Container State ---

func analyzeContainerState(bundle *domain.EvidenceBundle) finding {
	if bundle.K8sFacts == nil {
		return finding{}
	}

	for _, pod := range bundle.K8sFacts.PodStatuses {
		// Pending pod
		if pod.Phase == "Pending" {
			return finding{
				ID: "pending", Name: "Pod stuck in Pending",
				Score: 40, MaxScore: 100,
				Signals: []string{"pod_phase_pending"},
			}
		}

		for _, cs := range pod.ContainerStatuses {
			// OOMKilled (from current state or last termination)
			if cs.Reason == "OOMKilled" || cs.LastTermination == "OOMKilled" {
				return finding{
					ID: "oom_resource", Name: "OOM / Resource exhaustion",
					Score: 60, MaxScore: 100,
					Signals: []string{"oomkilled"},
					Detail:  fmt.Sprintf("exit_code=%d, restarts=%d", exitCode(cs), cs.RestartCount),
				}
			}

			// ImagePull errors — use container state message for specifics
			if cs.Reason == "ErrImagePull" || cs.Reason == "ImagePullBackOff" {
				f := classifyImagePullError(cs)
				return f
			}

			// Config error (missing ConfigMap, Secret, etc.)
			if cs.Reason == "CreateContainerConfigError" {
				detail := cs.Reason
				if cs.Message != "" {
					detail = cs.Message // e.g. "configmap 'app-config' not found"
				}
				return finding{
					ID: "config_env_missing", Name: "Config / Env missing",
					Score: 70, MaxScore: 100,
					Signals: []string{"config_error"},
					Detail:  detail,
				}
			}

			// Env ref errors (ConfigMap/Secret refs in spec)
			if len(cs.EnvErrors) > 0 && cs.Reason == "CreateContainerConfigError" {
				return finding{
					ID: "config_env_missing", Name: "Config / Env missing",
					Score: 70, MaxScore: 100,
					Signals: []string{"env_ref_error"},
					Detail:  strings.Join(cs.EnvErrors, "; "),
				}
			}

			// CrashLoopBackOff — need to dig deeper (logs, exit code)
			if cs.Reason == "CrashLoopBackOff" {
				// Exit code 137 = OOMKilled (SIGKILL)
				if exitCode(cs) == 137 {
					return finding{
						ID: "oom_resource", Name: "OOM / Resource exhaustion",
						Score: 50, MaxScore: 100,
						Signals: []string{"crashloop", "exit_137"},
						Detail:  fmt.Sprintf("exit_code=137 (SIGKILL/OOM), restarts=%d", cs.RestartCount),
					}
				}
				// Exit code 1 = app error (config? dependency?)
				return finding{
					ID: "app_crash", Name: "Application crash",
					Score: 30, MaxScore: 100,
					Signals: []string{"crashloop", fmt.Sprintf("exit_%d", exitCode(cs))},
					Detail:  fmt.Sprintf("exit_code=%d, restarts=%d", exitCode(cs), cs.RestartCount),
				}
			}

			// Terminated with error
			if cs.State == "terminated" && cs.ExitCode != 0 {
				return finding{
					ID: "app_crash", Name: "Application crash",
					Score: 30, MaxScore: 100,
					Signals: []string{"terminated", fmt.Sprintf("exit_%d", cs.ExitCode)},
				}
			}
		}

		// Check init containers
		for _, ics := range pod.InitContainerStatuses {
			if ics.State == "waiting" || (ics.State == "terminated" && ics.ExitCode != 0) {
				return finding{
					ID: "init_container_fail", Name: "Init container failure",
					Score: 50, MaxScore: 100,
					Signals: []string{"init_container_" + ics.State},
					Detail:  fmt.Sprintf("init container '%s': %s (exit_code=%d)", ics.Name, ics.Reason, exitCode(ics)),
				}
			}
		}
	}
	return finding{}
}

// classifyImagePullError determines the specific image pull failure from container state message.
func classifyImagePullError(cs domain.ContainerStatus) finding {
	msg := strings.ToLower(cs.Message)
	image := cs.Image

	// "not found" = tag doesn't exist
	if strings.Contains(msg, "not found") || strings.Contains(msg, "manifest unknown") {
		return finding{
			ID: "bad_image_tag", Name: "Image tag does not exist",
			Score: 70, MaxScore: 100,
			Signals: []string{"image_not_found"},
			Detail:  fmt.Sprintf("Image %s — not found in registry", image),
		}
	}

	// "unauthorized" / "denied" = auth failure
	if strings.Contains(msg, "unauthorized") || strings.Contains(msg, "denied") || strings.Contains(msg, "forbidden") {
		return finding{
			ID: "bad_image_auth", Name: "Image pull authentication failed",
			Score: 70, MaxScore: 100,
			Signals: []string{"image_auth_failed"},
			Detail:  fmt.Sprintf("Image %s — authentication failed", image),
		}
	}

	// "no such host" / "dial tcp" = registry unreachable
	if strings.Contains(msg, "no such host") || strings.Contains(msg, "dial tcp") || strings.Contains(msg, "i/o timeout") {
		return finding{
			ID: "bad_image_network", Name: "Registry unreachable",
			Score: 70, MaxScore: 100,
			Signals: []string{"registry_unreachable"},
			Detail:  fmt.Sprintf("Image %s — can't reach registry", image),
		}
	}

	// Generic image pull error
	detail := cs.Reason
	if image != "" {
		detail = fmt.Sprintf("Image %s — %s", image, cs.Reason)
	}
	return finding{
		ID: "bad_image", Name: "Image pull failure",
		Score: 60, MaxScore: 100,
		Signals: []string{"image_pull_error"},
		Detail:  detail,
	}
}

func exitCode(cs domain.ContainerStatus) int {
	if cs.ExitCode != 0 {
		return cs.ExitCode
	}
	return cs.LastExitCode
}

// --- Step 2: Events ---

func analyzeEvents(bundle *domain.EvidenceBundle) finding {
	if bundle.K8sFacts == nil {
		return finding{}
	}

	for _, ev := range bundle.K8sFacts.Events {
		if ev.Type != "Warning" {
			continue
		}
		msg := strings.ToLower(ev.Message)

		// Scheduling failures
		if ev.Reason == "FailedScheduling" {
			id := "insufficient_resources"
			name := "Insufficient cluster resources"
			if strings.Contains(msg, "untolerated taint") {
				id = "taint_mismatch"
				name = "Taint / Toleration mismatch"
			} else if strings.Contains(msg, "node affinity") || strings.Contains(msg, "node selector") {
				id = "affinity_issue"
				name = "Affinity / NodeSelector issue"
			} else if strings.Contains(msg, "persistentvolumeclaim") || strings.Contains(msg, "unbound") {
				id = "pvc_binding"
				name = "PVC binding issue"
			}
			return finding{
				ID: id, Name: name,
				Score: 60, MaxScore: 100,
				Signals: []string{"event_" + ev.Reason},
				Detail:  ev.Message,
			}
		}

		// Probe failures
		if strings.Contains(msg, "liveness probe failed") {
			return finding{
				ID: "probe_issue", Name: "Liveness probe failure",
				Score: 50, MaxScore: 100,
				Signals: []string{"event_liveness_probe_failed"},
				Detail:  ev.Message,
			}
		}
		if strings.Contains(msg, "readiness probe failed") {
			return finding{
				ID: "probe_issue", Name: "Readiness probe failure",
				Score: 40, MaxScore: 100,
				Signals: []string{"event_readiness_probe_failed"},
				Detail:  ev.Message,
			}
		}

		// Image pull specifics
		if strings.Contains(msg, "manifest unknown") || strings.Contains(msg, "not found") {
			img := extractImageFromEvent(ev.Message)
			return finding{
				ID: "bad_image_tag", Name: "Image tag does not exist",
				Score: 70, MaxScore: 100,
				Signals: []string{"event_manifest_unknown"},
				Detail:  fmt.Sprintf("Image: %s", img),
			}
		}
		if strings.Contains(msg, "unauthorized") || strings.Contains(msg, "denied") {
			img := extractImageFromEvent(ev.Message)
			return finding{
				ID: "bad_image_auth", Name: "Image pull authentication failed",
				Score: 70, MaxScore: 100,
				Signals: []string{"event_unauthorized"},
				Detail:  fmt.Sprintf("Image: %s", img),
			}
		}
	}

	return finding{}
}

// --- Step 3: Container Logs (direct from K8s) ---

func analyzeContainerLogs(bundle *domain.EvidenceBundle) finding {
	if bundle.K8sFacts == nil {
		return finding{}
	}

	// Check previous logs first (more relevant for crashed containers)
	for _, pod := range bundle.K8sFacts.PodStatuses {
		for _, cs := range pod.ContainerStatuses {
			logs := cs.PreviousLogs
			if len(logs) == 0 {
				logs = cs.CurrentLogs
			}
			if f := scanLogs(logs); f.ID != "" {
				return f
			}
		}
	}
	return finding{}
}

func analyzeVictoriaLogs(bundle *domain.EvidenceBundle) finding {
	if bundle.LogsFacts == nil {
		return finding{}
	}
	// Check top errors
	for _, pattern := range bundle.LogsFacts.TopErrors {
		if f := classifyLogLine(pattern.Sample); f.ID != "" {
			f.Detail = pattern.Sample
			return f
		}
	}
	// Check recent lines
	return scanLogs(bundle.LogsFacts.RecentLines)
}

func scanLogs(lines []string) finding {
	for _, line := range lines {
		if f := classifyLogLine(line); f.ID != "" {
			f.Detail = line
			return f
		}
	}
	return finding{}
}

func classifyLogLine(line string) finding {
	lower := strings.ToLower(line)

	// Config / env missing
	configPatterns := []string{
		"missing required", "not set", "config validation failed",
		"env not found", "undefined variable", "no such file",
		"cannot find", "configuration error", "invalid config",
	}
	for _, p := range configPatterns {
		if strings.Contains(lower, p) {
			return finding{
				ID: "config_env_missing", Name: "Config / Env missing",
				Score: 50, MaxScore: 100,
				Signals: []string{"log_config_error"},
			}
		}
	}

	// Dependency / connectivity
	connPatterns := []string{
		"connection refused", "connection timed out", "dial tcp",
		"econnrefused", "etimedout", "no such host",
		"service unavailable", "503", "connect: connection refused",
	}
	for _, p := range connPatterns {
		if strings.Contains(lower, p) {
			return finding{
				ID: "dependency_connectivity", Name: "Dependency / Connectivity issue",
				Score: 50, MaxScore: 100,
				Signals: []string{"log_connectivity_error"},
			}
		}
	}

	// OOM from application level
	oomPatterns := []string{
		"outofmemoryerror", "out of memory", "cannot allocate memory",
		"allocation failed", "heap out of memory",
	}
	for _, p := range oomPatterns {
		if strings.Contains(lower, p) {
			return finding{
				ID: "oom_resource", Name: "OOM / Resource exhaustion",
				Score: 50, MaxScore: 100,
				Signals: []string{"log_oom"},
			}
		}
	}

	// Permission errors
	if strings.Contains(lower, "permission denied") || strings.Contains(lower, "access denied") ||
		strings.Contains(lower, "unauthorized") || strings.Contains(lower, "forbidden") {
		return finding{
			ID: "permission_error", Name: "Permission / Auth error",
			Score: 40, MaxScore: 100,
			Signals: []string{"log_permission_error"},
		}
	}

	return finding{}
}

// --- Step 4: Metrics ---

func analyzeMetrics(bundle *domain.EvidenceBundle) finding {
	if bundle.MetricsFacts == nil {
		return finding{}
	}
	m := bundle.MetricsFacts

	// Memory near limit
	if m.MemoryUsage != nil && m.MemoryLimit != nil && *m.MemoryLimit > 0 {
		ratio := *m.MemoryUsage / *m.MemoryLimit
		if ratio > 0.9 {
			return finding{
				ID: "oom_resource", Name: "OOM / Resource exhaustion",
				Score: 40, MaxScore: 100,
				Signals: []string{"memory_near_limit"},
				Detail:  fmt.Sprintf("memory %.0f%% of limit", ratio*100),
			}
		}
	}

	// High restart rate
	if m.RestartRate != nil && *m.RestartRate > 2 {
		return finding{
			ID: "high_restarts", Name: "High restart rate",
			Score: 30, MaxScore: 100,
			Signals: []string{"high_restart_rate"},
			Detail:  fmt.Sprintf("%.1f restarts/15min", *m.RestartRate),
		}
	}

	return finding{}
}

// --- Step 5: Correlate ---

func correlate(findings ...finding) *domain.HypothesisScore {
	// Score each finding, boost if multiple sources agree
	best := finding{}

	// First pass: find the highest-scoring individual finding
	for _, f := range findings {
		if f.Score > best.Score {
			best = f
		}
	}

	if best.ID == "" {
		return nil
	}

	// Second pass: boost score if multiple sources agree on the same ID
	for _, f := range findings {
		if f.ID == best.ID && f.ID != "" && &f != &best {
			best.Score += 15 // correlation boost
			best.Signals = append(best.Signals, f.Signals...)
		}
	}

	// Cap score
	if best.Score > best.MaxScore {
		best.Score = best.MaxScore
	}

	return best.toHypothesis()
}

// --- Evidence collection ---

func collectDetailedEvidence(bundle *domain.EvidenceBundle) []string {
	var evidence []string

	if bundle.K8sFacts != nil {
		// Owner chain
		if len(bundle.K8sFacts.OwnerChain) > 1 {
			var chain []string
			for _, o := range bundle.K8sFacts.OwnerChain {
				chain = append(chain, o.Kind+"/"+o.Name)
			}
			evidence = append(evidence, fmt.Sprintf("Owner: %s", strings.Join(chain, " → ")))
		}

		for _, pod := range bundle.K8sFacts.PodStatuses {
			evidence = append(evidence, fmt.Sprintf("Pod %s: phase=%s, restarts=%d", pod.Name, pod.Phase, pod.RestartCount))

			// Pod conditions (show non-True conditions)
			for _, cond := range pod.Conditions {
				if cond.Status != "True" && cond.Message != "" {
					evidence = append(evidence, fmt.Sprintf("  Condition %s=%s: %s", cond.Type, cond.Status, truncateStr(cond.Message, 100)))
				}
			}

			// Main containers
			for _, cs := range pod.ContainerStatuses {
				line := fmt.Sprintf("  Container %s", cs.Name)
				if cs.Image != "" {
					line += fmt.Sprintf(" [%s]", cs.Image)
				}
				line += fmt.Sprintf(": state=%s", cs.State)
				if cs.Reason != "" {
					line += fmt.Sprintf(", reason=%s", cs.Reason)
				}
				if cs.Message != "" {
					line += fmt.Sprintf(" (%s)", truncateStr(cs.Message, 80))
				}
				if exitCode(cs) != 0 {
					line += fmt.Sprintf(", exit_code=%d", exitCode(cs))
				}
				evidence = append(evidence, line)

				if cs.LastTermination != "" {
					evidence = append(evidence, fmt.Sprintf("  Last termination: %s (exit_code=%d)", cs.LastTermination, cs.LastExitCode))
				}

				// Env ref issues
				for _, envErr := range cs.EnvErrors {
					evidence = append(evidence, fmt.Sprintf("  Env ref: %s", envErr))
				}

				// Log lines (previous first, then current)
				logs := cs.PreviousLogs
				logSource := "previous"
				if len(logs) == 0 {
					logs = cs.CurrentLogs
					logSource = "current"
				}
				errorLines := 0
				for _, line := range logs {
					if isErrorLine(line) && errorLines < 5 {
						evidence = append(evidence, fmt.Sprintf("  Log [%s]: %s", logSource, truncateStr(line, 120)))
						errorLines++
					}
				}
				// If no error lines but has logs, show last few
				if errorLines == 0 && len(logs) > 0 {
					for _, line := range lastN(logs, 3) {
						evidence = append(evidence, fmt.Sprintf("  Log [%s]: %s", logSource, truncateStr(line, 120)))
					}
				}
			}

			// Init containers
			for _, ics := range pod.InitContainerStatuses {
				if ics.State != "terminated" || ics.ExitCode != 0 || ics.Reason != "Completed" {
					evidence = append(evidence, fmt.Sprintf("  InitContainer %s: state=%s, reason=%s, exit_code=%d",
						ics.Name, ics.State, ics.Reason, exitCode(ics)))
				}
			}
		}

		// Events
		for _, ev := range bundle.K8sFacts.Events {
			if ev.Type == "Warning" {
				evidence = append(evidence, fmt.Sprintf("Event [%s]: %s (x%d)", ev.Reason, truncateStr(ev.Message, 120), ev.Count))
			}
		}

		// Rollout
		if bundle.K8sFacts.RolloutStatus != nil {
			rs := bundle.K8sFacts.RolloutStatus
			evidence = append(evidence, fmt.Sprintf("Rollout: revision=%s, desired=%d, ready=%d, updated=%d, unavailable=%d",
				rs.CurrentRevision, rs.DesiredReplicas, rs.ReadyReplicas, rs.UpdatedReplicas, rs.UnavailableReplicas))
		}

		// Resources
		if bundle.K8sFacts.ResourceRequests != nil {
			rr := bundle.K8sFacts.ResourceRequests
			evidence = append(evidence, fmt.Sprintf("Resources: cpu=%s/%s, memory=%s/%s", rr.CPURequest, rr.CPULimit, rr.MemoryRequest, rr.MemoryLimit))
		}

		// Node resources (for Pending pods)
		if bundle.K8sFacts.NodeResources != nil {
			nr := bundle.K8sFacts.NodeResources
			evidence = append(evidence, fmt.Sprintf("Cluster: %d nodes, allocatable cpu=%s memory=%s",
				nr.NodeCount, nr.AllocatableCPU, nr.AllocatableMemory))
		}
	}

	// Metrics
	if bundle.MetricsFacts != nil {
		if bundle.MetricsFacts.MemoryUsage != nil && bundle.MetricsFacts.MemoryLimit != nil {
			usageMi := *bundle.MetricsFacts.MemoryUsage / (1024 * 1024)
			limitMi := *bundle.MetricsFacts.MemoryLimit / (1024 * 1024)
			evidence = append(evidence, fmt.Sprintf("Memory: %.0fMi / %.0fMi (%.0f%%)", usageMi, limitMi, usageMi/limitMi*100))
		}
		if bundle.MetricsFacts.RestartRate != nil {
			evidence = append(evidence, fmt.Sprintf("Restart rate: %.1f/15min", *bundle.MetricsFacts.RestartRate))
		}
	}

	return evidence
}

func calculateDetailedConfidence(hypothesis *domain.HypothesisScore, bundle *domain.EvidenceBundle) domain.Confidence {
	if hypothesis == nil {
		return domain.ConfidenceLow
	}

	score := hypothesis.Score
	base := domain.ConfidenceLow

	if score >= 60 {
		base = domain.ConfidenceHigh
	} else if score >= 35 {
		base = domain.ConfidenceMedium
	}

	// Degrade if missing sources
	if !bundle.HasK8s() {
		if base == domain.ConfidenceHigh {
			base = domain.ConfidenceMedium
		}
	}

	return base
}

func collectDetailedNotes(bundle *domain.EvidenceBundle) []string {
	var notes []string
	for _, ps := range bundle.ProviderStatuses {
		if !ps.Available {
			notes = append(notes, fmt.Sprintf("⚠ Provider '%s' unavailable: %s", ps.Name, ps.Error))
		}
	}
	// Check if we got container logs
	hasContainerLogs := false
	if bundle.K8sFacts != nil {
		for _, pod := range bundle.K8sFacts.PodStatuses {
			for _, cs := range pod.ContainerStatuses {
				if len(cs.CurrentLogs) > 0 || len(cs.PreviousLogs) > 0 {
					hasContainerLogs = true
				}
			}
		}
	}
	if !hasContainerLogs && !bundle.HasLogs() {
		notes = append(notes, "No logs available. Diagnosis based on K8s status and events only.")
	}
	return notes
}

func generateSummary(result *domain.DiagnosisResult, bundle *domain.EvidenceBundle) string {
	if result.PrimaryHypothesis == nil {
		return "No clear anomaly detected. Check manually using the commands below."
	}

	h := result.PrimaryHypothesis
	var parts []string

	switch h.ID {
	case "oom_resource":
		parts = append(parts, "Container OOMKilled.")
		if bundle.K8sFacts != nil && bundle.K8sFacts.ResourceRequests != nil && bundle.K8sFacts.ResourceRequests.MemoryLimit != "" {
			parts = append(parts, fmt.Sprintf("Memory limit: %s.", bundle.K8sFacts.ResourceRequests.MemoryLimit))
		}
		parts = addRestartCount(parts, bundle)
		parts = addTopLogError(parts, bundle)
		parts = append(parts, "Increase memory limit or fix memory leak.")

	case "config_env_missing":
		parts = append(parts, "Container crashes on startup — missing config or environment variable.")
		parts = addTopLogError(parts, bundle)
		parts = addRestartCount(parts, bundle)

	case "dependency_connectivity":
		parts = append(parts, "Container can't reach dependency.")
		parts = addTopLogError(parts, bundle)
		parts = addRestartCount(parts, bundle)

	case "bad_image", "bad_image_tag":
		img := findContainerImage(bundle)
		if img != "" {
			parts = append(parts, fmt.Sprintf("Image tag does not exist: %s.", img))
		} else {
			parts = append(parts, "Container can't pull image — tag doesn't exist.")
		}
		parts = append(parts, "Verify the tag is published in the registry.")

	case "bad_image_auth":
		img := findContainerImage(bundle)
		if img != "" {
			parts = append(parts, fmt.Sprintf("Authentication failed pulling %s.", img))
		} else {
			parts = append(parts, "Image pull authentication failed.")
		}
		parts = append(parts, "Check imagePullSecrets and registry credentials.")

	case "bad_image_network":
		img := findContainerImage(bundle)
		if img != "" {
			parts = append(parts, fmt.Sprintf("Can't reach registry for %s.", img))
		} else {
			parts = append(parts, "Can't reach container registry.")
		}
		parts = append(parts, "Check registry hostname and network/DNS connectivity.")

	case "probe_issue":
		parts = append(parts, "Container killed by probe failure.")
		parts = addEventDetail(parts, bundle, "liveness probe failed", "readiness probe failed")
		parts = append(parts, "Check probe config — initialDelaySeconds may be too short.")

	case "insufficient_resources":
		parts = append(parts, "Pod can't be scheduled — insufficient cluster resources.")
		parts = addSchedulerDetail(parts, bundle)
		parts = append(parts, "Scale up cluster or reduce resource requests.")

	case "taint_mismatch":
		parts = append(parts, "Pod can't be scheduled — node taint not tolerated.")
		parts = addSchedulerDetail(parts, bundle)

	case "affinity_issue":
		parts = append(parts, "Pod can't be scheduled — no node matches affinity/nodeSelector.")
		parts = addSchedulerDetail(parts, bundle)

	case "pvc_binding":
		parts = append(parts, "Pod stuck Pending — PVC can't bind.")
		parts = addSchedulerDetail(parts, bundle)

	case "permission_error":
		parts = append(parts, "Container hit a permission/auth error.")
		parts = addTopLogError(parts, bundle)

	case "app_crash":
		parts = append(parts, "Container crashed.")
		parts = addTopLogError(parts, bundle)
		parts = addRestartCount(parts, bundle)
		parts = append(parts, "Check logs for root cause.")

	case "init_container_fail":
		parts = append(parts, "Init container failed — main container hasn't started.")
		parts = addTopLogError(parts, bundle)

	case "high_restarts":
		parts = append(parts, "Container has high restart rate.")
		parts = addTopLogError(parts, bundle)
		parts = addRestartCount(parts, bundle)

	default:
		parts = append(parts, fmt.Sprintf("Issue detected: %s.", h.Name))
		parts = addTopLogError(parts, bundle)
	}

	return strings.Join(parts, " ")
}

// --- helpers ---

func addRestartCount(parts []string, bundle *domain.EvidenceBundle) []string {
	if bundle.K8sFacts == nil {
		return parts
	}
	for _, pod := range bundle.K8sFacts.PodStatuses {
		if pod.RestartCount > 0 {
			return append(parts, fmt.Sprintf("Restarted %d times.", pod.RestartCount))
		}
	}
	return parts
}

func addTopLogError(parts []string, bundle *domain.EvidenceBundle) []string {
	// Try K8s container logs first
	if bundle.K8sFacts != nil {
		for _, pod := range bundle.K8sFacts.PodStatuses {
			for _, cs := range pod.ContainerStatuses {
				logs := cs.PreviousLogs
				if len(logs) == 0 {
					logs = cs.CurrentLogs
				}
				for _, line := range logs {
					if isErrorLine(line) {
						return append(parts, fmt.Sprintf("Log: %s", truncateStr(line, 150)))
					}
				}
			}
		}
	}
	// Fallback to VictoriaLogs
	if bundle.LogsFacts != nil && len(bundle.LogsFacts.TopErrors) > 0 {
		return append(parts, fmt.Sprintf("Log: %s", truncateStr(bundle.LogsFacts.TopErrors[0].Sample, 150)))
	}
	return parts
}

func addEventDetail(parts []string, bundle *domain.EvidenceBundle, patterns ...string) []string {
	if bundle.K8sFacts == nil {
		return parts
	}
	for _, ev := range bundle.K8sFacts.Events {
		lower := strings.ToLower(ev.Message)
		for _, p := range patterns {
			if strings.Contains(lower, p) {
				return append(parts, fmt.Sprintf("Event: %s", truncateStr(ev.Message, 120)))
			}
		}
	}
	return parts
}

func addSchedulerDetail(parts []string, bundle *domain.EvidenceBundle) []string {
	if bundle.K8sFacts == nil {
		return parts
	}
	for _, ev := range bundle.K8sFacts.Events {
		if ev.Reason == "FailedScheduling" {
			return append(parts, fmt.Sprintf("Scheduler: %s", truncateStr(ev.Message, 150)))
		}
	}
	return parts
}

// findContainerImage gets the image from container status (more reliable than parsing events).
func findContainerImage(bundle *domain.EvidenceBundle) string {
	if bundle.K8sFacts == nil {
		return ""
	}
	for _, pod := range bundle.K8sFacts.PodStatuses {
		for _, cs := range pod.ContainerStatuses {
			if cs.Image != "" && (cs.Reason == "ErrImagePull" || cs.Reason == "ImagePullBackOff" ||
				cs.Reason == "CrashLoopBackOff" || cs.State == "terminated") {
				return cs.Image
			}
		}
	}
	// Fallback: any container with an image
	for _, pod := range bundle.K8sFacts.PodStatuses {
		for _, cs := range pod.ContainerStatuses {
			if cs.Image != "" {
				return cs.Image
			}
		}
	}
	return ""
}

func extractImageFromEvents(bundle *domain.EvidenceBundle) string {
	if bundle.K8sFacts == nil {
		return ""
	}
	for _, ev := range bundle.K8sFacts.Events {
		if img := extractImageFromEvent(ev.Message); img != "" {
			return img
		}
	}
	return ""
}

func isErrorLine(line string) bool {
	lower := strings.ToLower(line)
	keywords := []string{"error", "fatal", "panic", "exception", "fail", "refused", "timeout", "denied", "missing", "not found", "not set", "killed"}
	for _, kw := range keywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

func lastN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
