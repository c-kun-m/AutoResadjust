package platform

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"time"
)

type AgentEndpoint struct {
	Petals        *PetalsEndpoint `json:"petals,omitempty"`
	NetworkProbe  bool            `json:"network_probe,omitempty"`
	Backends      []string        `json:"backends,omitempty"`
	URL           string          `json:"url"`
	RPCAddress    string          `json:"rpc_address"`
	EngineVersion string          `json:"engine_version"`
	NetworkGroup  string          `json:"network_group"`
}

type DeploymentSpec struct {
	BlockMiB                int64    `json:"block_mib,omitempty"`
	LoadRAMMiB              int64    `json:"load_ram_mib,omitempty"`
	KVBytesPerTokenPerLayer int64    `json:"kv_bytes_per_token_per_layer,omitempty"`
	Backend                 string   `json:"backend,omitempty"`
	ModelRef                string   `json:"model_ref,omitempty"`
	Layers                  int      `json:"layers,omitempty"`
	GPULayers               int      `json:"gpu_layers,omitempty"`
	RAMMiB                  int64    `json:"ram_mib,omitempty"`
	HostReserveMiB          int64    `json:"host_reserve_mib,omitempty"`
	ModelSHA256             string   `json:"model_sha256,omitempty"`
	Name                    string   `json:"name"`
	ModelFile               string   `json:"model_file"`
	WeightMiB               int64    `json:"weight_mib"`
	ReserveMiB              int64    `json:"reserve_mib"`
	KVCacheMiB              int64    `json:"kv_cache_mib"`
	ContextSize             int      `json:"context_size"`
	MinNodes                int      `json:"min_nodes"`
	MaxNodes                int      `json:"max_nodes"`
	NetworkGroup            string   `json:"network_group"`
	NodeIDs                 []string `json:"node_ids,omitempty"`
	CoordinatorID           string   `json:"coordinator_id,omitempty"`
	IdempotencyKey          string   `json:"idempotency_key,omitempty"`
}

type Placement struct {
	Petals         *PetalsEndpoint `json:"petals,omitempty"`
	StartBlock     int             `json:"start_block,omitempty"`
	EndBlock       int             `json:"end_block,omitempty"`
	RAMMiB         int64           `json:"ram_mib,omitempty"`
	NodeID         string          `json:"node_id"`
	GPUID          string          `json:"gpu_id"`
	AgentURL       string          `json:"agent_url"`
	RPCAddress     string          `json:"rpc_address"`
	UsableMiB      int64           `json:"usable_mib"`
	WeightShareMiB int64           `json:"weight_share_mib"`
	Fraction       float64         `json:"fraction"`
	Coordinator    bool            `json:"coordinator"`
}

type DeploymentPlan struct {
	Placements     []Placement         `json:"placements"`
	TotalUsableMiB int64               `json:"total_usable_mib"`
	Rejections     map[string][]string `json:"rejections"`
	Warning        string              `json:"warning"`
}

type WorkerReport struct {
	DeploymentID string    `json:"deployment_id"`
	Token        string    `json:"token"`
	NodeID       string    `json:"node_id"`
	RPCState     string    `json:"rpc_state"`
	ModelState   string    `json:"model_state"`
	Message      string    `json:"message,omitempty"`
	ObservedAt   time.Time `json:"observed_at"`
}

type Deployment struct {
	ID            string                  `json:"id"`
	Spec          DeploymentSpec          `json:"spec"`
	Plan          DeploymentPlan          `json:"plan"`
	Phase         string                  `json:"phase"`
	Token         string                  `json:"token"`
	Message       string                  `json:"message"`
	Workers       map[string]WorkerReport `json:"workers"`
	CreatedAt     time.Time               `json:"created_at"`
	UpdatedAt     time.Time               `json:"updated_at"`
	StopRequested bool                    `json:"stop_requested"`
	Failure       bool                    `json:"failure"`
}

type WorkAssignment struct {
	DeploymentID string         `json:"deployment_id"`
	Token        string         `json:"token"`
	Spec         DeploymentSpec `json:"spec"`
	Placement    Placement      `json:"placement"`
	Peers        []Placement    `json:"peers"`
	StartModel   bool           `json:"start_model"`
	Stop         bool           `json:"stop"`
}

