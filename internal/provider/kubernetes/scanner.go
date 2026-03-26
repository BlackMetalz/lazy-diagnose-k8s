package kubernetes

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// UnhealthyPod represents a pod with issues found during scan.
type UnhealthyPod struct {
	Name      string
	Namespace string
	Phase     string
	Reason    string // short description: "OOMKilled", "ImagePullBackOff", "Pending", etc.
	Restarts  int
	OwnerKind string // Deployment, StatefulSet, DaemonSet, etc.
	OwnerName string
}

// ScanNamespace finds all unhealthy pods in a namespace.
func (p *Provider) ScanNamespace(ctx context.Context, namespace string) ([]UnhealthyPod, error) {
	pods, err := p.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list pods in %s: %w", namespace, err)
	}

	var unhealthy []UnhealthyPod
	for i := range pods.Items {
		pod := &pods.Items[i]
		if u := checkUnhealthy(pod); u != nil {
			unhealthy = append(unhealthy, *u)
		}
	}

	return unhealthy, nil
}

func checkUnhealthy(pod *corev1.Pod) *UnhealthyPod {
	u := &UnhealthyPod{
		Name:      pod.Name,
		Namespace: pod.Namespace,
		Phase:     string(pod.Status.Phase),
	}

	// Get owner
	if len(pod.OwnerReferences) > 0 {
		u.OwnerKind = pod.OwnerReferences[0].Kind
		u.OwnerName = pod.OwnerReferences[0].Name
	}

	// Total restarts
	for _, cs := range pod.Status.ContainerStatuses {
		u.Restarts += int(cs.RestartCount)
	}

	// Check: Pending
	if pod.Status.Phase == corev1.PodPending {
		u.Reason = "Pending"
		// Try to get more specific reason from conditions
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse {
				u.Reason = "Unschedulable"
				break
			}
		}
		return u
	}

	// Check: Failed phase
	if pod.Status.Phase == corev1.PodFailed {
		u.Reason = "Failed"
		return u
	}

	// Check container statuses for issues
	for _, cs := range pod.Status.ContainerStatuses {
		// Waiting states
		if cs.State.Waiting != nil {
			switch cs.State.Waiting.Reason {
			case "CrashLoopBackOff":
				u.Reason = "CrashLoopBackOff"
				// Check last termination for more detail
				if cs.LastTerminationState.Terminated != nil {
					u.Reason = fmt.Sprintf("CrashLoop (%s)", cs.LastTerminationState.Terminated.Reason)
				}
				return u
			case "ImagePullBackOff", "ErrImagePull", "ErrImageNeverPull":
				u.Reason = cs.State.Waiting.Reason
				return u
			case "CreateContainerConfigError":
				u.Reason = "ConfigError"
				return u
			}
		}

		// Terminated states (not completed jobs)
		if cs.State.Terminated != nil && cs.State.Terminated.Reason != "Completed" {
			u.Reason = cs.State.Terminated.Reason
			return u
		}

		// High restart count (> 3) even if currently running
		if cs.RestartCount > 3 && cs.State.Running != nil {
			if cs.LastTerminationState.Terminated != nil {
				u.Reason = fmt.Sprintf("Restarting (%s, %dx)", cs.LastTerminationState.Terminated.Reason, cs.RestartCount)
			} else {
				u.Reason = fmt.Sprintf("Restarting (%dx)", cs.RestartCount)
			}
			return u
		}

		// Not ready for too long
		if !cs.Ready && cs.State.Running != nil && cs.RestartCount > 0 {
			u.Reason = "NotReady"
			return u
		}
	}

	// Check init containers
	for _, cs := range pod.Status.InitContainerStatuses {
		if cs.State.Waiting != nil {
			u.Reason = fmt.Sprintf("InitContainer: %s", cs.State.Waiting.Reason)
			return u
		}
		if cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0 {
			u.Reason = fmt.Sprintf("InitContainer: %s", cs.State.Terminated.Reason)
			return u
		}
	}

	return nil // healthy
}
