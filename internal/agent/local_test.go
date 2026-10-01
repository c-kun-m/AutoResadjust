package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/resource-adjust/compute-platform/internal/platform"
	"github.com/resource-adjust/compute-platform/internal/telemetry"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalExecutorUsesSelectedGPUWithoutRPC(t *testing.T) {
	e, w, m := executorFixture(t)
	e.cfg.RPCBinary = ""
	w.Spec.Backend = platform.BackendLocal
	w.Spec.Layers = 40
	w.Spec.GPULayers = 10
	w.Spec.RAMMiB = 8192
	w.StartModel = true
	if err := e.Apply([]platform.WorkAssignment{w}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { e.Tick(); return e.Reports()[0].ModelState == "ready" })
	if e.current.rpc != nil || e.current.proxyStop != nil {
		t.Fatal("local execution started RPC")
	}
	for _, s := range m.List(false) {
		if s.Kind == "rpc-worker" {
			t.Fatal("invented local RPC traffic")
		}
	}
	w.Stop = true
	e.Apply([]platform.WorkAssignment{w})
	if e.Reports()[0].ModelState != "stopped" {
		t.Fatal("local process did not stop")
	}
}
func TestModelChecksumAndLocalArguments(t *testing.T) {
	e, w, _ := executorFixture(t)
	w.Spec.Backend = platform.BackendLocal
	w.Spec.Layers = 40
	w.Spec.GPULayers = 10
	w.Spec.RAMMiB = 8192
	data, _ := os.ReadFile(filepath.Join(e.cfg.ModelDir, w.Spec.ModelFile))
	hash := sha256.Sum256(data)
	w.Spec.ModelSHA256 = hex.EncodeToString(hash[:])
	args, err := ModelArgs(e.cfg, w)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--n-gpu-layers 10") || !strings.Contains(joined, "--load-mode none") || !strings.Contains(joined, "--lazy-mode off") || strings.Contains(joined, "--rpc") {
		t.Fatal(joined)
	}
	w.Spec.ModelSHA256 = strings.Repeat("0", 64)
	if _, err = ModelArgs(e.cfg, w); err == nil {
		t.Fatal("checksum mismatch accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = modelArgs(ctx, e.cfg, w); err == nil {
		t.Fatal("cancelled preparation continued")
	}
}
func TestRuntimeHealthFailureIsNotLoadTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "busy", 503) }))
	defer server.Close()
	m := telemetry.New()
	m.Register(telemetry.Service{ID: "model"})
	e := NewExecutor(ExecutorConfig{ModelBackend: strings.TrimPrefix(server.URL, "http://")}, m)
	e.lastSync = time.Now()
	e.current = &execution{wasReady: true, modelID: "model", model: &child{started: time.Now().Add(-time.Hour), done: make(chan struct{})}, report: platform.WorkerReport{ModelState: "ready"}}
	e.Tick()
	if e.current.finished || e.current.report.ModelState != "degraded" {
		t.Fatalf("transient failure killed warm model: %+v", e.current.report)
	}
}
func TestWarmupMustProduceCompletion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"status":"ok"}`) }))
	defer server.Close()
	m := telemetry.New()
	m.Register(telemetry.Service{ID: "model"})
	e := NewExecutor(ExecutorConfig{ModelBackend: strings.TrimPrefix(server.URL, "http://")}, m)
	x := &execution{modelID: "model"}
	e.current = x
	e.mu.Lock()
	e.warmModelLocked(x)
	e.mu.Unlock()
	waitFor(t, func() bool { e.mu.Lock(); defer e.mu.Unlock(); return x.finished })
	if x.report.RPCState != "failed" {
		t.Fatal("health-only response counted as inference")
	}
}
func TestHostProbeReportsRealCapacity(t *testing.T) {
	h := (&HostProbe{}).Probe()
	if h == nil {
		t.Skip("host metrics unsupported")
	}
	if h.MemoryTotalMiB <= 0 || h.MemoryAvailableMiB < 0 || h.MemoryAvailableMiB > h.MemoryTotalMiB || h.CPUCount < 1 {
		t.Fatal(h)
	}
}