func normalizeDeployment(s DeploymentSpec) (DeploymentSpec, error) {
	if s.Backend == "" {
		s.Backend = BackendRPC
	}
	if s.Backend != BackendRPC && s.Backend != BackendLocal && s.Backend != BackendPetals {
		return s, fmt.Errorf("unsupported backend %q", s.Backend)
	}
	s.Name = strings.TrimSpace(s.Name)
	if s.Name == "" || len(s.Name) > 120 {
		return s, fmt.Errorf("name is required (max 120 characters)")
	}
	if s.ModelFile == "" || strings.ContainsAny(s.ModelFile, "/\\:\x00") || s.Backend != BackendPetals && !strings.HasSuffix(strings.ToLower(s.ModelFile), ".gguf") {
		return s, fmt.Errorf("model_file must be a GGUF filename inside the agent model directory")
	}
	if s.WeightMiB <= 0 || s.WeightMiB > 1<<30 {
		return s, fmt.Errorf("weight_mib must describe the actual GGUF weight size")
	}
	if s.ReserveMiB == 0 {
		s.ReserveMiB = 1024
	}
	if s.KVCacheMiB == 0 {
		s.KVCacheMiB = 512
	}
	if s.ReserveMiB < 0 || s.KVCacheMiB < 0 || s.ReserveMiB > 1<<30 || s.KVCacheMiB > 1<<30 {
		return s, fmt.Errorf("invalid memory headroom")
	}
	if s.ContextSize == 0 {
		s.ContextSize = 4096
	}
	if s.ContextSize < 128 || s.ContextSize > 131072 {
		return s, fmt.Errorf("context_size must be 128..131072")
	}
	if s.Backend == BackendPetals {
		if s.ModelRef == "" || s.ModelSHA256 == "" || !validCatalogID(s.ModelFile) || s.Layers < 1 || s.Layers > 1024 || s.BlockMiB < 1 || s.LoadRAMMiB < 1 || s.KVBytesPerTokenPerLayer < 1 {
			return s, fmt.Errorf("petals requires an immutable sealed model_ref")
		}
		if s.HostReserveMiB == 0 {
			s.HostReserveMiB = 4096
		}
		if s.HostReserveMiB < 0 || s.HostReserveMiB > 1<<30 {
			return s, fmt.Errorf("invalid host reserve")
		}
	}
	if s.Backend == BackendLocal {
		if s.MinNodes == 0 {
			s.MinNodes = 1
		}
		if s.MaxNodes == 0 {
			s.MaxNodes = 1
		}
		if s.MinNodes != 1 || s.MaxNodes != 1 {
			return s, fmt.Errorf("llama_local requires exactly one node")
		}
		if s.Layers < 1 || s.Layers > 1024 || s.GPULayers < 0 || s.GPULayers > s.Layers {
			return s, fmt.Errorf("local offload requires actual layers and gpu_layers in 0..layers")
		}
		if s.RAMMiB <= 0 || s.RAMMiB > 1<<30 {
			return s, fmt.Errorf("ram_mib must reserve host memory for the local model")
		}
		if s.HostReserveMiB == 0 {
			s.HostReserveMiB = 4096
		}
		if s.HostReserveMiB < 0 || s.HostReserveMiB > 1<<30 {
			return s, fmt.Errorf("invalid host reserve")
		}
	} else if s.MinNodes == 0 {
		s.MinNodes = 2
	}
	if s.MaxNodes == 0 {
		s.MaxNodes = 8
	}
	if (s.Backend != BackendLocal && s.MinNodes < 2) || s.MaxNodes < s.MinNodes || s.MaxNodes > 8 {
		return s, fmt.Errorf("require 2..8 nodes with max_nodes >= min_nodes")
	}
	if s.NetworkGroup == "" {
		return s, fmt.Errorf("network_group is required: RPC must run on a trusted private network")
	}
	if len(s.NodeIDs) > 8 {
		return s, fmt.Errorf("at most 8 explicit nodes")
	}
	seen := map[string]bool{}
	for _, id := range s.NodeIDs {
		if id == "" || seen[id] {
			return s, fmt.Errorf("duplicate or empty node id")
		}
		seen[id] = true
	}
	return s, nil
}

