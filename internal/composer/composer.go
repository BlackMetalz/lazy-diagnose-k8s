package composer

import (
	"fmt"

	"github.com/lazy-diagnose-k8s/internal/domain"
)

// Compose generates suggested kubectl commands based on diagnosis result.
func Compose(intent domain.Intent, result *domain.DiagnosisResult) []string {
	if result.Target == nil {
		return nil
	}

	t := result.Target
	ns := t.Namespace
	resource := t.Kind + "/" + t.ResourceName

	var cmds []string

	switch intent {
	case domain.IntentCrashLoop:
		cmds = composeCrashLoop(ns, resource, t, result)
	case domain.IntentPending:
		cmds = composePending(ns, resource, t)
	case domain.IntentRolloutRegression:
		cmds = composeRollout(ns, resource, t)
	default:
		cmds = composeGeneric(ns, resource)
	}

	return cmds
}

func composeCrashLoop(ns, resource string, t *domain.Target, result *domain.DiagnosisResult) []string {
	var cmds []string

	// Always useful: check logs
	cmds = append(cmds,
		fmt.Sprintf("kubectl logs %s -n %s --tail=100", resource, ns),
		fmt.Sprintf("kubectl logs %s -n %s --previous --tail=100", resource, ns),
	)

	// Based on hypothesis
	if result.PrimaryHypothesis != nil {
		switch result.PrimaryHypothesis.ID {
		case "oom_resource":
			cmds = append(cmds,
				fmt.Sprintf("kubectl get %s -n %s -o jsonpath='{.spec.containers[*].resources}'", resource, ns),
				fmt.Sprintf("kubectl top pods -n %s --containers | grep %s", ns, t.ResourceName),
				fmt.Sprintf("kubectl describe %s -n %s | grep -A5 'Limits\\|Requests'", resource, ns),
			)
		case "config_env_missing":
			cmds = append(cmds,
				fmt.Sprintf("kubectl describe %s -n %s | grep -A20 'Environment:'", resource, ns),
				fmt.Sprintf("kubectl get configmap -n %s", ns),
			)
		case "probe_issue":
			cmds = append(cmds,
				fmt.Sprintf("kubectl describe %s -n %s | grep -A10 'Liveness\\|Readiness'", resource, ns),
			)
		case "bad_image":
			cmds = append(cmds,
				fmt.Sprintf("kubectl describe %s -n %s | grep 'Image:'", resource, ns),
			)
		}
	}

	// Restart as last resort
	if t.Kind == "deployment" || t.Kind == "statefulset" {
		cmds = append(cmds,
			fmt.Sprintf("kubectl rollout restart %s -n %s", resource, ns),
		)
	}

	return cmds
}

func composePending(ns, resource string, t *domain.Target) []string {
	cmds := []string{
		fmt.Sprintf("kubectl describe %s -n %s", resource, ns),
		fmt.Sprintf("kubectl get events -n %s --field-selector involvedObject.name=%s --sort-by=.lastTimestamp", ns, t.ResourceName),
		fmt.Sprintf("kubectl get nodes -o wide"),
		fmt.Sprintf("kubectl describe nodes | grep -A5 'Allocated resources'"),
	}
	return cmds
}

func composeRollout(ns, resource string, t *domain.Target) []string {
	rolloutTarget := resource
	if t.RolloutTarget != "" {
		rolloutTarget = t.RolloutTarget
	}

	cmds := []string{
		fmt.Sprintf("kubectl rollout status %s -n %s", rolloutTarget, ns),
		fmt.Sprintf("kubectl rollout history %s -n %s", rolloutTarget, ns),
		fmt.Sprintf("kubectl logs %s -n %s --tail=100", resource, ns),
	}

	// Rollback command
	if t.Kind == "deployment" {
		cmds = append(cmds,
			fmt.Sprintf("kubectl rollout undo %s -n %s", rolloutTarget, ns),
		)
	}

	return cmds
}

func composeGeneric(ns, resource string) []string {
	return []string{
		fmt.Sprintf("kubectl describe %s -n %s", resource, ns),
		fmt.Sprintf("kubectl logs %s -n %s --tail=100", resource, ns),
		fmt.Sprintf("kubectl get events -n %s --sort-by=.lastTimestamp", ns),
	}
}
