package platform

import (
	"testing"
	"time"
)

func testNode(id, dc string, health NodeHealth, queue int, modelCached bool, topology string) ResourceNode {
	return ResourceNode{
		ID: id, Datacenter: dc, Region: "cn-east", Health: health,
		SchedulingEnabled: true, QueueDepth: queue, CachedModels: map[string]bool{"model-a": modelCached},
		CostPerGPUHour: 1, LastHeartbeat: time.Now(),
		GPUs: []GPU{{ID: id + "-0", Index: 0, Vendor: "NVIDIA", Model: "A100", MemoryMiB: 81920, FreeMemoryMiB: 81920, TopologyGroup: topology},
			{ID: id + "-1", Index: 1, Vendor: "NVIDIA", Model: "A100", MemoryMiB: 81920, FreeMemoryMiB: 81920, TopologyGroup: topology}},
	}
}

func TestSchedulerFiltersIncompatibleNodesAndScoresCacheHit(t *testing.T) {
	store := NewInMemoryStore()
	for _, node := range []ResourceNode{
		testNode("node-a", "dc-a", NodeReady, 5, false, "nvlink-0"),
		testNode("node-b", "dc-b", NodeReady, 0, true, "nvlink-0"),
		testNode("node-offline", "dc-c", NodeOffline, 0, true, "nvlink-0"),
	} {
		if err := store.UpsertNode(node); err != nil {
			t.Fatal(err)
		}
	}
	s := NewScheduler(store, DefaultSchedulerWeights())
	decision, err := s.Schedule(TaskSpec{ID: "task-1", ModelID: "model-a", GPUCount: 2, MinGPUMemoryMiB: 40000, RequiredVendor: "nvidia"})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Selected == nil || decision.Selected.NodeID != "node-b" {
		t.Fatalf("expected cache-hit node-b, got %#v", decision.Selected)
	}
	if len(decision.RejectionReasons["node-offline"]) == 0 {
		t.Fatal("expected offline node to have rejection reasons")
	}
}

func TestSchedulerRequiresOneTopologyGroup(t *testing.T) {
	store := NewInMemoryStore()
	node := testNode("node-a", "dc-a", NodeReady, 0, false, "")
	node.GPUs[0].TopologyGroup = "group-a"
	node.GPUs[1].TopologyGroup = "group-b"
	if err := store.UpsertNode(node); err != nil {
		t.Fatal(err)
	}
	_, err := NewScheduler(store, DefaultSchedulerWeights()).Schedule(TaskSpec{ID: "task-2", GPUCount: 2, MinGPUMemoryMiB: 1000})
	if err == nil {
		t.Fatal("expected topology constraint to reject split allocation")
	}
}

func TestSchedulerReportsCapacityRejection(t *testing.T) {
	store := NewInMemoryStore()
	node := testNode("node-a", "dc-a", NodeReady, 0, false, "group-a")
	node.GPUs[0].FreeMemoryMiB = 512
	if err := store.UpsertNode(node); err != nil {
		t.Fatal(err)
	}
	decision, err := NewScheduler(store, DefaultSchedulerWeights()).Schedule(TaskSpec{ID: "task-3", GPUCount: 2, MinGPUMemoryMiB: 1024})
	if err == nil {
		t.Fatal("expected no candidate error")
	}
	if len(decision.RejectionReasons["node-a"]) == 0 {
		t.Fatal("expected capacity rejection reason")
	}
}

func TestInMemoryStoreRejectsDuplicateTaskAndCopiesState(t *testing.T) {
	store := NewInMemoryStore()
	node := testNode("node-a", "dc-a", NodeReady, 0, false, "group-a")
	if err := store.UpsertNode(node); err != nil {
		t.Fatal(err)
	}
	node.Labels = map[string]string{"mutated": "after"}
	stored, ok := store.GetNode("node-a")
	if !ok || stored.Labels["mutated"] != "" {
		t.Fatal("store should isolate node state")
	}
	task := Task{Spec: TaskSpec{ID: "task-1"}, Phase: TaskPending}
	if err := store.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateTask(task); err == nil {
		t.Fatal("expected duplicate task rejection")
	}
}

func TestInMemoryStoreReservesGPUsAtomically(t *testing.T) {
	store := NewInMemoryStore()
	if err := store.UpsertNode(testNode("node-a", "dc-a", NodeReady, 0, false, "group-a")); err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveGPUs("node-a", "task-a", []string{"node-a-0"}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveGPUs("node-a", "task-a", []string{"node-a-0"}); err != nil {
		t.Fatalf("same task reservation should be idempotent: %v", err)
	}
	if err := store.ReserveGPUs("node-a", "task-b", []string{"node-a-0"}); err == nil {
		t.Fatal("expected a second task to fail reserving the same gpu")
	}
	if err := store.ReleaseGPUs("node-a", "task-a", []string{"node-a-0"}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveGPUs("node-a", "task-b", []string{"node-a-0"}); err != nil {
		t.Fatal(err)
	}
}
