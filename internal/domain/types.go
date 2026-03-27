package domain

import "time"

// Intent represents the classified diagnosis intent from user message.
type Intent string

const (
	IntentCrashLoop          Intent = "crashloop"
	IntentPending            Intent = "pending"
	IntentRolloutRegression  Intent = "rollout_regression"
	IntentUnknown            Intent = "unknown"
)

// Target represents a resolved Kubernetes resource target.
type Target struct {
	Name             string `json:"name"`
	Namespace        string `json:"namespace"`
	Kind             string `json:"kind"` // deployment, statefulset, daemonset, pod
	ResourceName     string `json:"resource_name"`
	Selectors        map[string]string `json:"selectors,omitempty"`
	MetricsJob       string `json:"metrics_job,omitempty"`
	RolloutTarget    string `json:"rollout_target,omitempty"`
}

// FullName returns namespace/kind/name for display.
func (t Target) FullName() string {
	return t.Namespace + "/" + t.Kind + "/" + t.ResourceName
}

// DiagnosisRequest represents a single diagnosis request from user.
type DiagnosisRequest struct {
	ID        string    `json:"id"`
	ChatID    int64     `json:"chat_id"`
	RawText   string    `json:"raw_text"`
	Intent    Intent    `json:"intent"`
	Target    *Target   `json:"target,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// ProviderStatus tracks whether a data provider succeeded.
type ProviderStatus struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Error     string `json:"error,omitempty"`
	Duration  time.Duration `json:"duration"`
}

// EvidenceBundle holds all collected evidence from providers.
type EvidenceBundle struct {
	Target           *Target                `json:"target"`
	K8sFacts         *K8sFacts              `json:"k8s_facts,omitempty"`
	LogsFacts        *LogsFacts             `json:"logs_facts,omitempty"`
	MetricsFacts     *MetricsFacts          `json:"metrics_facts,omitempty"`
	ProviderStatuses []ProviderStatus       `json:"provider_statuses"`
	CollectedAt      time.Time              `json:"collected_at"`
}

// HasK8s returns true if K8s data was collected.
func (e *EvidenceBundle) HasK8s() bool { return e.K8sFacts != nil }

// HasLogs returns true if logs data was collected.
func (e *EvidenceBundle) HasLogs() bool { return e.LogsFacts != nil }

// HasMetrics returns true if metrics data was collected.
func (e *EvidenceBundle) HasMetrics() bool { return e.MetricsFacts != nil }

// K8sFacts holds normalized Kubernetes data.
type K8sFacts struct {
	PodStatuses        []PodStatus         `json:"pod_statuses"`
	Events             []K8sEvent          `json:"events"`
	Conditions         []ResourceCondition `json:"conditions"`
	RolloutStatus      *RolloutStatus      `json:"rollout_status,omitempty"`
	ResourceRequests   *ResourceRequests   `json:"resource_requests,omitempty"`
	NodeInfo           *NodeInfo           `json:"node_info,omitempty"`
	NodeResources      *NodeResources      `json:"node_resources,omitempty"`
	OwnerChain         []OwnerRef          `json:"owner_chain,omitempty"` // Pod → ReplicaSet → Deployment
	PVCNames           []string            `json:"pvc_names,omitempty"`   // PersistentVolumeClaim names used by pods
}

type PodStatus struct {
	Name                string             `json:"name"`
	Phase               string             `json:"phase"` // Running, Pending, Failed, Succeeded, Unknown
	Ready               bool               `json:"ready"`
	RestartCount        int                `json:"restart_count"`
	ContainerStatuses   []ContainerStatus  `json:"container_statuses"`
	InitContainerStatuses []ContainerStatus `json:"init_container_statuses,omitempty"`
	Conditions          []ResourceCondition `json:"conditions,omitempty"`
}

type ContainerStatus struct {
	Name            string   `json:"name"`
	Image           string   `json:"image,omitempty"` // full image reference
	Ready           bool     `json:"ready"`
	RestartCount    int      `json:"restart_count"`
	State           string   `json:"state"`     // running, waiting, terminated
	Reason          string   `json:"reason"`    // OOMKilled, CrashLoopBackOff, Error, etc.
	Message         string   `json:"message,omitempty"` // detailed state message
	ExitCode        int      `json:"exit_code"`
	LastTermination string   `json:"last_termination,omitempty"`
	LastExitCode    int      `json:"last_exit_code,omitempty"`
	// Environment variable issues (missing ConfigMap/Secret refs)
	EnvErrors       []string `json:"env_errors,omitempty"`
	// Logs fetched directly from K8s (not VictoriaLogs)
	CurrentLogs     []string `json:"current_logs,omitempty"`
	PreviousLogs    []string `json:"previous_logs,omitempty"`
}

// OwnerRef tracks the owner chain: Pod → ReplicaSet → Deployment
type OwnerRef struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// NodeResources holds cluster-level resource availability.
type NodeResources struct {
	TotalCPU       string `json:"total_cpu"`
	TotalMemory    string `json:"total_memory"`
	AllocatableCPU string `json:"allocatable_cpu"`
	AllocatableMemory string `json:"allocatable_memory"`
	NodeCount      int    `json:"node_count"`
}

type K8sEvent struct {
	Type      string    `json:"type"`    // Normal, Warning
	Reason    string    `json:"reason"`
	Message   string    `json:"message"`
	Count     int       `json:"count"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

type ResourceCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type RolloutStatus struct {
	CurrentRevision    string `json:"current_revision"`
	DesiredReplicas    int    `json:"desired_replicas"`
	ReadyReplicas      int    `json:"ready_replicas"`
	UpdatedReplicas    int    `json:"updated_replicas"`
	AvailableReplicas  int    `json:"available_replicas"`
	UnavailableReplicas int   `json:"unavailable_replicas"`
}

type ResourceRequests struct {
	CPURequest    string `json:"cpu_request"`
	CPULimit      string `json:"cpu_limit"`
	MemoryRequest string `json:"memory_request"`
	MemoryLimit   string `json:"memory_limit"`
}

type NodeInfo struct {
	Taints       []string `json:"taints,omitempty"`
	Tolerations  []string `json:"tolerations,omitempty"`
	NodeSelector map[string]string `json:"node_selector,omitempty"`
	Affinity     string `json:"affinity,omitempty"`
}

// LogsFacts holds normalized log data.
type LogsFacts struct {
	TotalLines    int          `json:"total_lines"`
	ErrorCount    int          `json:"error_count"`
	TopErrors     []LogPattern `json:"top_errors"`
	RecentLines   []string     `json:"recent_lines"`
	PreviousLines []string     `json:"previous_lines,omitempty"` // from previous container
	TimeRange     TimeRange    `json:"time_range"`
}

type LogPattern struct {
	Pattern string `json:"pattern"`
	Count   int    `json:"count"`
	Sample  string `json:"sample"`
}

type TimeRange struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// MetricsFacts holds normalized metrics data.
type MetricsFacts struct {
	RestartRate      *float64     `json:"restart_rate,omitempty"`
	CPUUsage         *float64     `json:"cpu_usage,omitempty"`
	CPULimit         *float64     `json:"cpu_limit,omitempty"`
	MemoryUsage      *float64     `json:"memory_usage,omitempty"` // bytes
	MemoryLimit      *float64     `json:"memory_limit,omitempty"` // bytes
	ErrorRate        *float64     `json:"error_rate,omitempty"`   // 5xx per second
	ErrorRateBefore  *float64     `json:"error_rate_before,omitempty"`
	Latency          *float64     `json:"latency,omitempty"`      // p99 ms
	LatencyBefore    *float64     `json:"latency_before,omitempty"`
	TimeRange        TimeRange    `json:"time_range"`
}

// Confidence represents how confident the diagnosis is.
type Confidence string

const (
	ConfidenceHigh   Confidence = "High"
	ConfidenceMedium Confidence = "Medium"
	ConfidenceLow    Confidence = "Low"
)

// HypothesisScore represents a scored hypothesis.
type HypothesisScore struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Score       int      `json:"score"`
	MaxScore    int      `json:"max_score"`
	Signals     []string `json:"signals"` // what contributed to the score
}

// DiagnosisResult is the final output of a diagnosis run.
type DiagnosisResult struct {
	RequestID            string            `json:"request_id"`
	Target               *Target           `json:"target"`
	Summary              string            `json:"summary"`
	Confidence           Confidence        `json:"confidence"`
	PrimaryHypothesis    *HypothesisScore  `json:"primary_hypothesis"`
	AlternativeHypotheses []HypothesisScore `json:"alternative_hypotheses,omitempty"`
	SupportingEvidence   []string          `json:"supporting_evidence"`
	RecommendedSteps     []string          `json:"recommended_steps"`
	SuggestedCommands    []string          `json:"suggested_commands"`
	Notes                []string          `json:"notes,omitempty"`
	Duration             time.Duration     `json:"duration"`
}
