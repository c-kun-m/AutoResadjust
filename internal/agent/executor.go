package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/resource-adjust/compute-platform/internal/platform"
	"github.com/resource-adjust/compute-platform/internal/telemetry"
)

type ExecutorConfig struct {
	Command                                   func(context.Context, string, ...string) *exec.Cmd
	NodeID, RPCBinary, ServerBinary, ModelDir string
	RPCListen, RPCBackend, ModelBackend       string
	LeaseTimeout                              time.Duration
}

type logTail struct {
	mu   sync.Mutex
	text string
}

func (l *logTail) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.text += string(p)
	if len(l.text) > 32768 {
		l.text = l.text[len(l.text)-32768:]
	}
	return len(p), nil
}
func (l *logTail) Text() string { l.mu.Lock(); defer l.mu.Unlock(); return l.text }

type child struct {
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	done    chan struct{}
	mu      sync.Mutex
	err     error
	started time.Time
	logs    *logTail
}

func (p *child) exited() (bool, error) {
	select {
	case <-p.done:
		p.mu.Lock()
		defer p.mu.Unlock()
		return true, p.err
	default:
		return false, nil
	}
}
func (p *child) stop() {
	if p == nil {
		return
	}
	p.cancel()
	<-p.done
}

type execution struct {
	work           platform.WorkAssignment
	report         platform.WorkerReport
	rpc, model     *child
	proxyStop      chan struct{}
	rpcID, modelID string
	draining       bool
	finished       bool
}

type Executor struct {
	mu       sync.Mutex
	cfg      ExecutorConfig
	metrics  *telemetry.Registry
	current  *execution
	lastSync time.Time
	client   *http.Client
}

func NewExecutor(cfg ExecutorConfig, m *telemetry.Registry) *Executor {
	if cfg.LeaseTimeout == 0 {
		cfg.LeaseTimeout = 45 * time.Second
	}
	return &Executor{cfg: cfg, metrics: m, client: &http.Client{Timeout: 2 * time.Second}}
}

func (e *Executor) Enabled() bool { return e.cfg.RPCBinary != "" && e.cfg.ServerBinary != "" }

func (e *Executor) launch(binary string, args, env []string) (*child, error) {
	ctx, cancel := context.WithCancel(context.Background())
	command := e.cfg.Command
	if command == nil {
		command = exec.CommandContext
	}
	cmd := command(ctx, binary, args...)
	cmd.Env = append(os.Environ(), env...)
	configureChild(cmd)
	p := &child{cmd: cmd, cancel: cancel, done: make(chan struct{}), started: time.Now(), logs: &logTail{}}
	cmd.Stdout, cmd.Stderr = p.logs, p.logs
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	go func() { err := cmd.Wait(); p.mu.Lock(); p.err = err; p.mu.Unlock(); close(p.done) }()
	return p, nil
}

func serviceIDs(node, dep string) (string, string) {
	return node + "/rpc/" + dep, node + "/model/" + dep
}

// Apply is called only after a successful, authenticated synchronization.
// A lost HTTP response is never interpreted as an empty desired assignment.
func (e *Executor) Apply(work []platform.WorkAssignment) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastSync = time.Now()
	if len(work) > 1 {
		return fmt.Errorf("only one distributed deployment per node is supported")
	}
	if len(work) == 0 {
		if e.current != nil && !e.current.finished {
			e.stopLocked("assignment removed")
		}
		return nil
	}
	w := work[0]
	if w.Placement.NodeID != e.cfg.NodeID || w.Token == "" {
		return fmt.Errorf("invalid assignment identity")
	}
	if e.current != nil && e.current.work.Token != w.Token {
		if !e.current.finished {
			e.stopLocked("assignment replaced")
		}
		e.metrics.Forget(e.current.rpcID)
		e.metrics.Forget(e.current.modelID)
		e.current = nil
	}
	if e.current == nil {
		rpcID, modelID := serviceIDs(e.cfg.NodeID, w.DeploymentID)
		e.current = &execution{work: w, rpcID: rpcID, modelID: modelID, report: platform.WorkerReport{DeploymentID: w.DeploymentID, NodeID: e.cfg.NodeID, Token: w.Token, RPCState: "starting"}}
		e.metrics.Register(telemetry.Service{ID: rpcID, Name: "GPU RPC worker", Kind: "rpc-worker", NodeID: e.cfg.NodeID, DeploymentID: w.DeploymentID, Address: w.Placement.RPCAddress, Traffic: true})
		if w.Placement.Coordinator {
			e.metrics.Register(telemetry.Service{ID: modelID, Name: "Model inference server", Kind: "model-server", NodeID: e.cfg.NodeID, DeploymentID: w.DeploymentID, Address: w.Placement.AgentURL + "/inference", HTTP: true, Traffic: true})
		}
	}
	x := e.current
	x.work = w
	if w.Stop {
		e.stopLocked("deployment stopped")
		return nil
	}
	if x.finished {
		return nil
	} // Failed/expired attempts require a new assignment token.
	if x.rpc == nil {
		if !e.Enabled() {
			e.failLocked("executor binaries are not configured")
			return nil
		}
		_, port, err := net.SplitHostPort(e.cfg.RPCBackend)
		if err != nil {
			e.failLocked(err.Error())
			return nil
		}
		// UUID isolation makes the selected GPU the only CUDA device in this process.
		x.rpc, err = e.launch(e.cfg.RPCBinary, []string{"--host", "127.0.0.1", "--port", port, "--device", "CUDA0"}, []string{"CUDA_VISIBLE_DEVICES=" + w.Placement.GPUID, "GGML_RPC_NO_RDMA=1"})
		if err != nil {
			e.failLocked(err.Error())
			return nil
		}
		listener, err := net.Listen("tcp", e.cfg.RPCListen)
		if err != nil {
			e.failLocked(err.Error())
			return nil
		}
		x.proxyStop = make(chan struct{})
		go e.metrics.ServeTCP(listener, e.cfg.RPCBackend, x.rpcID, x.proxyStop)
	}
	if w.StartModel && x.model == nil && x.report.RPCState == "ready" {
		args, err := ModelArgs(e.cfg, w)
		if err != nil {
			e.failLocked(err.Error())
			return nil
		}
		x.model, err = e.launch(e.cfg.ServerBinary, args, []string{"CUDA_VISIBLE_DEVICES=-1", "GGML_RPC_NO_RDMA=1"})
		if err != nil {
			e.failLocked(err.Error())
			return nil
		}
		x.report.ModelState = "starting"
	}
	return nil
}

