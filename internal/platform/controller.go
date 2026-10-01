package platform

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrNodeNotFound = errors.New("resource node not found")
	ErrNodeExists   = errors.New("resource node already exists")
	ErrNodeBusy     = errors.New("resource node has active tasks or GPU reservations")
)

// Controller coordinates task admission, scheduling and the explicit GPU
// reservation boundary. The HTTP layer is intentionally thin and can later be
// replaced by gRPC or a message consumer without changing these rules.
type Controller struct {
	Nodes          *InMemoryStore
	Tasks          *InMemoryStore
	Scheduler      *Scheduler
	now            func() time.Time
	mu             sync.Mutex
	keys           map[string]string
	sequence       uint64
	deployments    map[string]Deployment
	deploymentKeys map[string]string
	artifacts      map[string]ModelArtifact
}

func NewController(store *InMemoryStore) *Controller {
	if store == nil {
		store = NewInMemoryStore()
	}
	return &Controller{
		Nodes: store, Tasks: store,
		Scheduler: NewScheduler(store, DefaultSchedulerWeights()),
		now:       time.Now, keys: make(map[string]string),
		deployments: make(map[string]Deployment), deploymentKeys: make(map[string]string),
		artifacts: make(map[string]ModelArtifact),
	}
}

// RegisterNode adds a node configured by an operator.  Agent heartbeats may
// subsequently fill in and refresh the observed GPU inventory.
func (c *Controller) RegisterNode(node ResourceNode) (ResourceNode, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if node.Name == "" {
		node.Name = node.ID
	}
	if err := validateNode(node); err != nil {
		return ResourceNode{}, err
	}
	if _, exists := c.Nodes.GetNode(node.ID); exists {
		return ResourceNode{}, fmt.Errorf("%w: %q", ErrNodeExists, node.ID)
	}
	if node.Health == "" {
		node.Health = NodeDegraded
	}
	if node.LastHeartbeat.IsZero() {
		// Configuration is not proof of liveness. A newly provisioned node must
		// receive a healthy agent heartbeat and an explicit enable operation
		// before it can receive work.
		node.Health = NodeDegraded
		node.SchedulingEnabled = false
	}
	if err := c.Nodes.UpsertNode(node); err != nil {
		return ResourceNode{}, err
	}
	stored, _ := c.Nodes.GetNode(node.ID)
	return stored, nil
}

// UpdateNode changes operator-managed node configuration.  The observed GPU
// inventory and control-plane counters are retained; only an idle node can be
// edited so a running workload cannot lose its placement metadata.
func (c *Controller) UpdateNode(id string, patch ResourceNode) (ResourceNode, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	current, ok := c.Nodes.GetNode(id)
	if !ok {
		return ResourceNode{}, fmt.Errorf("%w: %q", ErrNodeNotFound, id)
	}
	if c.nodeBusyLocked(id, current) {
		return ResourceNode{}, fmt.Errorf("%w: %q", ErrNodeBusy, id)
	}
	if patch.ID != "" && patch.ID != id {
		return ResourceNode{}, fmt.Errorf("node id in path and body do not match")
	}
	patch.ID = id
	if patch.Name == "" {
		patch.Name = current.Name
	}
	if patch.Datacenter == "" {
		patch.Datacenter = current.Datacenter
	}
	if patch.Region == "" {
		patch.Region = current.Region
	}
	if patch.Runtime == "" {
		patch.Runtime = current.Runtime
	}
	if patch.Health == "" {
		patch.Health = current.Health
	}
	// The edge agent owns observations.  Never let a configuration update
	// replace GPUs, reservations, or heartbeat freshness.
	patch.GPUs = current.GPUs
	patch.Host = current.Host
	patch.Agent = current.Agent
	patch.DeploymentID = current.DeploymentID
	patch.LastHeartbeat = current.LastHeartbeat
	patch.QueueDepth = current.QueueDepth
	patch.RunningTasks = current.RunningTasks
	if patch.CachedModels == nil {
		patch.CachedModels = current.CachedModels
	}
	if patch.DataReady == nil {
		patch.DataReady = current.DataReady
	}
	if patch.CostPerGPUHour == 0 {
		patch.CostPerGPUHour = current.CostPerGPUHour
	}
	if err := c.Nodes.UpsertNode(patch); err != nil {
		return ResourceNode{}, err
	}
	stored, _ := c.Nodes.GetNode(id)
	return stored, nil
}

// DrainNode stops new scheduling while preserving all active reservations and
// task state.  Existing tasks can finish and their GPUs are released normally.
func (c *Controller) DrainNode(id string) (ResourceNode, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	node, ok := c.Nodes.GetNode(id)
	if !ok {
		return ResourceNode{}, fmt.Errorf("%w: %q", ErrNodeNotFound, id)
	}
	node.SchedulingEnabled = false
	node.Health = NodeDraining
	if err := c.Nodes.UpsertNode(node); err != nil {
		return ResourceNode{}, err
	}
	stored, _ := c.Nodes.GetNode(id)
	return stored, nil
}

