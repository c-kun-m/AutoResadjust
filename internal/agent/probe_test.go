package agent

import "testing"

func TestParseNvidiaSMI(t *testing.T) {
	gpus, err := ParseNvidiaSMI("0, NVIDIA A100, GPU-uuid-0, 81920, 1024, 12\n1, NVIDIA A100, GPU-uuid-1, 81920, 2048, 15\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(gpus) != 2 || gpus[0].ID != "GPU-uuid-0" || gpus[0].Index != 0 || gpus[0].FreeMemoryMiB != 80896 {
		t.Fatalf("unexpected inventory: %#v", gpus)
	}
	if gpus[0].UtilizationPct == nil || *gpus[0].UtilizationPct != 12 {
		t.Fatal("GPU utilization was discarded")
	}
	if gpus[0].TopologyGroup == "default" || gpus[0].TopologyGroup != "" {
		t.Fatalf("unknown multi-GPU topology must remain unset: %#v", gpus)
	}
}

func TestParseNvidiaSMIRejectsMalformedLine(t *testing.T) {
	if _, err := ParseNvidiaSMI("0, NVIDIA A100"); err == nil {
		t.Fatal("expected malformed csv error")
	}
}

func TestParseNvidiaSMIToleratesUnavailableUtilization(t *testing.T) {
	gpus, err := ParseNvidiaSMI("0, \"NVIDIA, A100\", GPU-uuid-0, 81920, 0, N/A\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(gpus) != 1 || gpus[0].TopologyGroup != "single" {
		t.Fatalf("unexpected singleton inventory: %#v", gpus)
	}
	if gpus[0].UtilizationPct != nil {
		t.Fatal("unavailable utilization must stay unknown")
	}
	if gpus[0].ID != "GPU-uuid-0" || gpus[0].Model != "NVIDIA, A100" {
		t.Fatalf("uuid/model were not preserved: %#v", gpus[0])
	}
}

func TestParseNvidiaSMIExplicitTopologyGroup(t *testing.T) {
	gpus, err := ParseNvidiaSMIWithTopology("0, A, GPU-a, 100, 1, 0\n1, A, GPU-b, 100, 1, 0\n", "nvlink-a")
	if err != nil {
		t.Fatal(err)
	}
	for _, gpu := range gpus {
		if gpu.TopologyGroup != "nvlink-a" {
			t.Fatalf("explicit topology group was not applied: %#v", gpus)
		}
	}
}

func TestParseNvidiaSMIRejectsUnstableOrDuplicateUUID(t *testing.T) {
	for name, output := range map[string]string{
		"missing uuid":       "0, A, N/A, 100, 1, 0\n",
		"duplicate uuid":     "0, A, GPU-a, 100, 1, 0\n1, A, GPU-a, 100, 1, 0\n",
		"used exceeds total": "0, A, GPU-a, 100, 101, 0\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseNvidiaSMI(output); err == nil {
				t.Fatal("expected parse error")
			}
		})
	}
}

func TestParseNvidiaSMIRejectsEmptyInventory(t *testing.T) {
	if _, err := ParseNvidiaSMI("\n"); err == nil {
		t.Fatal("expected empty inventory error")
	}
}