func ModelArgs(cfg ExecutorConfig, w platform.WorkAssignment) ([]string, error) {
	if w.Spec.ModelFile == "" || strings.ContainsAny(w.Spec.ModelFile, "/\\:\x00") {
		return nil, fmt.Errorf("model must be a filename in model-dir")
	}
	root, err := filepath.EvalSymlinks(cfg.ModelDir)
	if err != nil {
		return nil, fmt.Errorf("model-dir: %w", err)
	}
	file, err := filepath.EvalSymlinks(filepath.Join(root, w.Spec.ModelFile))
	if err != nil {
		return nil, fmt.Errorf("model file: %w", err)
	}
	rel, err := filepath.Rel(root, file)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("model escapes model-dir")
	}
	info, err := os.Stat(file)
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("model is not a regular file")
	}
	if info.Size() > w.Spec.WeightMiB*1024*1024 {
		return nil, fmt.Errorf("GGUF file exceeds declared weight_mib; replan with actual size")
	}
	var addresses, ratios, devices []string
	for i, p := range w.Peers {
		addresses = append(addresses, p.RPCAddress)
		ratios = append(ratios, strconv.FormatInt(p.UsableMiB, 10))
		devices = append(devices, fmt.Sprintf("RPC%d", i))
	}
	_, port, err := net.SplitHostPort(cfg.ModelBackend)
	if err != nil {
		return nil, err
	}
	return []string{"--model", file, "--alias", w.DeploymentID, "--host", "127.0.0.1", "--port", port, "--rpc", strings.Join(addresses, ","), "--device", strings.Join(devices, ","), "--tensor-split", strings.Join(ratios, ","), "--split-mode", "layer", "--n-gpu-layers", "999", "--ctx-size", strconv.Itoa(w.Spec.ContextSize), "--fit", "off", "--parallel", "1", "--metrics"}, nil
}

func (e *Executor) failLocked(message string) {
	x := e.current
	if x == nil {
		return
	}
	x.report.Message = message
	if x.model != nil {
		x.model.stop()
	}
	if x.proxyStop != nil {
		close(x.proxyStop)
		x.proxyStop = nil
	}
	if x.rpc != nil {
		x.rpc.stop()
	}
	x.report.RPCState = "failed"
	if x.work.Placement.Coordinator {
		x.report.ModelState = "failed"
	}
	x.finished = true
	e.metrics.State(x.rpcID, "failed", message)
	if x.work.Placement.Coordinator {
		e.metrics.State(x.modelID, "failed", message)
	}
}

func (e *Executor) stopLocked(message string) {
	x := e.current
	if x == nil {
		return
	}
	x.draining = true
	// The HTTP gateway prevents new requests as soon as draining starts. Give
	// active streams a bounded opportunity to finish before terminating processes.
	if x.model != nil && !x.finished {
		e.metrics.State(x.modelID, "draining", message)
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			active := int64(0)
			for _, s := range e.metrics.List(false) {
				if s.ID == x.modelID {
					active = s.Active
				}
			}
			if active == 0 {
				break
			}
			e.mu.Unlock()
			time.Sleep(100 * time.Millisecond)
			e.mu.Lock()
		}
		x.model.stop()
	}
	if x.proxyStop != nil {
		close(x.proxyStop)
		x.proxyStop = nil
	}
	if x.rpc != nil {
		x.rpc.stop()
	}
	x.report.RPCState = "stopped"
	if x.work.Placement.Coordinator {
		x.report.ModelState = "stopped"
	}
	x.report.Message = message
	x.finished = true
	e.metrics.State(x.rpcID, "stopped", message)
	if x.work.Placement.Coordinator {
		e.metrics.State(x.modelID, "stopped", message)
	}
}

