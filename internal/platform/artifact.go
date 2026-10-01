package platform

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	BackendRPC    = "llama_rpc"
	BackendLocal  = "llama_local"
	BackendPetals = "petals"
)

// Artifacts are immutable: a new revision/quantization must get a new ID.
// Registration does not download weights or claim a hardware compatibility test.
type ModelArtifact struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Format       string `json:"format"`
	File         string `json:"file"`
	Revision     string `json:"revision"`
	SHA256       string `json:"sha256"`
	Architecture string `json:"architecture"`
	Quantization string `json:"quantization"`
	ChatTemplate string `json:"chat_template,omitempty"`
	WeightMiB    int64  `json:"weight_mib"`
	Layers       int    `json:"layers"`
	ContextLimit int    `json:"context_limit"`
}

func validCatalogID(id string) bool {
	if len(id) == 0 || len(id) > 120 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return id != "." && id != ".."
}

func (c *Controller) RegisterArtifact(a ModelArtifact) (ModelArtifact, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !validCatalogID(a.ID) || a.Name == "" || len(a.Name) > 120 {
		return a, fmt.Errorf("artifact id and name are required")
	}
	if a.Format != "gguf" {
		return a, fmt.Errorf("supported artifact format: gguf")
	}
	if a.File == "" || strings.ContainsAny(a.File, "/\\:\x00") || !strings.HasSuffix(strings.ToLower(a.File), ".gguf") {
		return a, fmt.Errorf("artifact file must be a GGUF basename")
	}
	if a.Revision == "" || a.Architecture == "" || a.Quantization == "" {
		return a, fmt.Errorf("revision, architecture and quantization are required")
	}
	hash, err := hex.DecodeString(a.SHA256)
	if err != nil || len(hash) != 32 {
		return a, fmt.Errorf("sha256 must contain 64 hexadecimal characters")
	}
	a.SHA256 = strings.ToLower(a.SHA256)
	if a.WeightMiB <= 0 || a.WeightMiB > 1<<30 || a.Layers < 1 || a.Layers > 1024 || a.ContextLimit < 128 || a.ContextLimit > 131072 {
		return a, fmt.Errorf("invalid model capacity metadata")
	}
	if prev, ok := c.artifacts[a.ID]; ok {
		x, _ := json.Marshal(prev)
		y, _ := json.Marshal(a)
		if string(x) != string(y) {
			return a, fmt.Errorf("artifact is immutable; register a new id for different content")
		}
		return prev, nil
	}
	c.artifacts[a.ID] = a
	return a, nil
}

