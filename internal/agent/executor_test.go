package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/resource-adjust/compute-platform/internal/platform"
	"github.com/resource-adjust/compute-platform/internal/telemetry"
)

func TestEngineSpecialValuesCannotBreakAgentSynchronization(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "llamacpp:tokens_predicted_total 12\nllamacpp:idle_rate NaN\nllamacpp:bad_rate +Inf\n")
	}))
	defer server.Close()
	m := telemetry.New()
	m.Register(telemetry.Service{ID: "model"})
	e := NewExecutor(ExecutorConfig{ModelBackend: strings.TrimPrefix(server.URL, "http://")}, m)
	e.collectEngine("model")
	values := m.List(false)[0].Engine
	if len(values) != 1 || values["tokens_predicted_total"] != 12 {
		t.Fatalf("non-JSON metrics propagated: %v", values)
	}
}

// The child helper speaks TCP/HTTP only. It is NOT a model or GPU simulator and
// must never be used as evidence of actual distributed inference performance.
func TestEngineHelper(t *testing.T) {
	if os.Getenv("PLATFORM_ENGINE_HELPER") != "1" {
		return
	}
	var args []string
	for i, a := range os.Args {
		if a == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	if len(args) == 0 {
		os.Exit(2)
	}
	value := func(flag string) string {
		for i, a := range args {
			if a == flag && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}
	port := value("--port")
	if args[0] == "rpc" {
		if os.Getenv("CUDA_VISIBLE_DEVICES") != "GPU-selected" {
			os.Exit(3)
		}
		l, err := net.Listen("tcp", "127.0.0.1:"+port)
		if err != nil {
			os.Exit(4)
		}
		for {
			c, err := l.Accept()
			if err != nil {
				break
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
		os.Exit(0)
	}
	if os.Getenv("CUDA_VISIBLE_DEVICES") != "-1" && os.Getenv("CUDA_VISIBLE_DEVICES") != "GPU-selected" {
		os.Exit(5)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"status":"ok"}`) })
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "llamacpp:tokens_predicted_total 12") })
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			MaxTokens int `json:"max_tokens"`
		}
		json.NewDecoder(r.Body).Decode(&input)
		if input.MaxTokens == 1 {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"choices":[{"message":{"content":"test"}}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"delta\":\"test\"}\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(20 * time.Millisecond)
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	http.ListenAndServe("127.0.0.1:"+port, mux)
	os.Exit(0)
}
func availableAddress(t *testing.T) string {
	t.Helper()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	a := l.Addr().String()
	l.Close()
	return a
}
func waitFor(t *testing.T, f func() bool) {
	t.Helper()
	end := time.Now().Add(5 * time.Second)
	for time.Now().Before(end) {
		if f() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting for process state")
}
func executorFixture(t *testing.T) (*Executor, platform.WorkAssignment, *telemetry.Registry) {
	t.Helper()
	t.Setenv("PLATFORM_ENGINE_HELPER", "1")
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "test.gguf"), []byte("test fixture, not model weights"), 0600)
	cfg := ExecutorConfig{NodeID: "a", RPCBinary: "rpc", ServerBinary: "model", ModelDir: dir, RPCListen: availableAddress(t), RPCBackend: availableAddress(t), ModelBackend: availableAddress(t), Command: func(ctx context.Context, b string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=TestEngineHelper", "--", b}, args...)...)
	}}
	m := telemetry.New()
	e := NewExecutor(cfg, m)
	t.Cleanup(e.Close)
	p := platform.Placement{NodeID: "a", GPUID: "GPU-selected", Coordinator: true, RPCAddress: cfg.RPCListen, UsableMiB: 6000}
	w := platform.WorkAssignment{DeploymentID: "dep-test", Token: "dep-test/1", Spec: platform.DeploymentSpec{ModelFile: "test.gguf", WeightMiB: 1, ContextSize: 512}, Placement: p, Peers: []platform.Placement{p, {NodeID: "b", RPCAddress: "127.0.0.1:50000", UsableMiB: 5000}}}
	return e, w, m
}
func TestExecutorProcessesStreamingAndStop(t *testing.T) {
	e, w, m := executorFixture(t)
	if err := e.Apply([]platform.WorkAssignment{w}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { e.Tick(); return e.Reports()[0].RPCState == "ready" })
	w.StartModel = true
	if err := e.Apply([]platform.WorkAssignment{w}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { e.Tick(); return e.Reports()[0].ModelState == "ready" })
	req := httptest.NewRequest("POST", "/inference/v1/chat/completions", strings.NewReader(`{"messages":[]}`))
	req.Header.Set("X-Assignment-Token", w.Token)
	req.Header.Set("X-Deployment-ID", w.DeploymentID)
	out := httptest.NewRecorder()
	e.ServeInference(out, req)
	if out.Code != 200 || !out.Flushed || !strings.Contains(out.Body.String(), "[DONE]") {
		t.Fatal(out.Code, out.Body.String())
	}
	_, id := serviceIDs("a", w.DeploymentID)
	found := false
	for _, s := range m.List(false) {
		if s.ID == id {
			found = s.Requests == 1 && s.TX > 0 && s.RX > 0 && s.Engine["tokens_predicted_total"] == 12
		}
	}
	if !found {
		t.Fatal("model metrics missing")
	}
	w.Stop = true
	e.Apply([]platform.WorkAssignment{w})
	if r := e.Reports()[0]; r.RPCState != "stopped" || r.ModelState != "stopped" {
		t.Fatal(r)
	}
	if c, err := net.DialTimeout("tcp", e.cfg.RPCBackend, 100*time.Millisecond); err == nil {
		c.Close()
		t.Fatal("RPC process still running")
	}
}
func TestExecutorLeaseExpiryAndInvalidModel(t *testing.T) {
	e, w, _ := executorFixture(t)
	w.Spec.ModelFile = "../escape.gguf"
	if _, err := ModelArgs(e.cfg, w); err == nil {
		t.Fatal("path traversal accepted")
	}
	w.Spec.ModelFile = "test.gguf"
	e.Apply([]platform.WorkAssignment{w})
	e.mu.Lock()
	e.lastSync = time.Now().Add(-time.Minute)
	e.mu.Unlock()
	e.Tick()
	if r := e.Reports()[0]; r.RPCState != "failed" || !strings.Contains(r.Message, "lease expired") {
		t.Fatal(r)
	}
	e.Apply([]platform.WorkAssignment{w})
	if e.Reports()[0].RPCState != "failed" {
		t.Fatal("expired token relaunched process")
	}
}