// EnableNode re-opens scheduling after a drain.  A node that is explicitly
// reported degraded/offline remains unschedulable until a healthy heartbeat.
func (c *Controller) EnableNode(id string) (ResourceNode, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	node, ok := c.Nodes.GetNode(id)
	if !ok {
		return ResourceNode{}, fmt.Errorf("%w: %q", ErrNodeNotFound, id)
	}
	node.SchedulingEnabled = true
	if node.Health == NodeDraining {
		node.Health = NodeReady
	}
	if err := c.Nodes.UpsertNode(node); err != nil {
		return ResourceNode{}, err
	}
	stored, _ := c.Nodes.GetNode(id)
	return stored, nil
}

// DeleteNode removes an idle node from the catalog.  Active tasks and GPU
// reservations must be drained/released first to prevent orphaned work.
func (c *Controller) DeleteNode(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	node, ok := c.Nodes.GetNode(id)
	if !ok {
		return fmt.Errorf("%w: %q", ErrNodeNotFound, id)
	}
	if c.nodeBusyLocked(id, node) {
		return fmt.Errorf("%w: %q", ErrNodeBusy, id)
	}
	if err := c.Nodes.DeleteNode(id); err != nil {
		return err
	}
	return nil
}

// ApplyHeartbeat merges an edge observation and then retries pending work.
// The stored node is returned so the caller sees the effective drain/enable
// state rather than the possibly stale state sent by the agent.
func (c *Controller) ApplyHeartbeat(node ResourceNode) (ResourceNode, []Task, error) {
	c.mu.Lock()
	if node.Health == "" {
		node.Health = NodeReady
	}
	if node.LastHeartbeat.IsZero() {
		node.LastHeartbeat = c.now().UTC()
	}
	if err := c.Nodes.UpsertHeartbeatNode(node); err != nil {
		c.mu.Unlock()
		return ResourceNode{}, nil, err
	}
	stored, _ := c.Nodes.GetNode(node.ID)
	c.mu.Unlock()
	assigned := c.ReconcilePending()
	return stored, assigned, nil
}

func (c *Controller) nodeBusyLocked(id string, node ResourceNode) bool {
	if node.DeploymentID != "" {
		return true
	}
	for _, gpu := range node.GPUs {
		if gpu.AllocatedTaskID != "" {
			return true
		}
	}
	for _, task := range c.Tasks.ListTasks() {
		if task.AssignedNodeID == id && !task.Terminal() {
			return true
		}
	}
	return false
}

func validateNode(node ResourceNode) error {
	if h := node.Host; h != nil {
		if h.MemoryTotalMiB <= 0 || h.MemoryAvailableMiB < 0 || h.MemoryAvailableMiB > h.MemoryTotalMiB || h.CPUCount < 1 {
			return fmt.Errorf("invalid host resource observation")
		}
		if h.CPUUtilizationPct != nil && !(*h.CPUUtilizationPct >= 0 && *h.CPUUtilizationPct <= 100) {
			return fmt.Errorf("invalid CPU utilization")
		}
	}
	if node.ID == "" {
		return fmt.Errorf("node id is required")
	}
	if node.Datacenter == "" {
		return fmt.Errorf("datacenter is required")
	}
	if node.Runtime == "" {
		return fmt.Errorf("runtime is required")
	}
	return nil
}

func (c *Controller) nextID() string {
	return fmt.Sprintf("task-%d-%d", c.now().UnixNano(), atomic.AddUint64(&c.sequence, 1))
}

// Submit is idempotent for the tenant/idempotency-key pair. A task with no
// currently suitable node remains pending and can be reconciled later.
func (c *Controller) Submit(spec TaskSpec) (Task, SchedulingDecision, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := validateTaskSpec(spec); err != nil {
		return Task{}, SchedulingDecision{}, false, err
	}
	if spec.ID == "" {
		spec.ID = c.nextID()
	}
	key := spec.TenantID + ":" + spec.IdempotencyKey
	if spec.IdempotencyKey != "" {
		if existingID := c.keys[key]; existingID != "" {
			if existing, ok := c.Tasks.GetTask(existingID); ok {
				return existing, SchedulingDecision{TaskID: existing.Spec.ID}, true, nil
			}
		}
	}
	now := c.now().UTC()
	task := Task{Spec: spec, Phase: TaskPending, Attempt: 1, CreatedAt: now, UpdatedAt: now}
	if err := c.Tasks.CreateTask(task); err != nil {
		return Task{}, SchedulingDecision{}, false, err
	}
	if spec.IdempotencyKey != "" {
		c.keys[key] = spec.ID
	}
	decision, scheduleErr := c.Scheduler.Schedule(spec)
	if scheduleErr == nil && decision.Selected != nil {
		candidate := decision.Selected
		if err := c.Nodes.ReserveGPUs(candidate.NodeID, spec.ID, candidate.GPUIDs); err != nil {
			task.FailureReason = "reservation failed: " + err.Error()
		} else {
			task.Phase = TaskAssigned
			task.AssignedNodeID = candidate.NodeID
			task.AssignedGPUIDs = append([]string(nil), candidate.GPUIDs...)
			task.AssignmentToken = fmt.Sprintf("%s/%d", spec.ID, task.Attempt)
		}
	} else {
		task.FailureReason = "waiting for compatible capacity"
	}
	task.UpdatedAt = c.now().UTC()
	task.Decision = &decision
	if err := c.Tasks.UpdateTask(task); err != nil {
		return Task{}, decision, false, err
	}
	return task, decision, false, nil
}

