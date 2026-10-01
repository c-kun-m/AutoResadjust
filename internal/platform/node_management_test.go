package platform

import (
	"errors"
	"testing"
	"time"
)

func managedTestNode() ResourceNode {
	return ResourceNode{
		ID: "node-managed", Name: "Managed node", Datacenter: "dc-a", Region: "cn-east",
		Runtime: "wsl2-docker", Health: NodeReady, SchedulingEnabled: true,
		LastHeartbeat: time.Now().UTC(),
		GPUs:          []GPU{{ID: "gpu-managed", Index: 0, Vendor: "NVIDIA", Model: "A100", MemoryMiB: 81920, FreeMemoryMiB: 81920, TopologyGroup: "single"}},
	}
}

func TestRegisterNodeRejectsDuplicateAndUpdatePreservesObservations(t *testing.T) {
	c := NewController(nil)
	node, err := c.RegisterNode(managedTestNode())
	if err != nil {
		t.Fatal(err)
	}
	if node.ID != "node-managed" {
		t.Fatalf("unexpected registered node: %#v", node)
	}
	if _, err := c.RegisterNode(managedTestNode()); !errors.Is(err, ErrNodeExists) {
		t.Fatalf("expected duplicate node error, got %v", err)
	}

	updated, err := c.UpdateNode(node.ID, ResourceNode{Name: "Renamed", Datacenter: node.Datacenter, Region: node.Region, Runtime: node.Runtime, SchedulingEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "Renamed" || len(updated.GPUs) != 1 || updated.GPUs[0].ID != "gpu-managed" {
		t.Fatalf("update should retain observed GPU inventory: %#v", updated)
	}
	degraded := managedTestNode()
	degraded.ID = "node-degraded"
	degraded.Health = NodeDegraded
	degraded.SchedulingEnabled = false
	registered, err := c.RegisterNode(degraded)
	if err != nil {
		t.Fatal(err)
	}
	if registered.Health != NodeDegraded || registered.SchedulingEnabled {
		t.Fatalf("register must retain an explicitly disabled/degraded node: %#v", registered)
	}
}

func TestDrainIsNotUndoneByHeartbeatAndEnableRestoresScheduling(t *testing.T) {
	c := NewController(nil)
	if _, err := c.RegisterNode(managedTestNode()); err != nil {
		t.Fatal(err)
	}
	drained, err := c.DrainNode("node-managed")
	if err != nil || drained.Health != NodeDraining || drained.SchedulingEnabled {
		t.Fatalf("unexpected drain result: %#v err=%v", drained, err)
	}

	heartbeat := managedTestNode()
	heartbeat.GPUs[0].FreeMemoryMiB = 70000
	heartbeat.GPUs[0].Model = "A10"
	heartbeat.Health = NodeReady
	heartbeat.SchedulingEnabled = true
	stored, _, err := c.ApplyHeartbeat(heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Health != NodeDraining || stored.SchedulingEnabled {
		t.Fatalf("heartbeat must preserve drain state: %#v", stored)
	}
	if stored.GPUs[0].Model != "A10" || stored.GPUs[0].FreeMemoryMiB != 70000 {
		t.Fatalf("heartbeat should refresh observations: %#v", stored.GPUs[0])
	}

	enabled, err := c.EnableNode("node-managed")
	if err != nil || enabled.Health != NodeReady || !enabled.SchedulingEnabled {
		t.Fatalf("unexpected enable result: %#v err=%v", enabled, err)
	}
}

func TestBusyNodeCannotBeEditedOrDeleted(t *testing.T) {
	c := NewController(nil)
	if _, err := c.RegisterNode(managedTestNode()); err != nil {
		t.Fatal(err)
	}
	task, _, _, err := c.Submit(TaskSpec{TenantID: "tenant-a", Type: TaskInference, GPUCount: 1, MinGPUMemoryMiB: 1024, ModelID: "model-a"})
	if err != nil || task.Phase != TaskAssigned {
		t.Fatalf("expected task assignment: %#v err=%v", task, err)
	}
	if _, err := c.UpdateNode("node-managed", ResourceNode{Name: "unsafe-edit", Datacenter: "dc-a", Region: "cn-east", Runtime: "wsl2-docker", SchedulingEnabled: true}); !errors.Is(err, ErrNodeBusy) {
		t.Fatalf("expected update to reject busy node, got %v", err)
	}
	if err := c.DeleteNode("node-managed"); !errors.Is(err, ErrNodeBusy) {
		t.Fatalf("expected delete to reject busy node, got %v", err)
	}
	if _, err := c.UpdateTaskPhase(task.Spec.ID, TaskRunning, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpdateTaskPhase(task.Spec.ID, TaskSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteNode("node-managed"); err != nil {
		t.Fatalf("idle node should be deletable: %v", err)
	}
}

func TestDeletedNodeHeartbeatRequiresExplicitReregistration(t *testing.T) {
	c := NewController(nil)
	node := managedTestNode()
	if _, err := c.RegisterNode(node); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteNode(node.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.ApplyHeartbeat(node); err == nil {
		t.Fatal("heartbeat from a decommissioned node must not silently recreate it")
	}
	registered, err := c.RegisterNode(node)
	if err != nil || registered.ID != node.ID {
		t.Fatalf("explicit re-registration should re-admit node: %#v %v", registered, err)
	}
}
