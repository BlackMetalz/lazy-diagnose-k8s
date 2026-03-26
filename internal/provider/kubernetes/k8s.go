package kubernetes

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/lazy-diagnose-k8s/internal/domain"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Provider collects K8s facts using client-go.
type Provider struct {
	client kubernetes.Interface
}

// NewInCluster creates a provider using in-cluster config (when running as a pod).
func NewInCluster() (*Provider, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create k8s client: %w", err)
	}
	return &Provider{client: client}, nil
}

// NewFromKubeconfig creates a provider using a kubeconfig file.
func NewFromKubeconfig(kubeconfigPath string) (*Provider, error) {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("build kubeconfig: %w", err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create k8s client: %w", err)
	}
	return &Provider{client: client}, nil
}

// CollectFacts gathers K8s data for the given target.
func (p *Provider) CollectFacts(ctx context.Context, target *domain.Target) (*domain.K8sFacts, error) {
	facts := &domain.K8sFacts{}

	// Get pods by label selector or direct name
	pods, err := p.getPods(ctx, target)
	if err != nil {
		return nil, fmt.Errorf("get pods: %w", err)
	}
	facts.PodStatuses = pods

	// Get events for the target namespace
	events, err := p.getEvents(ctx, target)
	if err != nil {
		// Non-fatal: continue without events
		events = nil
	}
	facts.Events = events

	// Get rollout status for deployments
	if target.Kind == "deployment" {
		rollout, err := p.getRolloutStatus(ctx, target)
		if err == nil {
			facts.RolloutStatus = rollout
		}
	}

	// Get resource requests from the first pod
	if len(pods) > 0 {
		reqs := p.getResourceRequests(ctx, target)
		if reqs != nil {
			facts.ResourceRequests = reqs
		}
	}

	// Get node info for pending pods
	for _, pod := range pods {
		if pod.Phase == "Pending" {
			nodeInfo := p.getNodeInfo(ctx, target)
			if nodeInfo != nil {
				facts.NodeInfo = nodeInfo
			}
			break
		}
	}

	return facts, nil
}