func (e *Executor) Tick() {
	e.mu.Lock()
	defer e.mu.Unlock()
	x := e.current
	if x == nil || x.finished || x.draining {
		return
	}
	if time.Since(e.lastSync) > e.cfg.LeaseTimeout {
		e.failLocked("control-plane lease expired; local processes terminated")
		return
	}
	if x.rpc != nil {
		if exited, err := x.rpc.exited(); exited {
			e.failLocked(fmt.Sprintf("RPC process exited: %v; %s", err, x.rpc.logs.Text()))
			return
		}
		if x.report.RPCState != "ready" {
			conn, err := net.DialTimeout("tcp", e.cfg.RPCBackend, 300*time.Millisecond)
			if err == nil {
				conn.Close()
				x.report.RPCState = "ready"
				e.metrics.State(x.rpcID, "ready", "TCP listener ready; model warmup verifies end-to-end RPC")
			} else if time.Since(x.rpc.started) > 2*time.Minute {
				e.failLocked("RPC startup timed out")
				return
			}
		}
	}
	if x.model != nil {
		if exited, err := x.model.exited(); exited {
			e.failLocked(fmt.Sprintf("model process exited: %v; %s", err, x.model.logs.Text()))
			return
		}
		resp, err := e.client.Get("http://" + e.cfg.ModelBackend + "/health")
		ready := err == nil && resp.StatusCode == 200
		if resp != nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
		}
		if ready {
			x.report.ModelState = "ready"
			e.metrics.State(x.modelID, "ready", "model loaded and engine health ready")
			e.collectEngine(x.modelID)
		} else {
			x.report.ModelState = "starting"
			e.metrics.State(x.modelID, "loading", "waiting for model health check")
			if time.Since(x.model.started) > 15*time.Minute {
				e.failLocked("model load timeout")
				return
			}
		}
	}
}

func (e *Executor) collectEngine(id string) {
	resp, err := e.client.Get("http://" + e.cfg.ModelBackend + "/metrics")
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return
	}
	values := map[string]float64{}
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 128*1024))
	for sc.Scan() {
		parts := strings.Fields(sc.Text())
		if len(parts) != 2 || !strings.HasPrefix(parts[0], "llamacpp:") || strings.Contains(parts[0], "{") {
			continue
		}
		if v, err := strconv.ParseFloat(parts[1], 64); err == nil && !math.IsNaN(v) && !math.IsInf(v, 0) {
			values[strings.TrimPrefix(parts[0], "llamacpp:")] = v
		}
	}
	e.metrics.Update(id, func(s *telemetry.Service) { s.Engine = values })
}

func (e *Executor) Reports() []platform.WorkerReport {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.current == nil {
		return []platform.WorkerReport{}
	}
	return []platform.WorkerReport{e.current.report}
}
func (e *Executor) Close() { e.mu.Lock(); defer e.mu.Unlock(); e.stopLocked("agent shutting down") }
func (e *Executor) Logs() map[string]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := map[string]string{}
	if x := e.current; x != nil {
		if x.rpc != nil {
			out[x.rpcID] = x.rpc.logs.Text()
		}
		if x.model != nil {
			out[x.modelID] = x.model.logs.Text()
		}
	}
	return out
}

func (e *Executor) ServeInference(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	x := e.current
	id := ""
	token := ""
	dep := ""
	if x != nil {
		id = x.modelID
		token = x.work.Token
		dep = x.work.DeploymentID
	}
	ready := x != nil && !x.finished && !x.draining && x.report.ModelState == "ready"
	e.mu.Unlock()
	if !ready || r.Header.Get("X-Assignment-Token") != token || r.Header.Get("X-Deployment-ID") != dep {
		http.Error(w, "model is not ready or assignment is stale", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
		http.NotFound(w, r)
		return
	}
	u, _ := url.Parse("http://" + e.cfg.ModelBackend)
	proxy := httputil.NewSingleHostReverseProxy(u)
	original := proxy.Director
	proxy.Director = func(q *http.Request) {
		original(q)
		q.URL.Path = "/v1/chat/completions"
		q.Header.Del("Authorization")
		q.Header.Del("X-Assignment-Token")
		q.Header.Del("X-Deployment-ID")
	}
	proxy.FlushInterval = -1
	e.metrics.HTTP(id, proxy).ServeHTTP(w, r)
}
