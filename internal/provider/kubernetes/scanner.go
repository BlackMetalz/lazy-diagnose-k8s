package kubernetes

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// systemNamespaces are skipped during ScanAll.
var systemNamespaces = map[string]bool{
	"kube-system":     true,
	"kube-public":     true,
	"kube-node-lease": true,
	"local-path-storage": true,
	"monitoring":      true,
}

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

// ScanAllNamespaces scans all non-system namespaces for unhealthy pods.
func (p *Provider) ScanAllNamespaces(ctx context.Context) ([]UnhealthyPod, error) {
	nsList, err := p.client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}

	var all []UnhealthyPod
	for _, ns := range nsList.Items {
		if systemNamespaces[ns.Name] {
			continue
		}
		unhealthy, err := p.ScanNamespace(ctx, ns.Name)
		if err != nil {
			continue // skip namespaces we can't access
		}
		all = append(all, unhealthy...)
	}
	return all, nil
}

// FuzzyFindPod searches for pods matching a partial name across a namespace.
// Returns matching pods sorted by relevance (exact prefix > contains).
func (p *Provider) FuzzyFindPod(ctx context.Context, query string, namespace string) ([]PodMatch, error) {
	var allPods []corev1.Pod

	if namespace != "" {
		pods, err := p.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		allPods = pods.Items
	} else {
		// Search all non-system namespaces
		nsList, err := p.client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		for _, ns := range nsList.Items {
			if systemNamespaces[ns.Name] {
				continue
			}
			pods, err := p.client.CoreV1().Pods(ns.Name).List(ctx, metav1.ListOptions{})
			if err != nil {
				continue
			}
			allPods = append(allPods, pods.Items...)
		}
	}

	query = strings.ToLower(query)
	var matches []PodMatch

	for _, pod := range allPods {
		name := strings.ToLower(pod.Name)
		score := 0

		if name == query {
			score = 100 // exact
		} else if strings.HasPrefix(name, query) {
			score = 80 // prefix
		} else if strings.Contains(name, query) {
			score = 60 // contains
		}

		// Also match against deployment/owner name
		if score == 0 {
			for _, ref := range pod.OwnerReferences {
				ownerName := strings.ToLower(ref.Name)
				if strings.HasPrefix(ownerName, query) || strings.Contains(ownerName, query) {
					score = 50
					break
				}
			}
		}

		// Match against labels (app label)
		if score == 0 {
			if app, ok := pod.Labels["app"]; ok {
				if strings.Contains(strings.ToLower(app), query) {
					score = 40
				}
			}
		}

		if score > 0 {
			matches = append(matches, PodMatch{
				Name:      pod.Name,
				Namespace: pod.Namespace,
				Phase:     string(pod.Status.Phase),
				Score:     score,
			})
		}
	}

	// Sort by score desc
	for i := 0; i < len(matches); i++ {
		for j := i + 1; j < len(matches); j++ {
			if matches[j].Score > matches[i].Score {
				matches[i], matches[j] = matches[j], matches[i]
			}
		}
	}

	// Limit to 10
	if len(matches) > 10 {
		matches = matches[:10]
	}

	return matches, nil
}

// PodMatch represents a fuzzy search result.
type PodMatch struct {
	Name      string
	Namespace string
	Phase     string
	Score     int // 100=exact, 80=prefix, 60=contains, 50=owner, 40=label
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
