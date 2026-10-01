package platform

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func petalsFixture(t *testing.T) (*Controller, DeploymentSpec) {
	t.Helper()
	c := networkController(t)
	bootstrap := "/ip4/10.0.0.9/tcp/31330/p2p/Qm" + strings.Repeat("1", 44)
	for i, n := range c.Nodes.ListNodes() {
		host := "10.0.0." + string(rune('1'+i))
		n.Agent.URL = "http://" + host + ":9090"
		n.Agent.Backends = []string{BackendPetals}
		n.Agent.EngineVersion = PetalsProfile
		n.Agent.Petals = &PetalsEndpoint{PeerID: "Qm" + strings.Repeat(string(rune('A'+i)), 44), Address: host + ":31332", InitialPeers: []string{bootstrap}}
		n.Host = &HostResources{MemoryTotalMiB: 32768, MemoryAvailableMiB: 24000}
		if err := c.Nodes.UpsertNode(n); err != nil {
			t.Fatal(err)
		}
	}
	a := ModelArtifact{ID: "sealed", Name: "Sealed model", Format: "safetensors", File: "team-model", Revision: "immutable-rev", TokenizerRevision: "immutable-tokenizer", SHA256: strings.Repeat("b", 64), Architecture: "llama", Quantization: "nf4", WeightMiB: 24000, Layers: 40, ContextLimit: 4096, BlockMiB: 300, LoadRAMMiB: 9000, KVBytesPerTokenPerLayer: 20480}
	if _, err := c.RegisterArtifact(a); err != nil {
		t.Fatal(err)
	}
	s := DeploymentSpec{Name: "Private model", Backend: BackendPetals, ModelRef: a.ID, NetworkGroup: "measured-lan", MinNodes: 2, MaxNodes: 3, ContextSize: 1024, KVCacheMiB: 1024, ReserveMiB: 1024}
	for _, pair := range [][2]string{{"a", "b"}, {"b", "a"}, {"a", "c"}, {"c", "a"}, {"b", "c"}, {"c", "b"}} {
		recordLink(t, c, pair[0], pair[1], 1000)
	}
	return c, s
}

func TestPetalsFixedBlocksReservePersistAndStop(t *testing.T) {
	c, s := petalsFixture(t)
	p, err := c.PlanDeployment(s)
	if err != nil || len(p.Placements) != 2 {
		t.Fatalf("%+v %v", p, err)
	}
	end := 0
	for _, x := range p.Placements {
		if x.StartBlock != end || x.EndBlock <= end || x.RAMMiB != 11048 || x.WeightShareMiB > 6144 {
			t.Fatalf("invalid fixed placement: %+v", x)
		}
		end = x.EndBlock
	}
	if end != 40 {
		t.Fatalf("coverage ends at %d", end)
	}
	d, _, err := c.CreateDeployment(s)
	if err != nil || d.Phase != "starting" {
		t.Fatalf("%+v %v", d, err)
	}
	for _, x := range d.Plan.Placements {
		n, _ := c.Nodes.GetNode(x.NodeID)
		if n.ReservedRAMMiB != x.RAMMiB || n.DeploymentID != d.ID {
			t.Fatal("reservation missing")
		}
		work := c.Assignments(x.NodeID)
		if len(work) != 1 || work[0].Placement.Petals.PeerID != x.Petals.PeerID || work[0].Spec.BlockMiB != 300 {
			t.Fatal(work)
		}
		if err := c.ReportWorkers(x.NodeID, []WorkerReport{{NodeID: x.NodeID, DeploymentID: d.ID, Token: d.Token, RPCState: "ready", ModelState: "ready"}}); err != nil {
			t.Fatal(err)
		}
	}
	c.ReconcileDeployments()
	if _, err = c.InferenceTarget(d.ID); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "state.json")
	if err = c.Save(file); err != nil {
		t.Fatal(err)
	}
	restarted, err := LoadController(file)
	if err != nil {
		t.Fatal(err)
	}
	loaded, _ := restarted.GetDeployment(d.ID)
	if loaded.Plan.Placements[1].EndBlock != 40 || loaded.Spec.BlockMiB != 300 {
		t.Fatal("fixed ownership lost after restart")
	}
	if _, err = restarted.InferenceTarget(d.ID); err == nil {
		t.Fatal("stale observations admitted after restart")
	}
	c.StopDeployment(d.ID)
	for _, x := range d.Plan.Placements {
		if err := c.ReportWorkers(x.NodeID, []WorkerReport{{NodeID: x.NodeID, DeploymentID: d.ID, Token: d.Token, RPCState: "stopped", ModelState: "stopped"}}); err != nil {
			t.Fatal(err)
		}
	}
	c.ReconcileDeployments()
	for _, x := range d.Plan.Placements {
		n, _ := c.Nodes.GetNode(x.NodeID)
		if n.ReservedRAMMiB != 0 || n.DeploymentID != "" {
			t.Fatal("ownership leaked")
		}
	}
}