func validEndpoint(a *AgentEndpoint) bool {
	if a == nil || a.EngineVersion == "" || a.NetworkGroup == "" {
		return false
	}
	u, e := url.Parse(a.URL)
	if e != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return false
	}
	h, p, e := net.SplitHostPort(a.RPCAddress)
	return e == nil && h != "" && p != ""
}

func (c *Controller) PlanDeployment(spec DeploymentSpec) (DeploymentPlan, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, e := c.resolveDeployment(spec)
	if e != nil {
		return DeploymentPlan{}, e
	}
	return c.planDeployment(s)
}

func (c *Controller) planDeployment(s DeploymentSpec) (DeploymentPlan, error) {
	if s.Backend == BackendLocal {
		return c.planLocalDeployment(s)
	}
	if s.Backend == BackendPetals {
		return c.planPetalsDeployment(s)
	}
	plan := DeploymentPlan{Placements: []Placement{}, Rejections: map[string][]string{}, Warning: "Capacity estimate only: layer rounding, actual KV cache and workspace must pass engine load and warmup. One NVIDIA GPU per node; selected nodes are exclusive. RPC requires a trusted private network."}
	if _, ok := c.networkGroups[s.NetworkGroup]; !ok {
		plan.Warning += " Legacy group label: no measured network policy is configured."
	}
	var candidates []Placement
	versions := map[string]string{}
	nodes := c.Nodes.ListNodes()
	gpuOwners, rpcOwners := map[string]int{}, map[string]int{}
	for _, n := range nodes {
		for _, g := range n.GPUs {
			gpuOwners[g.ID]++
		}
		if n.Agent != nil {
			rpcOwners[n.Agent.RPCAddress]++
		}
	}
	for _, n := range nodes {
		if len(s.NodeIDs) > 0 && !contains(s.NodeIDs, n.ID) {
			continue
		}
		var reasons []string
		if !n.SchedulingEnabled || n.Health != NodeReady {
			reasons = append(reasons, "node is not enabled and ready")
		}
		if n.LastHeartbeat.IsZero() || c.now().Sub(n.LastHeartbeat) > 35*time.Second {
			reasons = append(reasons, "heartbeat expired")
		}
		if n.DeploymentID != "" || c.nodeBusyLocked(n.ID, n) {
			reasons = append(reasons, "node already has a task or deployment")
		}
		if !validEndpoint(n.Agent) || !supportsBackend(n.Agent, BackendRPC) {
			reasons = append(reasons, "distributed executor is not configured")
		} else if !c.networkMember(s.NetworkGroup, n) {
			reasons = append(reasons, "network group mismatch")
		}
		var selected GPU
		for _, g := range n.GPUs {
			if strings.EqualFold(g.Vendor, "NVIDIA") && g.Available() && g.FreeMemoryMiB > selected.FreeMemoryMiB {
				selected = g
			}
		}
		usable := selected.FreeMemoryMiB - s.ReserveMiB - s.KVCacheMiB
		if gpuOwners[selected.ID] > 1 || (n.Agent != nil && rpcOwners[n.Agent.RPCAddress] > 1) {
			reasons = append(reasons, "duplicate physical GPU UUID or RPC endpoint registered under different nodes")
		}
		if usable <= 0 {
			reasons = append(reasons, "no NVIDIA GPU with enough free memory after reserves")
		}
		if len(reasons) > 0 {
			plan.Rejections[n.ID] = reasons
			continue
		}
		versions[n.ID] = n.Agent.EngineVersion
		candidates = append(candidates, Placement{NodeID: n.ID, GPUID: selected.ID, AgentURL: n.Agent.URL, RPCAddress: n.Agent.RPCAddress, UsableMiB: usable})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].UsableMiB == candidates[j].UsableMiB {
			return candidates[i].NodeID < candidates[j].NodeID
		}
		return candidates[i].UsableMiB > candidates[j].UsableMiB
	})
	if s.CoordinatorID != "" {
		found := -1
		for i, p := range candidates {
			if p.NodeID == s.CoordinatorID {
				found = i
			}
		}
		if found < 0 {
			return plan, fmt.Errorf("coordinator is unavailable")
		}
		p := candidates[found]
		candidates = append([]Placement{p}, append(candidates[:found], candidates[found+1:]...)...)
	}
	// Never mix versions of the experimental RPC protocol. Try each version
	// independently so a large incompatible node cannot hide a feasible group.
	if _, measured := c.networkGroups[s.NetworkGroup]; measured {
		return c.planMeasuredRPC(s, plan, candidates, versions)
	}
	for _, seed := range candidates {
		if s.CoordinatorID != "" && seed.NodeID != s.CoordinatorID {
			continue
		}
		var ps []Placement
		var total int64
		seenRPC, seenGPU := map[string]bool{}, map[string]bool{}
		for _, p := range candidates {
			if versions[p.NodeID] != versions[seed.NodeID] {
				continue
			}
			if seenRPC[p.RPCAddress] || seenGPU[p.GPUID] {
				plan.Rejections[p.NodeID] = []string{"duplicate RPC endpoint or physical GPU UUID"}
				continue
			}
			if len(ps) >= s.MaxNodes {
				break
			}
			ps = append(ps, p)
			seenRPC[p.RPCAddress], seenGPU[p.GPUID] = true, true
			total += p.UsableMiB
			if len(ps) >= s.MinNodes && total >= s.WeightMiB && len(s.NodeIDs) == 0 {
				break
			}
		}
		if len(ps) < s.MinNodes || total < s.WeightMiB || (len(s.NodeIDs) > 0 && len(ps) != len(s.NodeIDs)) {
			continue
		}
		plan.Placements, plan.TotalUsableMiB = ps, total
		for i := range plan.Placements {
			p := &plan.Placements[i]
			p.Fraction = float64(p.UsableMiB) / float64(total)
			p.WeightShareMiB = int64(float64(s.WeightMiB) * p.Fraction)
			p.Coordinator = i == 0
		}
		return plan, nil
	}
	return plan, fmt.Errorf("no compatible group with sufficient capacity; need %d MiB across %d..%d nodes on the same engine version", s.WeightMiB, s.MinNodes, s.MaxNodes)
}

