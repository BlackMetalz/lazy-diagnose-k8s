package kubernetes

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"regexp"
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

// NewFromKubeconfig creates a provider using a kubeconfig file (current context).
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

// NewFromContext creates a provider using a specific kubeconfig context.
func NewFromContext(kubeconfigPath, contextName string) (*Provider, error) {
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfigPath},
		&clientcmd.ConfigOverrides{CurrentContext: contextName},
	).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("build kubeconfig context %s: %w", contextName, err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create k8s client for context %s: %w", contextName, err)
	}
	return &Provider{client: client}, nil
}

// CollectFacts gathers comprehensive K8s data for the given target.
func (p *Provider) CollectFacts(ctx context.Context, target *domain.Target) (*domain.K8sFacts, error) {
	facts := &domain.K8sFacts{}

	// 1. Get pods (status, containers, init containers, conditions, env errors, image)
	pods, err := p.getPods(ctx, target)
	if err != nil {
		return nil, fmt.Errorf("get pods (ns=%s, kind=%s, name=%s, selectors=%v): %w",
			target.Namespace, target.Kind, target.ResourceName, target.Selectors, err)
	}
	facts.PodStatuses = pods

	// 2. Fetch container logs for unhealthy pods (current + previous)
	for i := range facts.PodStatuses {
		for j := range facts.PodStatuses[i].ContainerStatuses {
			cs := &facts.PodStatuses[i].ContainerStatuses[j]
			podName := facts.PodStatuses[i].Name
			if cs.RestartCount > 0 || cs.State == "waiting" || cs.State == "terminated" {
				cs.CurrentLogs = p.fetchLogs(ctx, target.Namespace, podName, cs.Name, false, 50)
				if cs.RestartCount > 0 {
					cs.PreviousLogs = p.fetchLogs(ctx, target.Namespace, podName, cs.Name, true, 50)
				}
			}
		}
		// Also fetch init container logs if they failed
		for j := range facts.PodStatuses[i].InitContainerStatuses {
			ics := &facts.PodStatuses[i].InitContainerStatuses[j]
			if ics.State == "waiting" || ics.State == "terminated" {
				ics.CurrentLogs = p.fetchLogs(ctx, target.Namespace, facts.PodStatuses[i].Name, ics.Name, false, 30)
			}
		}
	}

	// 3. Events timeline
	events, err := p.getEvents(ctx, target)
	if err == nil {
		facts.Events = events
	}

	// 4. Rollout status (deployments)
	if target.Kind == "deployment" {
		if rollout, err := p.getRolloutStatus(ctx, target); err == nil {
			facts.RolloutStatus = rollout
		}
	}

	// 5. Resource requests/limits
	if len(pods) > 0 {
		facts.ResourceRequests = p.getResourceRequests(ctx, target)
	}

	// 6. Owner chain (Pod → ReplicaSet → Deployment)
	facts.OwnerChain = p.getOwnerChain(ctx, target)

	// 7. Node info + resources for pending pods, and PVC names
	for _, pod := range pods {
		if pod.Phase == "Pending" {
			facts.NodeInfo = p.getNodeInfo(ctx, target)
			facts.NodeResources = p.getNodeResources(ctx)
			facts.PVCNames = p.getPVCNames(ctx, target)
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
		return nil, fmt.Errorf("list pods (ns=%s, selector=%s): %w", target.Namespace, labelSelector, err)
	}

	for i := range pods.Items {
		podStatuses = append(podStatuses, convertPodStatus(&pods.Items[i]))
	}

	// Log empty results for debugging
	if len(podStatuses) == 0 {
		// Try without label selector as fallback — maybe labels don't match
		allPods, err2 := p.client.CoreV1().Pods(target.Namespace).List(ctx, metav1.ListOptions{})
		if err2 == nil {
			// Search by name prefix
			for i := range allPods.Items {
				if strings.HasPrefix(allPods.Items[i].Name, target.ResourceName) {
					podStatuses = append(podStatuses, convertPodStatus(&allPods.Items[i]))
				}
			}
		}
	}

	return podStatuses, nil
}

func convertPodStatus(pod *corev1.Pod) domain.PodStatus {
	status := domain.PodStatus{
		Name:  pod.Name,
		Phase: string(pod.Status.Phase),
		Ready: isPodReady(pod),
	}

	// Pod conditions (Ready, Initialized, PodScheduled, ContainersReady)
	for _, cond := range pod.Status.Conditions {
		status.Conditions = append(status.Conditions, domain.ResourceCondition{
			Type:    string(cond.Type),
			Status:  string(cond.Status),
			Reason:  cond.Reason,
			Message: cond.Message,
		})
	}

	// Main containers
	for i, cs := range pod.Status.ContainerStatuses {
		container := convertContainerStatus(cs)
		// Get image from spec
		if i < len(pod.Spec.Containers) {
			container.Image = pod.Spec.Containers[i].Image
			// Check env refs for missing ConfigMaps/Secrets
			container.EnvErrors = checkEnvRefs(pod.Spec.Containers[i])
		}
		status.RestartCount += container.RestartCount
		status.ContainerStatuses = append(status.ContainerStatuses, container)
	}

	// Init containers
	for i, cs := range pod.Status.InitContainerStatuses {
		container := convertContainerStatus(cs)
		if i < len(pod.Spec.InitContainers) {
			container.Image = pod.Spec.InitContainers[i].Image
		}
		status.InitContainerStatuses = append(status.InitContainerStatuses, container)
	}

	return status
}

func convertContainerStatus(cs corev1.ContainerStatus) domain.ContainerStatus {
	container := domain.ContainerStatus{
		Name:         cs.Name,
		Ready:        cs.Ready,
		RestartCount: int(cs.RestartCount),
	}

	if cs.State.Running != nil {
		container.State = "running"
	} else if cs.State.Waiting != nil {
		container.State = "waiting"
		container.Reason = cs.State.Waiting.Reason
		container.Message = cs.State.Waiting.Message
	} else if cs.State.Terminated != nil {
		container.State = "terminated"
		container.Reason = cs.State.Terminated.Reason
		container.Message = cs.State.Terminated.Message
		container.ExitCode = int(cs.State.Terminated.ExitCode)
	}

	if cs.LastTerminationState.Terminated != nil {
		container.LastTermination = cs.LastTerminationState.Terminated.Reason
		container.LastExitCode = int(cs.LastTerminationState.Terminated.ExitCode)
	}

	// Image from status (actual pulled image)
	if container.Image == "" {
		container.Image = cs.Image
	}

	return container
}

// checkEnvRefs checks if container env references ConfigMaps/Secrets that might be missing.
func checkEnvRefs(container corev1.Container) []string {
	var errors []string
	for _, env := range container.Env {
		if env.ValueFrom != nil {
			if ref := env.ValueFrom.ConfigMapKeyRef; ref != nil {
				optional := ref.Optional != nil && *ref.Optional
				if !optional {
					errors = append(errors, fmt.Sprintf("env %s refs ConfigMap/%s key %s",
						env.Name, ref.Name, ref.Key))
				}
			}
			if ref := env.ValueFrom.SecretKeyRef; ref != nil {
				optional := ref.Optional != nil && *ref.Optional
				if !optional {
					errors = append(errors, fmt.Sprintf("env %s refs Secret/%s key %s",
						env.Name, ref.Name, ref.Key))
				}
			}
		}
	}
	for _, envFrom := range container.EnvFrom {
		if envFrom.ConfigMapRef != nil {
			errors = append(errors, fmt.Sprintf("envFrom ConfigMap/%s", envFrom.ConfigMapRef.Name))
		}
		if envFrom.SecretRef != nil {
			errors = append(errors, fmt.Sprintf("envFrom Secret/%s", envFrom.SecretRef.Name))
		}
	}
	return errors
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
		CurrentRevision:     deploy.Annotations["deployment.kubernetes.io/revision"],
		DesiredReplicas:     int(*deploy.Spec.Replicas),
		ReadyReplicas:       int(deploy.Status.ReadyReplicas),
		UpdatedReplicas:     int(deploy.Status.UpdatedReplicas),
		AvailableReplicas:   int(deploy.Status.AvailableReplicas),
		UnavailableReplicas: int(deploy.Status.UnavailableReplicas),
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

// getOwnerChain traces the owner hierarchy: Pod → ReplicaSet → Deployment.
func (p *Provider) getOwnerChain(ctx context.Context, target *domain.Target) []domain.OwnerRef {
	var chain []domain.OwnerRef

	// Start from pods
	labelSelector := fmt.Sprintf("app=%s", target.ResourceName)
	if len(target.Selectors) > 0 {
		var parts []string
		for k, v := range target.Selectors {
			parts = append(parts, fmt.Sprintf("%s=%s", k, v))
		}
		labelSelector = strings.Join(parts, ",")
	}

	pods, err := p.client.CoreV1().Pods(target.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
		Limit:         1,
	})
	if err != nil || len(pods.Items) == 0 {
		return nil
	}

	pod := pods.Items[0]
	chain = append(chain, domain.OwnerRef{Kind: "Pod", Name: pod.Name})

	for _, ref := range pod.OwnerReferences {
		chain = append(chain, domain.OwnerRef{Kind: ref.Kind, Name: ref.Name})

		// If owner is ReplicaSet, find its Deployment owner
		if ref.Kind == "ReplicaSet" {
			rs, err := p.client.AppsV1().ReplicaSets(target.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
			if err == nil {
				for _, rsRef := range rs.OwnerReferences {
					chain = append(chain, domain.OwnerRef{Kind: rsRef.Kind, Name: rsRef.Name})
				}
			}
		}
	}

	return chain
}

// getNodeResources returns cluster-level resource availability.
func (p *Provider) getNodeResources(ctx context.Context) *domain.NodeResources {
	nodes, err := p.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil || len(nodes.Items) == 0 {
		return nil
	}

	nr := &domain.NodeResources{
		NodeCount: len(nodes.Items),
	}

	// Sum allocatable resources across all nodes
	var totalCPUMillis, totalMemBytes, allocCPUMillis, allocMemBytes int64
	for _, node := range nodes.Items {
		if cpu, ok := node.Status.Capacity[corev1.ResourceCPU]; ok {
			totalCPUMillis += cpu.MilliValue()
		}
		if mem, ok := node.Status.Capacity[corev1.ResourceMemory]; ok {
			totalMemBytes += mem.Value()
		}
		if cpu, ok := node.Status.Allocatable[corev1.ResourceCPU]; ok {
			allocCPUMillis += cpu.MilliValue()
		}
		if mem, ok := node.Status.Allocatable[corev1.ResourceMemory]; ok {
			allocMemBytes += mem.Value()
		}
	}

	nr.TotalCPU = fmt.Sprintf("%dm", totalCPUMillis)
	nr.TotalMemory = fmt.Sprintf("%dMi", totalMemBytes/(1024*1024))
	nr.AllocatableCPU = fmt.Sprintf("%dm", allocCPUMillis)
	nr.AllocatableMemory = fmt.Sprintf("%dMi", allocMemBytes/(1024*1024))

	return nr
}

// fetchLogs fetches container logs directly from K8s API.
// Returns last `tailLines` lines. If `previous` is true, fetches previous container's logs.
func (p *Provider) fetchLogs(ctx context.Context, namespace, podName, containerName string, previous bool, tailLines int) []string {
	tailInt64 := int64(tailLines)
	opts := &corev1.PodLogOptions{
		Container: containerName,
		Previous:  previous,
		TailLines: &tailInt64,
	}

	req := p.client.CoreV1().Pods(namespace).GetLogs(podName, opts)
	stream, err := req.Stream(ctx)
	if err != nil {
		return nil
	}
	defer stream.Close()

	var lines []string
	scanner := bufio.NewScanner(io.LimitReader(stream, 256*1024)) // 256KB limit
	for scanner.Scan() {
		line := scanner.Text()
		// Strip containerd:// URIs from K8s internal messages — they add noise
		if strings.Contains(line, "unable to retrieve container logs") {
			line = stripContainerRuntimeURI(line)
		}
		lines = append(lines, line)
	}
	return lines
}

// getPVCNames extracts PersistentVolumeClaim names from pod volumes.
func (p *Provider) getPVCNames(ctx context.Context, target *domain.Target) []string {
	pods, err := p.client.CoreV1().Pods(target.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", target.ResourceName),
	})
	if err != nil || len(pods.Items) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var names []string
	for _, pod := range pods.Items {
		for _, vol := range pod.Spec.Volumes {
			if vol.PersistentVolumeClaim != nil {
				name := vol.PersistentVolumeClaim.ClaimName
				if !seen[name] {
					seen[name] = true
					names = append(names, name)
				}
			}
		}
	}
	return names
}

// reContainerRuntimeURI matches runtime URIs like containerd://abc123... or docker://abc123...
var reContainerRuntimeURI = regexp.MustCompile(`\s+for\s+\S+://\S+`)

// stripContainerRuntimeURI removes "for containerd://..." or "for docker://..." from log lines.
func stripContainerRuntimeURI(line string) string {
	return reContainerRuntimeURI.ReplaceAllString(line, "")
}