func TestPetalsRejectsIncompatibleOrUnsafeGroups(t *testing.T) {
	for _, change := range []string{"legacy", "missing-link", "duplicate-peer", "duplicate-gpu", "bootstrap", "version", "ram", "kv", "public", "mismatch"} {
		t.Run(change, func(t *testing.T) {
			c, s := petalsFixture(t)
			s.NodeIDs = []string{"a", "b"}
			a, _ := c.Nodes.GetNode("a")
			b, _ := c.Nodes.GetNode("b")
			switch change {
			case "legacy":
				s.NetworkGroup = "lan"
			case "missing-link":
				delete(c.networkLinks, linkKey("b", "a"))
			case "duplicate-peer":
				b.Agent.Petals.PeerID = a.Agent.Petals.PeerID
			case "duplicate-gpu":
				b.GPUs[0].ID = a.GPUs[0].ID
			case "bootstrap":
				b.Agent.Petals.InitialPeers = []string{strings.Replace(a.Agent.Petals.InitialPeers[0], "10.0.0.9", "10.0.0.8", 1)}
			case "version":
				b.Agent.EngineVersion = "unpinned"
			case "ram":
				b.Host.MemoryAvailableMiB = 13000
			case "kv":
				s.KVCacheMiB = 1
			case "public":
				b.Agent.Petals.InitialPeers = []string{strings.Replace(a.Agent.Petals.InitialPeers[0], "10.0.0.9", "8.8.8.8", 1)}
			case "mismatch":
				b.Agent.Petals.Address = "10.0.0.7:31332"
			}
			c.Nodes.UpsertNode(b)
			if _, err := c.PlanDeployment(s); err == nil {
				t.Fatal("invalid deployment admitted")
			}
		})
	}
}

func TestPetalsSealedMetadataAndWorkerDegradation(t *testing.T) {
	c, s := petalsFixture(t)
	for _, bad := range []DeploymentSpec{{Name: "bad", Backend: BackendPetals, ModelFile: "raw.gguf", NetworkGroup: "lan"}, {Name: "bad", Backend: BackendRPC, ModelRef: "sealed", NetworkGroup: "lan"}} {
		if _, err := c.PlanDeployment(bad); err == nil {
			t.Fatal("wrong model format accepted")
		}
	}
	s.BlockMiB = 1
	if _, err := c.PlanDeployment(s); err == nil {
		t.Fatal("budget overwrite accepted")
	}
	s.BlockMiB = 0
	d, _, _ := c.CreateDeployment(s)
	for _, p := range d.Plan.Placements {
		if err := c.ReportWorkers(p.NodeID, []WorkerReport{{NodeID: p.NodeID, DeploymentID: d.ID, Token: d.Token, RPCState: "ready", ModelState: "ready"}}); err != nil {
			t.Fatal(err)
		}
	}
	c.ReconcileDeployments()
	p := d.Plan.Placements[len(d.Plan.Placements)-1]
	if err := c.ReportWorkers(p.NodeID, []WorkerReport{{NodeID: p.NodeID, DeploymentID: d.ID, Token: d.Token, RPCState: "degraded"}}); err != nil {
		t.Fatal(err)
	}
	c.ReconcileDeployments()
	if _, err := c.InferenceTarget(d.ID); err == nil {
		t.Fatal("degraded block admitted")
	}
	n, _ := c.Nodes.GetNode(p.NodeID)
	if n.DeploymentID != d.ID {
		t.Fatal("degraded worker released physical ownership")
	}
	c.now = func() time.Time { return time.Now().Add(time.Minute) }
	if _, err := c.InferenceTarget(d.ID); err == nil {
		t.Fatal("stale block admitted")
	}
}
