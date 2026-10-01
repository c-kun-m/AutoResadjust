package platform

import (
	"testing"
	"time"
)

func TestControllerIsIdempotentAndReleasesReservation(t *testing.T) {
	store := NewInMemoryStore()
	if err := store.UpsertNode(ResourceNode{
		ID: "node-a", Datacenter: "dc-a", Region: "cn-east", Health: NodeReady,
		LastHeartbeat:     time.Now(),
		SchedulingEnabled: true, GPUs: []GPU{{ID: "gpu-0", Index: 0, Vendor: "NVIDIA", Model: "A100", FreeMemoryMiB: 81920, TopologyGroup: "g0"}},
	}); err != nil {
		t.Fatal(err)
	}
	controller := NewController(store)
	spec := TaskSpec{TenantID: "tenant-a", Name: "job-a", Type: TaskInference, ModelID: "model-a", GPUCount: 1, MinGPUMemoryMiB: 1024, IdempotencyKey: "same-request"}
	first, _, replay, err := controller.Submit(spec)
	if err != nil || replay || first.Phase != TaskAssigned {
		t.Fatalf("unexpected first submit: task=%#v replay=%v err=%v", first, replay, err)
	}
	second, _, replay, err := controller.Submit(spec)
	if err != nil || !replay || second.Spec.ID != first.Spec.ID {
		t.Fatalf("expected idempotent replay: first=%#v second=%#v replay=%v err=%v", first, second, replay, err)
	}
	if _, err := controller.UpdateTaskPhase(first.Spec.ID, TaskRunning, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.UpdateTaskPhase(first.Spec.ID, TaskSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	node, _ := store.GetNode("node-a")
	if node.GPUs[0].AllocatedTaskID != "" {
		t.Fatalf("expected reservation release, got %#v", node.GPUs[0])
	}
}

func TestControllerReconcilesPendingTaskAfterNodeHeartbeat(t *testing.T) {
	controller := NewController(nil)
	task, _, _, err := controller.Submit(TaskSpec{TenantID: "tenant-a", Name: "queued", Type: TaskInference, GPUCount: 1, MinGPUMemoryMiB: 1024, IdempotencyKey: "queued-1"})
	if err != nil || task.Phase != TaskPending {
		t.Fatalf("expected pending task: %#v %v", task, err)
	}
	if err := controller.Nodes.UpsertNode(ResourceNode{
		ID: "node-b", Datacenter: "dc-b", Region: "cn-east", Health: NodeReady, SchedulingEnabled: true, LastHeartbeat: time.Now(),
		GPUs: []GPU{{ID: "gpu-b", Vendor: "NVIDIA", Model: "A100", FreeMemoryMiB: 81920, TopologyGroup: "single"}},
	}); err != nil {
		t.Fatal(err)
	}
	assigned := controller.ReconcilePending()
	if len(assigned) != 1 || assigned[0].Phase != TaskAssigned {
		t.Fatalf("expected pending task to be assigned: %#v", assigned)
	}
}
