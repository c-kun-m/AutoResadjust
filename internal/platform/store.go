package platform

import (
	"fmt"
	"sort"
	"sync"
)

// NodeStore and TaskStore are narrow ports used by the scheduler and HTTP
// layer. PostgreSQL can implement these interfaces without changing domain
// logic; the first executable version uses InMemoryStore for local bring-up.
type NodeStore interface {
	UpsertNode(ResourceNode) error
	GetNode(string) (ResourceNode, bool)
	ListNodes() []ResourceNode
}

// ResourceAllocator is implemented by stores that can atomically claim a set
// of GPUs. The control plane should reserve immediately after selecting a
// candidate; this prevents two concurrent scheduler workers from selecting
// the same devices.
type ResourceAllocator interface {
	NodeStore
	ReserveGPUs(nodeID, taskID string, gpuIDs []string) error
	ReleaseGPUs(nodeID, taskID string, gpuIDs ...[]string) error
}

type TaskStore interface {
	CreateTask(Task) error
	GetTask(string) (Task, bool)
	UpdateTask(Task) error
	ListTasks() []Task
}

type InMemoryStore struct {
	mu          sync.RWMutex
	nodes       map[string]ResourceNode
	tasks       map[string]Task
	allocations map[string]map[string]string
	blocked     map[string]bool
}

func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{nodes: make(map[string]ResourceNode), tasks: make(map[string]Task), allocations: make(map[string]map[string]string), blocked: make(map[string]bool)}
}

func (s *InMemoryStore) UpsertNode(node ResourceNode) error {
	return s.upsertNode(node, false)
}

// UpsertHeartbeatNode applies an observation from an edge agent.  Hardware
// observations are allowed to replace the GPU inventory, but operator-owned
// scheduling state and control-plane reservations remain authoritative.
func (s *InMemoryStore) UpsertHeartbeatNode(node ResourceNode) error {
	return s.upsertNode(node, true)
}

