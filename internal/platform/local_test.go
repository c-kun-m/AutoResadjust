package platform

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func localSpec() DeploymentSpec {
	return DeploymentSpec{Name: "offload", Backend: BackendLocal, ModelFile: "13b.gguf", WeightMiB: 14000, Layers: 40, GPULayers: 12, RAMMiB: 17000, NetworkGroup: "lan"}
}
func localController(t *testing.T) *Controller {
	t.Helper()
	c := NewController(nil)
	n := deploymentNode("a", 8192)
	n.Host = &HostResources{MemoryTotalMiB: 32768, MemoryAvailableMiB: 25000, CPUCount: 8}
	n.Agent.Backends = []string{BackendRPC, BackendLocal}
	if err := c.Nodes.UpsertNode(n); err != nil {
		t.Fatal(err)
	}
	return c
}
func TestLocalRAMReservationHeartbeatAndStop(t *testing.T) {
	c := localController(t)
	spec := localSpec()
	d, _, err := c.CreateDeployment(spec)
	if err != nil || d.Phase != "starting" || len(d.Plan.Placements) != 1 {
		t.Fatalf("%+v %v", d, err)
	}
	n, _ := c.Nodes.GetNode("a")
	if n.ReservedRAMMiB != spec.RAMMiB {
		t.Fatal(n)
	}
	n.ReservedRAMMiB = 0
	n.DeploymentID = ""
	c.Nodes.UpsertHeartbeatNode(n)
	n, _ = c.Nodes.GetNode("a")
	if n.ReservedRAMMiB != spec.RAMMiB || n.DeploymentID != d.ID {
		t.Fatal("heartbeat erased reservation")
	}
	second, _, _ := c.CreateDeployment(spec)
	if second.Phase != "pending" {
		t.Fatal("double reservation")
	}
	c.StopDeployment(second.ID)
	c.StopDeployment(d.ID)
	c.ReconcileDeployments()
	n, _ = c.Nodes.GetNode("a")
	if n.ReservedRAMMiB == 0 {
		t.Fatal("released before stop acknowledgement")
	}
	c.ReportWorkers("a", []WorkerReport{{DeploymentID: d.ID, NodeID: "a", Token: d.Token, RPCState: "stopped", ModelState: "stopped"}})
	c.ReconcileDeployments()
	n, _ = c.Nodes.GetNode("a")
	if n.ReservedRAMMiB != 0 || n.DeploymentID != "" {
		t.Fatal("reservation not released")
	}
}
func TestLocalMissingRAMCapabilitiesAndInvalidBudget(t *testing.T) {
	for _, kind := range []string{"missing_ram", "low_ram", "old_agent", "gpu_overflow", "host_headroom", "stale"} {
		t.Run(kind, func(t *testing.T) {
			c := localController(t)
			n, _ := c.Nodes.GetNode("a")
			spec := localSpec()
			switch kind {
			case "missing_ram":
				n.Host = nil
			case "low_ram":
				n.Host.MemoryAvailableMiB = 10000
			case "old_agent":
				n.Agent.Backends = nil
			case "gpu_overflow":
				spec.GPULayers = 40
			case "host_headroom":
				n.Host.MemoryAvailableMiB = spec.RAMMiB + 100
			case "stale":
				n.LastHeartbeat = time.Now().Add(-time.Minute)
			}
			c.Nodes.UpsertNode(n)
			if _, err := c.PlanDeployment(spec); err == nil {
				t.Fatal("unsafe local placement accepted")
			}
		})
	}
}
func TestArtifactsImmutableAndSnapshotMigration(t *testing.T) {
	c := localController(t)
	a := ModelArtifact{ID: "13b-q8", Name: "13B", Format: "gguf", File: "13b.gguf", Revision: "fixed-commit", SHA256: strings.Repeat("a", 64), Architecture: "llama", Quantization: "Q8_0", WeightMiB: 14000, Layers: 40, ContextLimit: 2048}
	if _, err := c.RegisterArtifact(a); err != nil {
		t.Fatal(err)
	}
	a.Revision = "changed"
	if _, err := c.RegisterArtifact(a); err == nil {
		t.Fatal("mutable artifact accepted")
	}
	spec := localSpec()
	spec.ModelRef = a.ID
	spec.ModelFile = ""
	spec.WeightMiB = 0
	spec.Layers = 0
	d, _, err := c.CreateDeployment(spec)
	if err != nil || d.Spec.ModelSHA256 != a.SHA256 || d.Spec.ContextSize != 2048 {
		t.Fatalf("%+v %v", d, err)
	}
	file := filepath.Join(t.TempDir(), "state.json")
	if err = c.Save(file); err != nil {
		t.Fatal(err)
	}
	restored, err := LoadController(file)
	if err != nil || len(restored.ListArtifacts()) != 1 {
		t.Fatal(err)
	}
	n, _ := restored.Nodes.GetNode("a")
	if n.ReservedRAMMiB != spec.RAMMiB || !n.LastHeartbeat.IsZero() {
		t.Fatal("restored capacity lost ownership/freshness")
	}
	legacy := snapshot{Version: 1, Deployments: map[string]Deployment{"old": {ID: "old", Spec: deploymentSpec(), Phase: "ready"}}}
	b, _ := json.Marshal(legacy)
	os.WriteFile(file, b, 0600)
	restored, err = LoadController(file)
	if err != nil {
		t.Fatal(err)
	}
	old, _ := restored.GetDeployment("old")
	if old.Spec.Backend != BackendRPC || old.Phase != "degraded" {
		t.Fatal(old)
	}
}
func TestNodeObservationsAreCopied(t *testing.T) {
	c := localController(t)
	updated, err := c.UpdateNode("a", ResourceNode{Name: "renamed", Host: &HostResources{MemoryTotalMiB: 1}})
	if err != nil || updated.Host == nil || updated.Host.MemoryTotalMiB != 32768 {
		t.Fatalf("configuration update changed host observation: %+v %v", updated.Host, err)
	}
	n, _ := c.Nodes.GetNode("a")
	n.Host.MemoryAvailableMiB = 1
	n.Agent.Backends[0] = "bad"
	n, _ = c.Nodes.GetNode("a")
	if n.Host.MemoryAvailableMiB == 1 || n.Agent.Backends[0] == "bad" {
		t.Fatal("mutable observation escaped store")
	}
}