func (c *Controller) ListArtifacts() []ModelArtifact {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ModelArtifact, 0, len(c.artifacts))
	for _, a := range c.artifacts {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (c *Controller) resolveDeployment(s DeploymentSpec) (DeploymentSpec, error) {
	if s.ModelRef != "" {
		a, ok := c.artifacts[s.ModelRef]
		if !ok {
			return s, fmt.Errorf("unknown model_ref %q", s.ModelRef)
		}
		if s.ModelFile != "" && s.ModelFile != a.File || s.WeightMiB != 0 && s.WeightMiB != a.WeightMiB || s.Layers != 0 && s.Layers != a.Layers || s.ModelSHA256 != "" && s.ModelSHA256 != a.SHA256 {
			return s, fmt.Errorf("deployment conflicts with immutable artifact metadata")
		}
		s.ModelFile, s.WeightMiB, s.Layers, s.ModelSHA256 = a.File, a.WeightMiB, a.Layers, a.SHA256
		if s.ContextSize == 0 {
			s.ContextSize = a.ContextLimit
			if s.ContextSize > 4096 {
				s.ContextSize = 4096
			}
		}
		if s.ContextSize > a.ContextLimit {
			return s, fmt.Errorf("context exceeds artifact context_limit")
		}
	}
	return normalizeDeployment(s)
}

func supportsBackend(a *AgentEndpoint, backend string) bool {
	if a == nil {
		return false
	}
	if len(a.Backends) == 0 {
		return backend == BackendRPC
	} // old agents only speak RPC
	return contains(a.Backends, backend)
}

func validAgentURL(a *AgentEndpoint) bool {
	if a == nil || a.EngineVersion == "" || a.NetworkGroup == "" {
		return false
	}
	u, err := url.Parse(a.URL)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && u.Path == ""
}

func (c *Controller) planLocalDeployment(s DeploymentSpec) (DeploymentPlan, error) {
	p := DeploymentPlan{Placements: []Placement{}, Rejections: map[string][]string{}, Warning: "GPU layer capacity is an estimate; engine loading and warmup must succeed. Host RAM reserves the full file plus KV/workspace to cover loading peaks. Disk offload is disabled."}
	if len(s.NodeIDs) > 1 {
		return p, fmt.Errorf("llama_local accepts at most one node")
	}
	// Reserve the full file in host RAM even when some weights move to the GPU.
	if s.RAMMiB < s.WeightMiB+s.KVCacheMiB+s.ReserveMiB {
		return p, fmt.Errorf("ram_mib must cover full weights, KV cache and workspace (%d MiB)", s.WeightMiB+s.KVCacheMiB+s.ReserveMiB)
	}
	gpuWeights := (s.WeightMiB*int64(s.GPULayers+1) + int64(s.Layers)) / int64(s.Layers+1)
	nodes := c.Nodes.ListNodes()
	owners := map[string]int{}
	for _, n := range nodes {
		for _, g := range n.GPUs {
			owners[g.ID]++
		}
	}
	for _, n := range nodes {
		if len(s.NodeIDs) > 0 && n.ID != s.NodeIDs[0] || s.CoordinatorID != "" && n.ID != s.CoordinatorID {
			continue
		}
		reasons := []string{}
		if !n.SchedulingEnabled || n.Health != NodeReady || n.LastHeartbeat.IsZero() || c.now().Sub(n.LastHeartbeat) > 35*time.Second {
			reasons = append(reasons, "node is disabled, unavailable or heartbeat expired")
		}
		if n.DeploymentID != "" || c.nodeBusyLocked(n.ID, n) {
			reasons = append(reasons, "node already reserved")
		}
		if !validAgentURL(n.Agent) || !supportsBackend(n.Agent, BackendLocal) {
			reasons = append(reasons, "llama_local executor unavailable")
		} else if n.Agent.NetworkGroup != s.NetworkGroup {
			reasons = append(reasons, "network group mismatch")
		}
		if n.Host == nil || n.Host.MemoryAvailableMiB-n.ReservedRAMMiB-s.HostReserveMiB < s.RAMMiB {
			reasons = append(reasons, "host memory unavailable or below requested RAM plus host reserve")
		}
		var gpu GPU
		for _, g := range n.GPUs {
			if strings.EqualFold(g.Vendor, "NVIDIA") && g.Available() && g.FreeMemoryMiB > gpu.FreeMemoryMiB {
				gpu = g
			}
		}
		if gpu.ID == "" || owners[gpu.ID] > 1 || gpu.FreeMemoryMiB < gpuWeights+s.KVCacheMiB+s.ReserveMiB {
			reasons = append(reasons, "GPU missing, duplicated or below estimated layer memory plus reserves")
		}
		if len(reasons) > 0 {
			p.Rejections[n.ID] = reasons
			continue
		}
		p.Placements = append(p.Placements, Placement{NodeID: n.ID, GPUID: gpu.ID, AgentURL: n.Agent.URL, UsableMiB: gpuWeights, WeightShareMiB: gpuWeights, Fraction: float64(gpuWeights) / float64(s.WeightMiB), Coordinator: true, RAMMiB: s.RAMMiB})
	}
	if len(p.Placements) == 0 {
		return p, fmt.Errorf("no local node satisfies GPU and host RAM budgets")
	}
	sort.Slice(p.Placements, func(i, j int) bool { return p.Placements[i].NodeID < p.Placements[j].NodeID })
	p.Placements = p.Placements[:1]
	p.TotalUsableMiB = p.Placements[0].UsableMiB
	return p, nil
}