func (s *InMemoryStore) upsertNode(node ResourceNode, heartbeat bool) error {
	if node.ID == "" {
		return fmt.Errorf("node id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if heartbeat && s.blocked[node.ID] {
		return fmt.Errorf("node %q is decommissioned; register it again before accepting heartbeats", node.ID)
	}
	// An explicit operator registration is the re-admission operation after a
	// node has been decommissioned.
	if !heartbeat {
		delete(s.blocked, node.ID)
	}
	previous, hadPrevious := s.nodes[node.ID]
	node.DeploymentID = previous.DeploymentID
	node.ReservedRAMMiB = previous.ReservedRAMMiB
	if heartbeat && !hadPrevious {
		node.SchedulingEnabled = false
	}
	node = cloneNode(node)
	if hadPrevious {
		if heartbeat {
			node.Name, node.Datacenter, node.Region = previous.Name, previous.Datacenter, previous.Region
			node.Labels = cloneStringMap(previous.Labels)
		}
		// Queue and allocation metadata are control-plane owned. A hardware
		// heartbeat must not erase them with an empty observation.
		node.QueueDepth = previous.QueueDepth
		node.RunningTasks = previous.RunningTasks
		if node.CachedModels == nil {
			node.CachedModels = cloneBoolMap(previous.CachedModels)
		}
		if node.DataReady == nil {
			node.DataReady = cloneBoolMap(previous.DataReady)
		}
		if node.CostPerGPUHour == 0 {
			node.CostPerGPUHour = previous.CostPerGPUHour
		}
		if heartbeat {
			// A heartbeat must never undo an operator drain/disable action.  The
			// next explicit enable operation is the only way to re-open scheduling.
			if !previous.SchedulingEnabled {
				node.SchedulingEnabled = false
			}
			if previous.Health == NodeDraining {
				node.Health = NodeDraining
			}
		}
	}
	// Heartbeats report observations, never ownership. Preserve reservations even
	// when a GPU temporarily disappears from the inventory and later returns.
	for i := range node.GPUs {
		node.GPUs[i].AllocatedTaskID = s.allocations[node.ID][node.GPUs[i].ID]
	}
	s.nodes[node.ID] = cloneNode(node)
	return nil
}

// DeleteNode removes a node from the control-plane catalog.  Callers are
// responsible for checking task ownership before invoking this method.
func (s *InMemoryStore) DeleteNode(id string) error {
	if id == "" {
		return fmt.Errorf("node id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.nodes[id]; !ok {
		return fmt.Errorf("node %q does not exist", id)
	}
	if len(s.allocations[id]) > 0 {
		return fmt.Errorf("node %q has GPU reservations", id)
	}
	delete(s.nodes, id)
	delete(s.allocations, id)
	s.blocked[id] = true
	return nil
}

func (s *InMemoryStore) GetNode(id string) (ResourceNode, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	node, ok := s.nodes[id]
	if !ok {
		return ResourceNode{}, false
	}
	return cloneNode(node), true
}

func (s *InMemoryStore) ListNodes() []ResourceNode {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]ResourceNode, 0, len(s.nodes))
	for _, node := range s.nodes {
		result = append(result, cloneNode(node))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (s *InMemoryStore) ReserveGPUs(nodeID, taskID string, gpuIDs []string) error {
	if taskID == "" || nodeID == "" || len(gpuIDs) == 0 {
		return fmt.Errorf("node id, task id and gpu ids are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	node, ok := s.nodes[nodeID]
	if !ok {
		return fmt.Errorf("node %q does not exist", nodeID)
	}
	if node.DeploymentID != "" {
		return fmt.Errorf("node belongs to deployment %s", node.DeploymentID)
	}
	wanted := make(map[string]struct{}, len(gpuIDs))
	for _, id := range gpuIDs {
		if id == "" {
			return fmt.Errorf("gpu id cannot be empty")
		}
		if _, duplicate := wanted[id]; duplicate {
			return fmt.Errorf("gpu %q appears more than once", id)
		}
		wanted[id] = struct{}{}
	}
	for _, gpu := range node.GPUs {
		if _, ok := wanted[gpu.ID]; !ok {
			continue
		}
		if gpu.AllocatedTaskID != "" && gpu.AllocatedTaskID != taskID {
			return fmt.Errorf("gpu %q is already allocated or has no free memory", gpu.ID)
		}
		if gpu.AllocatedTaskID == "" && gpu.FreeMemoryMiB <= 0 {
			return fmt.Errorf("gpu %q is already allocated or has no free memory", gpu.ID)
		}
	}
	for id := range wanted {
		found := false
		for _, gpu := range node.GPUs {
			if gpu.ID == id {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("gpu %q does not exist on node %q", id, nodeID)
		}
	}
	for i := range node.GPUs {
		if _, ok := wanted[node.GPUs[i].ID]; ok {
			node.GPUs[i].AllocatedTaskID = taskID
			if s.allocations[nodeID] == nil {
				s.allocations[nodeID] = make(map[string]string)
			}
			s.allocations[nodeID][node.GPUs[i].ID] = taskID
		}
	}
	s.nodes[nodeID] = cloneNode(node)
	return nil
}

func (s *InMemoryStore) ReleaseGPUs(nodeID, taskID string, gpuIDs ...[]string) error {
	if taskID == "" || nodeID == "" {
		return fmt.Errorf("node id and task id are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	node, ok := s.nodes[nodeID]
	if !ok {
		return fmt.Errorf("node %q does not exist", nodeID)
	}
	requested := make(map[string]struct{})
	if len(gpuIDs) > 0 {
		for _, id := range gpuIDs[0] {
			requested[id] = struct{}{}
		}
	}
	for id, owner := range s.allocations[nodeID] {
		_, selected := requested[id]
		if owner == taskID && (len(requested) == 0 || selected) {
			delete(s.allocations[nodeID], id)
		}
	}
	for i := range node.GPUs {
		_, requestedExplicitly := requested[node.GPUs[i].ID]
		if (len(requested) == 0 || requestedExplicitly) && node.GPUs[i].AllocatedTaskID == taskID {
			node.GPUs[i].AllocatedTaskID = ""
		}
	}
	s.nodes[nodeID] = cloneNode(node)
	return nil
}

func (s *InMemoryStore) CreateTask(task Task) error {
	if task.Spec.ID == "" {
		return fmt.Errorf("task id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.tasks[task.Spec.ID]; exists {
		return fmt.Errorf("task %q already exists", task.Spec.ID)
	}
	s.tasks[task.Spec.ID] = cloneTask(task)
	return nil
}

func (s *InMemoryStore) GetTask(id string) (Task, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	task, ok := s.tasks[id]
	if !ok {
		return Task{}, false
	}
	return cloneTask(task), true
}

func (s *InMemoryStore) UpdateTask(task Task) error {
	if task.Spec.ID == "" {
		return fmt.Errorf("task id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.tasks[task.Spec.ID]; !exists {
		return fmt.Errorf("task %q does not exist", task.Spec.ID)
	}
	s.tasks[task.Spec.ID] = cloneTask(task)
	return nil
}

func (s *InMemoryStore) ListTasks() []Task {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]Task, 0, len(s.tasks))
	for _, task := range s.tasks {
		result = append(result, cloneTask(task))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Spec.ID < result[j].Spec.ID })
	return result
}

func cloneNode(in ResourceNode) ResourceNode {
	out := in
	if in.Agent != nil {
		a := *in.Agent
		a.Backends = append([]string(nil), in.Agent.Backends...)
		out.Agent = &a
	}
	if in.Host != nil {
		h := *in.Host
		if h.CPUUtilizationPct != nil {
			v := *h.CPUUtilizationPct
			h.CPUUtilizationPct = &v
		}
		out.Host = &h
	}
	out.Labels = cloneStringMap(in.Labels)
	out.CachedModels = cloneBoolMap(in.CachedModels)
	out.DataReady = cloneBoolMap(in.DataReady)
	out.GPUs = append([]GPU(nil), in.GPUs...)
	for i := range out.GPUs {
		if in.GPUs[i].UtilizationPct != nil {
			v := *in.GPUs[i].UtilizationPct
			out.GPUs[i].UtilizationPct = &v
		}
	}
	return out
}

func cloneTask(in Task) Task {
	out := in
	out.Spec.RequiredLabels = cloneStringMap(in.Spec.RequiredLabels)
	out.Spec.AllowedDatacenters = append([]string(nil), in.Spec.AllowedDatacenters...)
	out.Spec.DeniedDatacenters = append([]string(nil), in.Spec.DeniedDatacenters...)
	out.Spec.AllowedRegions = append([]string(nil), in.Spec.AllowedRegions...)
	out.AssignedGPUIDs = append([]string(nil), in.AssignedGPUIDs...)
	if in.StartedAt != nil {
		t := *in.StartedAt
		out.StartedAt = &t
	}
	if in.FinishedAt != nil {
		t := *in.FinishedAt
		out.FinishedAt = &t
	}
	if in.Decision != nil {
		d := *in.Decision
		d.Candidates = append([]Candidate(nil), in.Decision.Candidates...)
		for i := range d.Candidates {
			d.Candidates[i].GPUIDs = append([]string(nil), d.Candidates[i].GPUIDs...)
		}
		if d.Selected != nil {
			v := *d.Selected
			v.GPUIDs = append([]string(nil), v.GPUIDs...)
			d.Selected = &v
		}
		d.RejectionReasons = make(map[string][]string)
		for k, v := range in.Decision.RejectionReasons {
			d.RejectionReasons[k] = append([]string(nil), v...)
		}
		out.Decision = &d
	}
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneBoolMap(in map[string]bool) map[string]bool {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]bool, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
