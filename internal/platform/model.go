package platform

import "time"

// NodeHealth is the health state reported by an edge agent.
type NodeHealth string

const (
	NodeReady    NodeHealth = "ready"
	NodeDegraded NodeHealth = "degraded"
	NodeDraining NodeHealth = "draining"
	NodeOffline  NodeHealth = "offline"
)

// TaskType controls the execution policy used by the scheduler.
type TaskType string

const (
	TaskTraining  TaskType = "training"
	TaskFineTune  TaskType = "fine_tune"
	TaskInference TaskType = "inference"
)

// TaskPhase is deliberately separate from an agent's execution state. The
// control plane owns this state and can reconcile it after an agent reconnects.
type TaskPhase string

const (
	TaskPending    TaskPhase = "pending"
	TaskAssigned   TaskPhase = "assigned"
	TaskRunning    TaskPhase = "running"
	TaskCancelling TaskPhase = "cancelling"
	TaskSucceeded  TaskPhase = "succeeded"
	TaskFailed     TaskPhase = "failed"
	TaskCancelled  TaskPhase = "cancelled"
)

// GPU describes one allocatable device. FreeMemoryMiB is the capacity that
// remains available to this platform, not necessarily the value reported by
// the driver when another workload is outside the platform.
type GPU struct {
	ID              string   `json:"id"`
	Index           int      `json:"index"`
	Vendor          string   `json:"vendor"`
	Model           string   `json:"model"`
	MemoryMiB       int64    `json:"memory_mib"`
	FreeMemoryMiB   int64    `json:"free_memory_mib"`
	UtilizationPct  *float64 `json:"utilization_pct,omitempty"`
	ComputeMajor    int      `json:"compute_major"`
	ComputeMinor    int      `json:"compute_minor"`
	TopologyGroup   string   `json:"topology_group"`
	AllocatedTaskID string   `json:"allocated_task_id,omitempty"`
}

func (g GPU) Available() bool {
	return g.AllocatedTaskID == "" && g.FreeMemoryMiB > 0
}

// ResourceNode is the scheduling view of one independent execution server.
// Legacy tasks use one node; distributed deployments reserve a group of nodes.
type ResourceNode struct {
	Agent             *AgentEndpoint    `json:"agent,omitempty"`
	DeploymentID      string            `json:"deployment_id,omitempty"`
	ID                string            `json:"id"`
	Name              string            `json:"name"`
	Datacenter        string            `json:"datacenter"`
	Region            string            `json:"region"`
	Runtime           string            `json:"runtime"`
	Labels            map[string]string `json:"labels,omitempty"`
	GPUs              []GPU             `json:"gpus"`
	Health            NodeHealth        `json:"health"`
	LastHeartbeat     time.Time         `json:"last_heartbeat"`
	QueueDepth        int               `json:"queue_depth"`
	RunningTasks      int               `json:"running_tasks"`
	CachedModels      map[string]bool   `json:"cached_models,omitempty"`
	CostPerGPUHour    float64           `json:"cost_per_gpu_hour"`
	DataReady         map[string]bool   `json:"data_ready,omitempty"`
	SchedulingEnabled bool              `json:"scheduling_enabled"`
}

// TaskSpec contains scheduling intent. The executor may add runtime details
// after assignment, but it must not mutate these constraints.
type TaskSpec struct {
	ID                   string            `json:"id"`
	TenantID             string            `json:"tenant_id"`
	Name                 string            `json:"name"`
	Type                 TaskType          `json:"type"`
	ModelID              string            `json:"model_id"`
	DataID               string            `json:"data_id,omitempty"`
	GPUCount             int               `json:"gpu_count"`
	MinGPUMemoryMiB      int64             `json:"min_gpu_memory_mib"`
	RequiredVendor       string            `json:"required_vendor,omitempty"`
	RequiredGPUModel     string            `json:"required_gpu_model,omitempty"`
	RequiredRuntime      string            `json:"required_runtime,omitempty"`
	RequiredLabels       map[string]string `json:"required_labels,omitempty"`
	AllowedDatacenters   []string          `json:"allowed_datacenters,omitempty"`
	DeniedDatacenters    []string          `json:"denied_datacenters,omitempty"`
	AllowedRegions       []string          `json:"allowed_regions,omitempty"`
	Priority             int               `json:"priority"`
	EstimatedDurationSec int64             `json:"estimated_duration_sec"`
	IdempotencyKey       string            `json:"idempotency_key,omitempty"`
}

// Task is a control-plane record. Assignment is explicit so reconciliation
// can distinguish a pending task from one already sent to an edge agent.
type Task struct {
	Spec            TaskSpec            `json:"spec"`
	Phase           TaskPhase           `json:"phase"`
	AssignedNodeID  string              `json:"assigned_node_id,omitempty"`
	AssignedGPUIDs  []string            `json:"assigned_gpu_ids,omitempty"`
	AssignmentToken string              `json:"assignment_token,omitempty"`
	Attempt         int                 `json:"attempt"`
	FailureReason   string              `json:"failure_reason,omitempty"`
	CreatedAt       time.Time           `json:"created_at"`
	UpdatedAt       time.Time           `json:"updated_at"`
	StartedAt       *time.Time          `json:"started_at,omitempty"`
	FinishedAt      *time.Time          `json:"finished_at,omitempty"`
	GPUSeconds      float64             `json:"gpu_seconds"`
	Decision        *SchedulingDecision `json:"decision,omitempty"`
}

func (t Task) Terminal() bool {
	return t.Phase == TaskSucceeded || t.Phase == TaskFailed || t.Phase == TaskCancelled
}

// ScoreBreakdown is persisted with a scheduling decision so an operator can
// understand why a node won without reproducing the scoring calculation.
type ScoreBreakdown struct {
	Queue      float64 `json:"queue"`
	Load       float64 `json:"load"`
	ModelCache float64 `json:"model_cache"`
	DataReady  float64 `json:"data_ready"`
	Cost       float64 `json:"cost"`
	Total      float64 `json:"total"`
}

type Candidate struct {
	NodeID         string         `json:"node_id"`
	GPUIDs         []string       `json:"gpu_ids"`
	Score          float64        `json:"score"`
	ScoreBreakdown ScoreBreakdown `json:"score_breakdown"`
}

type SchedulingDecision struct {
	TaskID           string              `json:"task_id"`
	Selected         *Candidate          `json:"selected,omitempty"`
	Candidates       []Candidate         `json:"candidates,omitempty"`
	RejectionReasons map[string][]string `json:"rejection_reasons,omitempty"`
}