func cloneDeployment(d Deployment) Deployment {
	b, _ := json.Marshal(d)
	var out Deployment
	_ = json.Unmarshal(b, &out)
	return out
}

func (c *Controller) CreateDeployment(spec DeploymentSpec) (Deployment, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, err := c.resolveDeployment(spec)
	if err != nil {
		return Deployment{}, false, err
	}
	if s.IdempotencyKey != "" {
		if id := c.deploymentKeys[s.IdempotencyKey]; id != "" {
			d := c.deployments[id]
			a, _ := json.Marshal(s)
			b, _ := json.Marshal(d.Spec)
			if string(a) != string(b) {
				return Deployment{}, false, fmt.Errorf("idempotency key reused with different specification")
			}
			return cloneDeployment(d), true, nil
		}
	}
	now := c.now().UTC()
	id := "dep-" + strings.TrimPrefix(c.nextID(), "task-")
	d := Deployment{ID: id, Spec: s, Token: id + "/1", Phase: "pending", CreatedAt: now, UpdatedAt: now, Workers: map[string]WorkerReport{}}
	c.placeDeployment(&d)
	c.deployments[id] = d
	if s.IdempotencyKey != "" {
		c.deploymentKeys[s.IdempotencyKey] = id
	}
	return cloneDeployment(d), false, nil
}

func (c *Controller) placeDeployment(d *Deployment) {
	plan, err := c.planDeployment(d.Spec)
	d.Plan = plan
	if err == nil {
		err = c.Nodes.ReserveGroup(d.ID, plan.Placements, c.now())
	}
	if err != nil {
		d.Message = err.Error()
		return
	}
	d.Phase = "starting"
	d.Message = "group reserved; waiting for RPC workers"
	if d.Spec.Backend == BackendLocal {
		d.Message = "GPU and host RAM reserved; preparing local model"
	}
	if d.Spec.Backend == BackendPetals {
		d.Message = "GPU and host RAM reserved; verifying and loading assigned Petals blocks"
	}
}

