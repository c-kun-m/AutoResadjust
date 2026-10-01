package platform

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

type SchedulerWeights struct {
	Queue      float64
	Load       float64
	ModelCache float64
	DataReady  float64
	Cost       float64
}

func DefaultSchedulerWeights() SchedulerWeights {
	return SchedulerWeights{Queue: 0.30, Load: 0.25, ModelCache: 0.20, DataReady: 0.15, Cost: 0.10}
}

type Scheduler struct {
	Nodes            NodeStore
	Weights          SchedulerWeights
	Now              func() time.Time
	HeartbeatTimeout time.Duration
}

func NewScheduler(nodes NodeStore, weights SchedulerWeights) *Scheduler {
	if weights == (SchedulerWeights{}) {
		weights = DefaultSchedulerWeights()
	}
	return &Scheduler{Nodes: nodes, Weights: weights, Now: time.Now, HeartbeatTimeout: 35 * time.Second}
}

// Schedule implements the platform's first version policy: hard constraints
// filter nodes, then a deterministic weighted score ranks the survivors.
func (s *Scheduler) Schedule(task TaskSpec) (SchedulingDecision, error) {
	if task.ID == "" {
		return SchedulingDecision{}, fmt.Errorf("task id is required")
	}
	if task.GPUCount <= 0 {
		return SchedulingDecision{}, fmt.Errorf("task %q must request at least one gpu", task.ID)
	}
	if s == nil || s.Nodes == nil {
		return SchedulingDecision{}, fmt.Errorf("node store is required")
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	decision := SchedulingDecision{TaskID: task.ID, RejectionReasons: make(map[string][]string)}
	for _, node := range s.Nodes.ListNodes() {
		gpus, reasons := filterNode(node, task)
		if node.LastHeartbeat.IsZero() || now.Sub(node.LastHeartbeat) > s.HeartbeatTimeout {
			reasons = append(reasons, "heartbeat is stale; execution state requires reconciliation")
		}
		if len(reasons) > 0 {
			decision.RejectionReasons[node.ID] = reasons
			continue
		}
		breakdown := s.score(node, task, gpus)
		decision.Candidates = append(decision.Candidates, Candidate{NodeID: node.ID, GPUIDs: gpuIDs(gpus), Score: breakdown.Total, ScoreBreakdown: breakdown})
	}
	if len(decision.Candidates) == 0 {
		return decision, fmt.Errorf("no schedulable node for task %q", task.ID)
	}
	sort.SliceStable(decision.Candidates, func(i, j int) bool {
		if decision.Candidates[i].Score == decision.Candidates[j].Score {
			return decision.Candidates[i].NodeID < decision.Candidates[j].NodeID
		}
		return decision.Candidates[i].Score > decision.Candidates[j].Score
	})
	decision.Selected = &decision.Candidates[0]
	return decision, nil
}

func filterNode(node ResourceNode, task TaskSpec) ([]GPU, []string) {
	var reasons []string
	if node.DeploymentID != "" {
		reasons = append(reasons, "node is reserved by a distributed deployment")
	}
	if node.ID == "" {
		reasons = append(reasons, "node has no id")
	}
	if !node.SchedulingEnabled {
		reasons = append(reasons, "scheduling is disabled")
	}
	if node.Health != NodeReady {
		reasons = append(reasons, "node health is "+string(node.Health))
	}
	if task.RequiredRuntime != "" && task.RequiredRuntime != node.Runtime {
		reasons = append(reasons, "runtime is incompatible")
	}
	if !containsOrEmpty(task.AllowedDatacenters, node.Datacenter) {
		reasons = append(reasons, "datacenter is not allowed")
	}
	if contains(task.DeniedDatacenters, node.Datacenter) {
		reasons = append(reasons, "datacenter is denied")
	}
	if !containsOrEmpty(task.AllowedRegions, node.Region) {
		reasons = append(reasons, "region is not allowed")
	}
	for key, expected := range task.RequiredLabels {
		if node.Labels[key] != expected {
			reasons = append(reasons, fmt.Sprintf("label %q must be %q", key, expected))
		}
	}
	available := make([]GPU, 0, len(node.GPUs))
	for _, gpu := range node.GPUs {
		if !gpu.Available() || gpu.FreeMemoryMiB < task.MinGPUMemoryMiB {
			continue
		}
		if task.RequiredVendor != "" && !strings.EqualFold(gpu.Vendor, task.RequiredVendor) {
			continue
		}
		if task.RequiredGPUModel != "" && !strings.EqualFold(gpu.Model, task.RequiredGPUModel) {
			continue
		}
		available = append(available, gpu)
	}
	if len(available) < task.GPUCount {
		reasons = append(reasons, fmt.Sprintf("need %d compatible free gpus, found %d", task.GPUCount, len(available)))
		return nil, reasons
	}
	selected := selectGPUGroup(available, task.GPUCount)
	if len(selected) < task.GPUCount {
		reasons = append(reasons, "requested gpus are not available in one topology group")
		return nil, reasons
	}
	return selected, reasons
}

func selectGPUGroup(available []GPU, count int) []GPU {
	groups := make(map[string][]GPU)
	for _, gpu := range available {
		groupKey := gpu.TopologyGroup
		if groupKey == "" {
			// Unknown topology must not silently permit a multi-card placement;
			// treat each device as an isolated group until the agent reports it.
			groupKey = "__unknown__:" + gpu.ID
		}
		groups[groupKey] = append(groups[groupKey], gpu)
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var best []GPU
	for _, key := range keys {
		group := groups[key]
		sort.Slice(group, func(i, j int) bool { return group[i].Index < group[j].Index })
		if len(group) >= count && (len(best) == 0 || group[0].Index < best[0].Index) {
			best = group[:count]
		}
	}
	return best
}

func (s *Scheduler) score(node ResourceNode, task TaskSpec, gpus []GPU) ScoreBreakdown {
	queue := 1.0 / float64(1+node.QueueDepth)
	load := 1.0 / float64(1+node.RunningTasks)
	cache := 0.0
	if task.ModelID != "" && node.CachedModels[task.ModelID] {
		cache = 1
	}
	data := 0.0
	if task.DataID != "" && node.DataReady[task.DataID] {
		data = 1
	}
	cost := 0.0
	if node.CostPerGPUHour <= 0 {
		cost = 1
	} else {
		// Cost score is normalized against a practical ceiling. The value is
		// intentionally monotonic and can be replaced by a catalog percentile.
		cost = 1 / (1 + node.CostPerGPUHour)
	}
	return ScoreBreakdown{
		Queue:      queue * s.Weights.Queue,
		Load:       load * s.Weights.Load,
		ModelCache: cache * s.Weights.ModelCache,
		DataReady:  data * s.Weights.DataReady,
		Cost:       cost * s.Weights.Cost,
		Total:      queue*s.Weights.Queue + load*s.Weights.Load + cache*s.Weights.ModelCache + data*s.Weights.DataReady + cost*s.Weights.Cost,
	}
}

func gpuIDs(gpus []GPU) []string {
	ids := make([]string, len(gpus))
	for i, gpu := range gpus {
		ids[i] = gpu.ID
	}
	return ids
}

func contains(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

func containsOrEmpty(values []string, value string) bool {
	return len(values) == 0 || contains(values, value)
}
