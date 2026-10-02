package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/resource-adjust/compute-platform/internal/platform"
)

func TestInspectionPreservesMissingMeasurementsAndExitsUnready(t *testing.T) {
	var output bytes.Buffer
	err := writeInspection(context.Background(), &output, func(context.Context) ([]platform.GPU, error) { return nil, errors.New("driver unavailable") }, func() *platform.HostResources { return nil })
	if err == nil {
		t.Fatal("missing inventory reported success")
	}
	var report nodeInspection
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Host != nil || len(report.GPUs) != 0 || len(report.Errors) != 2 || report.MeasuredAt.IsZero() {
		t.Fatal(report)
	}
}

func TestInspectionReturnsActualProbeInventory(t *testing.T) {
	var output bytes.Buffer
	gpu := platform.GPU{ID: "test-only-uuid", MemoryMiB: 8000, FreeMemoryMiB: 7200}
	host := &platform.HostResources{}
	if err := writeInspection(context.Background(), &output, func(context.Context) ([]platform.GPU, error) { return []platform.GPU{gpu}, nil }, func() *platform.HostResources { return host }); err != nil {
		t.Fatal(err)
	}
	var report nodeInspection
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.GPUs) != 1 || report.GPUs[0].FreeMemoryMiB != 7200 || report.GPUs[0].ID != gpu.ID || report.Host == nil || len(report.Errors) != 0 {
		t.Fatal(report)
	}
}