func (c *Controller) ListDeployments() []Deployment {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []Deployment{}
	for _, d := range c.deployments {
		out = append(out, cloneDeployment(d))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}
func (c *Controller) GetDeployment(id string) (Deployment, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	d, ok := c.deployments[id]
	return cloneDeployment(d), ok
}

func (c *Controller) StopDeployment(id string) (Deployment, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	d, ok := c.deployments[id]
	if !ok {
		return d, fmt.Errorf("deployment not found")
	}
	if d.Phase == "stopped" || d.Phase == "failed" {
		return cloneDeployment(d), nil
	}
	d.StopRequested = true
	d.Phase = "stopping"
	d.Message = "waiting for every worker to confirm process termination"
	if len(d.Plan.Placements) == 0 {
		d.Phase = "stopped"
		d.Message = "deployment stopped; no resources were reserved"
	}
	d.UpdatedAt = c.now().UTC()
	c.deployments[id] = d
	return cloneDeployment(d), nil
}

func (c *Controller) ReportWorkers(node string, reports []WorkerReport) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range reports {
		d, ok := c.deployments[r.DeploymentID]
		if !ok {
			continue
		}
		if r.NodeID != node || r.Token != d.Token {
			return fmt.Errorf("worker identity or assignment token mismatch")
		}
		member := false
		for _, p := range d.Plan.Placements {
			if p.NodeID == node {
				member = true
			}
		}
		if !member {
			return fmt.Errorf("node is not assigned to deployment")
		}
		if d.Phase == "stopped" || d.Phase == "failed" {
			continue
		}
		if !contains([]string{"starting", "ready", "degraded", "failed", "stopped"}, r.RPCState) || !contains([]string{"", "starting", "ready", "degraded", "failed", "stopped"}, r.ModelState) {
			return fmt.Errorf("invalid worker state")
		}
		r.ObservedAt = c.now().UTC()
		d.Workers[node] = r
		if r.RPCState == "failed" || r.ModelState == "failed" || (r.RPCState == "stopped" && !d.StopRequested) {
			d.Failure = true
			d.StopRequested = true
			d.Message = r.Message
		}
		c.deployments[d.ID] = d
	}
	return nil
}

// ReconcileDeployments keeps ownership until every process is confirmed stopped.
// Offline does not mean stopped and never authorizes reallocating its GPU.
func (c *Controller) ReconcileDeployments() {
	c.mu.Lock()
	defer c.mu.Unlock()
	ids := make([]string, 0, len(c.deployments))
	for id := range c.deployments {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		d := c.deployments[id]
		if d.Phase == "stopped" || d.Phase == "failed" {
			continue
		}
		if d.Phase == "pending" {
			c.placeDeployment(&d)
			c.deployments[id] = d
			continue
		}
		allRPC, modelReady, allStopped, stale, unhealthy := true, false, true, false, false
		for _, p := range d.Plan.Placements {
			r, ok := d.Workers[p.NodeID]
			fresh := ok && c.now().Sub(r.ObservedAt) <= 35*time.Second
			if !fresh {
				stale = true
			}
			if !fresh || r.RPCState != "ready" {
				allRPC = false
			}
			if p.Coordinator {
				modelReady = fresh && r.ModelState == "ready"
				unhealthy = unhealthy || r.ModelState == "degraded"
			}
			unhealthy = unhealthy || r.RPCState == "degraded"
			if !ok || r.RPCState != "stopped" || (p.Coordinator && r.ModelState != "stopped") {
				allStopped = false
			}
		}
		if d.StopRequested {
			d.Phase = "stopping"
			if allStopped {
				c.Nodes.ReleaseGroup(d.ID)
				d.Phase = "stopped"
				if d.Failure {
					d.Phase = "failed"
				} else {
					d.Message = "all processes stopped; GPU and host RAM reservations released"
				}
			}
		} else if allRPC && modelReady {
			d.Phase = "ready"
			d.Message = "all workers and model health check are ready"
		} else if unhealthy {
			d.Phase = "degraded"
			d.Message = "model runtime unhealthy; new inference requests blocked; resources retained"
		} else if stale && c.now().Sub(d.CreatedAt) > 35*time.Second {
			d.Phase = "degraded"
			d.Message = "worker telemetry missing; new inference requests blocked; resources retained"
		} else if allRPC {
			d.Phase = "loading"
			d.Message = "RPC workers ready; loading and warming model"
			if d.Spec.Backend == BackendLocal {
				d.Message = "loading and warming local GPU/RAM model"
			}
			if d.Spec.Backend == BackendPetals {
				d.Message = "assigned blocks ready; loading private gateway and warming complete model"
			}
		} else {
			d.Phase = "starting"
		}
		d.UpdatedAt = c.now().UTC()
		c.deployments[id] = d
	}
}