func (p *Provider) getPods(ctx context.Context, target *domain.Target) ([]domain.PodStatus, error) {
	var podStatuses []domain.PodStatus

	// If target is a pod, get it directly
	if target.Kind == "pod" {
		pod, err := p.client.CoreV1().Pods(target.Namespace).Get(ctx, target.ResourceName, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		podStatuses = append(podStatuses, convertPodStatus(pod))
		return podStatuses, nil
	}

	// Otherwise, list pods by label selector
	var labelSelector string
	if len(target.Selectors) > 0 {
		var parts []string
		for k, v := range target.Selectors {
			parts = append(parts, fmt.Sprintf("%s=%s", k, v))
		}
		labelSelector = strings.Join(parts, ",")
	} else {
		// Fallback: try common label patterns
		labelSelector = fmt.Sprintf("app=%s", target.ResourceName)
	}

	pods, err := p.client.CoreV1().Pods(target.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return nil, err
	}

	for i := range pods.Items {
		podStatuses = append(podStatuses, convertPodStatus(&pods.Items[i]))
	}

	return podStatuses, nil
}

func convertPodStatus(pod *corev1.Pod) domain.PodStatus {
	status := domain.PodStatus{
		Name:  pod.Name,
		Phase: string(pod.Status.Phase),
		Ready: isPodReady(pod),
	}

	for _, cs := range pod.Status.ContainerStatuses {
		container := domain.ContainerStatus{
			Name:         cs.Name,
			Ready:        cs.Ready,
			RestartCount: int(cs.RestartCount),
		}
		status.RestartCount += int(cs.RestartCount)

		if cs.State.Running != nil {
			container.State = "running"
		} else if cs.State.Waiting != nil {
			container.State = "waiting"
			container.Reason = cs.State.Waiting.Reason
		} else if cs.State.Terminated != nil {
			container.State = "terminated"
			container.Reason = cs.State.Terminated.Reason
			container.ExitCode = int(cs.State.Terminated.ExitCode)
		}

		if cs.LastTerminationState.Terminated != nil {
			container.LastTermination = cs.LastTerminationState.Terminated.Reason
		}

		status.ContainerStatuses = append(status.ContainerStatuses, container)
	}

	return status
}

func isPodReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

func (p *Provider) getEvents(ctx context.Context, target *domain.Target) ([]domain.K8sEvent, error) {
	// Get events from the namespace, filter by involved object
	events, err := p.client.CoreV1().Events(target.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	var result []domain.K8sEvent
	for _, ev := range events.Items {
		// Filter: match by resource name or pod prefix
		name := ev.InvolvedObject.Name
		if !strings.HasPrefix(name, target.ResourceName) && name != target.ResourceName {
			continue
		}

		result = append(result, domain.K8sEvent{
			Type:      ev.Type,
			Reason:    ev.Reason,
			Message:   ev.Message,
			Count:     int(ev.Count),
			FirstSeen: ev.FirstTimestamp.Time,
			LastSeen:  ev.LastTimestamp.Time,
		})
	}

	// Sort by last seen descending
	sort.Slice(result, func(i, j int) bool {
		return result[i].LastSeen.After(result[j].LastSeen)
	})

	// Limit to 20 most recent
	if len(result) > 20 {
		result = result[:20]
	}

	return result, nil
}

func (p *Provider) getRolloutStatus(ctx context.Context, target *domain.Target) (*domain.RolloutStatus, error) {
	deploy, err := p.client.AppsV1().Deployments(target.Namespace).Get(ctx, target.ResourceName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	return &domain.RolloutStatus{
		CurrentRevision:   deploy.Annotations["deployment.kubernetes.io/revision"],
		DesiredReplicas:   int(*deploy.Spec.Replicas),
		ReadyReplicas:     int(deploy.Status.ReadyReplicas),
		UpdatedReplicas:   int(deploy.Status.UpdatedReplicas),
		AvailableReplicas: int(deploy.Status.AvailableReplicas),
	}, nil
}

func (p *Provider) getResourceRequests(ctx context.Context, target *domain.Target) *domain.ResourceRequests {
	var labelSelector string
	if len(target.Selectors) > 0 {
		var parts []string
		for k, v := range target.Selectors {
			parts = append(parts, fmt.Sprintf("%s=%s", k, v))
		}
		labelSelector = strings.Join(parts, ",")
	} else {
		labelSelector = fmt.Sprintf("app=%s", target.ResourceName)
	}

	pods, err := p.client.CoreV1().Pods(target.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
		Limit:         1,
	})
	if err != nil || len(pods.Items) == 0 {
		return nil
	}

	pod := pods.Items[0]
	if len(pod.Spec.Containers) == 0 {
		return nil
	}

	c := pod.Spec.Containers[0]
	reqs := &domain.ResourceRequests{}

	if v, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
		reqs.CPURequest = v.String()
	}
	if v, ok := c.Resources.Limits[corev1.ResourceCPU]; ok {
		reqs.CPULimit = v.String()
	}
	if v, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
		reqs.MemoryRequest = v.String()
	}
	if v, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
		reqs.MemoryLimit = v.String()
	}

	return reqs
}

func (p *Provider) getNodeInfo(ctx context.Context, target *domain.Target) *domain.NodeInfo {
	var labelSelector string
	if len(target.Selectors) > 0 {
		var parts []string
		for k, v := range target.Selectors {
			parts = append(parts, fmt.Sprintf("%s=%s", k, v))
		}
		labelSelector = strings.Join(parts, ",")
	} else {
		labelSelector = fmt.Sprintf("app=%s", target.ResourceName)
	}

	pods, err := p.client.CoreV1().Pods(target.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
		Limit:         1,
	})
	if err != nil || len(pods.Items) == 0 {
		return nil
	}

	pod := pods.Items[0]
	info := &domain.NodeInfo{
		NodeSelector: pod.Spec.NodeSelector,
	}

	for _, t := range pod.Spec.Tolerations {
		info.Tolerations = append(info.Tolerations, fmt.Sprintf("%s=%s:%s", t.Key, t.Value, t.Effect))
	}

	if pod.Spec.Affinity != nil {
		info.Affinity = fmt.Sprintf("%+v", pod.Spec.Affinity)
	}

	return info
}
