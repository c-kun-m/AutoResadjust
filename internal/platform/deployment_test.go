package platform

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func deploymentNode(id string, memory int64) ResourceNode {
	n := testNode(id, "dc", NodeReady, 0, false, "")
	n.Runtime = "wsl2-docker"
	n.GPUs = n.GPUs[:1]
	n.GPUs[0].MemoryMiB = memory
	n.GPUs[0].FreeMemoryMiB = memory
	n.Agent = &AgentEndpoint{URL: "http://" + id + ":9090", RPCAddress: id + ":50052", NetworkGroup: "lan", EngineVersion: "test-revision"}
	return n
}
func deploymentSpec() DeploymentSpec {
	return DeploymentSpec{Name: "test", ModelFile: "test.gguf", WeightMiB: 12000, MinNodes: 2, MaxNodes: 3, NetworkGroup: "lan"}
}
func groupController(t *testing.T) *Controller {
	t.Helper()
	c := NewController(nil)
	for _, id := range []string{"a", "b", "c"} {
		if err := c.Nodes.UpsertNode(deploymentNode(id, 8192)); err != nil {
			t.Fatal(err)
		}
	}
	return c
}
func TestDistributedPlanAndExclusiveReservation(t *testing.T) {
	c := groupController(t)
	plan, err := c.PlanDeployment(deploymentSpec())
	if err != nil || len(plan.Placements) != 2 {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	d, _, err := c.CreateDeployment(deploymentSpec())
	if err != nil || d.Phase != "starting" {
		t.Fatalf("create: %+v %v", d, err)
	}
	for _, p := range d.Plan.Placements {
		n, _ := c.Nodes.GetNode(p.NodeID)
		if n.DeploymentID != d.ID || n.GPUs[0].AllocatedTaskID != d.ID {
			t.Fatal("group ownership missing")
		}
		if err := c.Nodes.ReserveGPUs(n.ID, "legacy", []string{n.GPUs[0].ID}); err == nil {
			t.Fatal("legacy task bypassed group lease")
		}
		if err := c.DeleteNode(n.ID); err == nil {
			t.Fatal("deleted reserved node")
		}
	}
	second, _, _ := c.CreateDeployment(deploymentSpec())
	if second.Phase != "pending" {
		t.Fatal("oversubscribed GPUs")
	}
}
func TestGroupReservationRollsBackOnConflict(t *testing.T) {
	c := groupController(t)
	plan, _ := c.PlanDeployment(deploymentSpec())
	p := plan.Placements[1]
	if err := c.Nodes.ReserveGPUs(p.NodeID, "busy", []string{p.GPUID}); err != nil {
		t.Fatal(err)
	}
	if err := c.Nodes.ReserveGroup("dep-x", plan.Placements, time.Now()); err == nil {
		t.Fatal("expected conflict")
	}
	n, _ := c.Nodes.GetNode(plan.Placements[0].NodeID)
	if n.DeploymentID != "" || n.GPUs[0].AllocatedTaskID != "" {
		t.Fatal("partial group reservation leaked")
	}
}

func TestDuplicatePhysicalResourcesCannotInflateCapacity(t *testing.T) {
	for _, duplicate := range []string{"gpu", "rpc"} {
		t.Run(duplicate, func(t *testing.T) {
			c := groupController(t)
			a, _ := c.Nodes.GetNode("a")
			b, _ := c.Nodes.GetNode("b")
			if duplicate == "gpu" {
				b.GPUs[0].ID = a.GPUs[0].ID
			} else {
				b.Agent.RPCAddress = a.Agent.RPCAddress
			}
			if err := c.Nodes.UpsertNode(b); err != nil {
				t.Fatal(err)
			}
			p, err := c.PlanDeployment(deploymentSpec())
			if err == nil || len(p.Rejections["a"]) == 0 || len(p.Rejections["b"]) == 0 {
				t.Fatalf("duplicate resource counted: %+v %v", p, err)
			}
		})
	}
}
func TestConcurrentDeploymentsDoNotDoubleAllocate(t *testing.T) {
	c := groupController(t)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _, _ = c.CreateDeployment(deploymentSpec()) }()
	}
	wg.Wait()
	n := 0
	for _, d := range c.ListDeployments() {
		if d.Phase == "starting" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("allocated %d deployments", n)
	}
}
func TestDeploymentVersionAndMemoryConstraints(t *testing.T) {
	c := groupController(t)
	n, _ := c.Nodes.GetNode("a")
	n.Agent.EngineVersion = "different"
	c.Nodes.UpsertNode(n)
	p, err := c.PlanDeployment(deploymentSpec())
	if err != nil || p.Placements[0].NodeID == "a" {
		t.Fatalf("mixed protocol versions: %+v %v", p, err)
	}
	s := deploymentSpec()
	s.CoordinatorID = "a"
	if _, err = c.PlanDeployment(s); err == nil {
		t.Fatal("ignored coordinator version")
	}
	s = deploymentSpec()
	s.NodeIDs = []string{"a", "b"}
	if _, err = c.PlanDeployment(s); err == nil {
		t.Fatal("ignored explicit node set")
	}
}
func TestDeploymentLifecycleStaleWorkerAndConfirmedStop(t *testing.T) {
	c := groupController(t)
	d, _, _ := c.CreateDeployment(deploymentSpec())
	for _, p := range d.Plan.Placements {
		r := WorkerReport{NodeID: p.NodeID, DeploymentID: d.ID, Token: d.Token, RPCState: "ready"}
		if p.Coordinator {
			r.ModelState = "ready"
		}
		if err := c.ReportWorkers(p.NodeID, []WorkerReport{r}); err != nil {
			t.Fatal(err)
		}
	}
	c.ReconcileDeployments()
	d, _ = c.GetDeployment(d.ID)
	if d.Phase != "ready" {
		t.Fatal(d.Phase)
	}
	original := c.now
	c.now = func() time.Time { return time.Now().Add(time.Minute) }
	c.ReconcileDeployments()
	if _, err := c.InferenceTarget(d.ID); err == nil {
		t.Fatal("routed to stale worker")
	}
	n, _ := c.Nodes.GetNode(d.Plan.Placements[0].NodeID)
	if n.DeploymentID == "" {
		t.Fatal("stale heartbeat released live allocation")
	}
	c.now = original
	c.StopDeployment(d.ID)
	co := d.Plan.Placements[0]
	other := d.Plan.Placements[1]
	if c.Assignments(other.NodeID)[0].Stop {
		t.Fatal("RPC killed before coordinator drained")
	}
	c.ReportWorkers(co.NodeID, []WorkerReport{{NodeID: co.NodeID, DeploymentID: d.ID, Token: d.Token, RPCState: "stopped", ModelState: "stopped"}})
	c.ReconcileDeployments()
	if !c.Assignments(other.NodeID)[0].Stop {
		t.Fatal("worker never received stop")
	}
	n, _ = c.Nodes.GetNode(co.NodeID)
	if n.DeploymentID == "" {
		t.Fatal("released group before every worker stopped")
	}
	c.ReportWorkers(other.NodeID, []WorkerReport{{NodeID: other.NodeID, DeploymentID: d.ID, Token: d.Token, RPCState: "stopped"}})
	c.ReconcileDeployments()
	d, _ = c.GetDeployment(d.ID)
	if d.Phase != "stopped" {
		t.Fatal(d.Phase)
	}
	for _, p := range d.Plan.Placements {
		n, _ := c.Nodes.GetNode(p.NodeID)
		if n.DeploymentID != "" || n.GPUs[0].AllocatedTaskID != "" {
			t.Fatal("reservation leaked")
		}
	}
}
func TestDeploymentIdentityIdempotencyAndPersistence(t *testing.T) {
	c := groupController(t)
	s := deploymentSpec()
	s.IdempotencyKey = "same"
	d, _, _ := c.CreateDeployment(s)
	replay, exists, err := c.CreateDeployment(s)
	if err != nil || !exists || d.ID != replay.ID {
		t.Fatal("idempotency failed")
	}
	s.WeightMiB++
	if _, _, err = c.CreateDeployment(s); err == nil {
		t.Fatal("reused key accepted different spec")
	}
	p := d.Plan.Placements[0]
	if c.ReportWorkers(p.NodeID, []WorkerReport{{NodeID: p.NodeID, DeploymentID: d.ID, Token: "stale", RPCState: "ready"}}) == nil {
		t.Fatal("stale token accepted")
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err = c.Save(path); err != nil {
		t.Fatal(err)
	}
	if err = c.Save(path); err != nil {
		t.Fatal("atomic replace", err)
	}
	loaded, err := LoadController(path)
	if err != nil {
		t.Fatal(err)
	}
	restored, ok := loaded.GetDeployment(d.ID)
	if !ok || restored.Token != d.Token {
		t.Fatal("deployment lost")
	}
	n, _ := loaded.Nodes.GetNode(p.NodeID)
	if n.DeploymentID != d.ID || !n.LastHeartbeat.IsZero() {
		t.Fatal("restart trusted stale inventory or lost ownership")
	}
}