func (c *Controller) Assignments(node string) []WorkAssignment {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []WorkAssignment{}
	for _, d := range c.deployments {
		if d.Phase == "pending" || d.Phase == "stopped" || d.Phase == "failed" {
			continue
		}
		allRPC := true
		for _, p := range d.Plan.Placements {
			r := d.Workers[p.NodeID]
			if r.RPCState != "ready" || c.now().Sub(r.ObservedAt) > 35*time.Second {
				allRPC = false
			}
		}
		for _, p := range d.Plan.Placements {
			if p.NodeID == node {
				stop := d.StopRequested
				if stop && !p.Coordinator && !d.Failure {
					for _, coordinator := range d.Plan.Placements {
						if coordinator.Coordinator && d.Workers[coordinator.NodeID].ModelState != "stopped" {
							stop = false
						}
					}
				}
				out = append(out, WorkAssignment{DeploymentID: d.ID, Token: d.Token, Spec: d.Spec, Placement: p, Peers: append([]Placement(nil), d.Plan.Placements...), StartModel: p.Coordinator && allRPC && !d.StopRequested, Stop: stop})
			}
		}
	}
	return out
}

func (c *Controller) InferenceTarget(id string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	d, ok := c.deployments[id]
	if !ok || d.Phase != "ready" || d.StopRequested {
		return "", fmt.Errorf("model service is not ready")
	}
	for _, p := range d.Plan.Placements {
		r := d.Workers[p.NodeID]
		n, present := c.Nodes.GetNode(p.NodeID)
		if !present || c.now().Sub(n.LastHeartbeat) > 35*time.Second || c.now().Sub(r.ObservedAt) > 35*time.Second || r.RPCState != "ready" || p.Coordinator && r.ModelState != "ready" {
			return "", fmt.Errorf("worker is unavailable")
		}
	}
	for _, p := range d.Plan.Placements {
		if p.Coordinator {
			return p.AgentURL, nil
		}
	}
	return "", fmt.Errorf("coordinator is missing")
}

func (s *InMemoryStore) ReserveGroup(owner string, ps []Placement, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	if owner == "" || len(ps) < 1 {
		return fmt.Errorf("deployment requires an owner and at least one node")
	}
	for _, p := range ps {
		n, ok := s.nodes[p.NodeID]
		if !ok || seen[p.NodeID] || n.DeploymentID != "" || !n.SchedulingEnabled || n.Health != NodeReady || now.Sub(n.LastHeartbeat) > 35*time.Second {
			return fmt.Errorf("node %s is unavailable for reservation", p.NodeID)
		}
		seen[p.NodeID] = true
		if p.RAMMiB > 0 && (n.Host == nil || n.Host.MemoryAvailableMiB < p.RAMMiB || n.ReservedRAMMiB != 0) {
			return fmt.Errorf("host RAM changed during planning on %s", p.NodeID)
		}
		if len(s.allocations[p.NodeID]) > 0 {
			return fmt.Errorf("node %s has existing reservations", p.NodeID)
		}
		found := false
		for _, g := range n.GPUs {
			if g.ID == p.GPUID && g.Available() && g.FreeMemoryMiB >= p.UsableMiB {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("GPU %s changed during planning", p.GPUID)
		}
	}
	for _, p := range ps {
		n := s.nodes[p.NodeID]
		n.DeploymentID = owner
		n.ReservedRAMMiB = p.RAMMiB
		if s.allocations[p.NodeID] == nil {
			s.allocations[p.NodeID] = map[string]string{}
		}
		s.allocations[p.NodeID][p.GPUID] = owner
		for i := range n.GPUs {
			if n.GPUs[i].ID == p.GPUID {
				n.GPUs[i].AllocatedTaskID = owner
			}
		}
		s.nodes[p.NodeID] = n
	}
	return nil
}

func (s *InMemoryStore) ReleaseGroup(owner string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, n := range s.nodes {
		if n.DeploymentID != owner {
			continue
		}
		for gpu, task := range s.allocations[id] {
			if task == owner {
				delete(s.allocations[id], gpu)
			}
		}
		for i := range n.GPUs {
			if n.GPUs[i].AllocatedTaskID == owner {
				n.GPUs[i].AllocatedTaskID = ""
			}
		}
		n.DeploymentID = ""
		n.ReservedRAMMiB = 0
		s.nodes[id] = n
	}
}