// ReconcilePending retries only tasks that have not been assigned. It is
// called after a node heartbeat and can also be driven by a periodic worker in
// the persistent deployment.
func (c *Controller) ReconcilePending() []Task {
	c.mu.Lock()
	defer c.mu.Unlock()
	var assigned []Task
	for _, task := range c.Tasks.ListTasks() {
		if task.Phase != TaskPending {
			continue
		}
		decision, err := c.Scheduler.Schedule(task.Spec)
		if err != nil || decision.Selected == nil {
			continue
		}
		candidate := decision.Selected
		if err := c.Nodes.ReserveGPUs(candidate.NodeID, task.Spec.ID, candidate.GPUIDs); err != nil {
			continue
		}
		task.Phase = TaskAssigned
		task.AssignedNodeID = candidate.NodeID
		task.AssignedGPUIDs = append([]string(nil), candidate.GPUIDs...)
		task.AssignmentToken = fmt.Sprintf("%s/%d", task.Spec.ID, task.Attempt+1)
		task.Attempt++
		task.UpdatedAt = c.now().UTC()
		task.Decision = &decision
		if err := c.Tasks.UpdateTask(task); err == nil {
			assigned = append(assigned, task)
		}
	}
	return assigned
}

func (c *Controller) GetTask(id string) (Task, bool) { return c.Tasks.GetTask(id) }

func (c *Controller) ListTasks() []Task { return c.Tasks.ListTasks() }

func (c *Controller) TasksForNode(nodeID string) []Task {
	var result []Task
	for _, task := range c.Tasks.ListTasks() {
		if task.AssignedNodeID == nodeID && (task.Phase == TaskAssigned || task.Phase == TaskRunning) {
			result = append(result, task)
		}
	}
	return result
}

func (c *Controller) UpdateTaskPhase(id string, phase TaskPhase, reason string) (Task, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	task, ok := c.Tasks.GetTask(id)
	if !ok {
		return Task{}, fmt.Errorf("task %q does not exist", id)
	}
	if !validTransition(task.Phase, phase) {
		return Task{}, fmt.Errorf("invalid task transition %s -> %s", task.Phase, phase)
	}
	task.Phase = phase
	task.FailureReason = reason
	task.UpdatedAt = c.now().UTC()
	if phase == TaskRunning && task.StartedAt == nil {
		now := c.now().UTC()
		task.StartedAt = &now
	}
	if task.Terminal() {
		now := c.now().UTC()
		task.FinishedAt = &now
		if task.StartedAt != nil {
			task.GPUSeconds = now.Sub(*task.StartedAt).Seconds() * float64(len(task.AssignedGPUIDs))
		}
	}
	if (phase == TaskSucceeded || phase == TaskFailed || phase == TaskCancelled) && task.AssignedNodeID != "" {
		_ = c.Nodes.ReleaseGPUs(task.AssignedNodeID, task.Spec.ID)
	}
	if err := c.Tasks.UpdateTask(task); err != nil {
		return Task{}, err
	}
	return task, nil
}

func validateTaskSpec(spec TaskSpec) error {
	if spec.TenantID == "" {
		return fmt.Errorf("tenant_id is required")
	}
	if spec.GPUCount <= 0 {
		return fmt.Errorf("gpu_count must be greater than zero")
	}
	if spec.MinGPUMemoryMiB < 0 {
		return fmt.Errorf("min_gpu_memory_mib cannot be negative")
	}
	switch spec.Type {
	case TaskTraining, TaskFineTune, TaskInference:
	default:
		return fmt.Errorf("unsupported task type %q", spec.Type)
	}
	return nil
}

func validTransition(from, to TaskPhase) bool {
	if from == to {
		return true
	}
	if from == TaskSucceeded || from == TaskFailed || from == TaskCancelled {
		return false
	}
	switch from {
	case TaskPending:
		return to == TaskAssigned || to == TaskCancelling || to == TaskCancelled || to == TaskFailed
	case TaskAssigned:
		return to == TaskRunning || to == TaskCancelling || to == TaskCancelled || to == TaskFailed
	case TaskRunning:
		return to == TaskCancelling || to == TaskSucceeded || to == TaskFailed
	case TaskCancelling:
		return to == TaskCancelled || to == TaskFailed
	default:
		return false
	}
}
