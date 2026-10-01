package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/resource-adjust/compute-platform/internal/agent"
	"github.com/resource-adjust/compute-platform/internal/platform"
	"github.com/resource-adjust/compute-platform/internal/telemetry"
)

func runAgent() {
	nodeID := flag.String("node-id", "", "stable node ID")
	nodeName := flag.String("node-name", "", "display name")
	site := flag.String("site", "default", "datacenter")
	runtimeName := flag.String("runtime", "wsl2-docker", "reported runtime label; use windows-native for a host executable")
	control := flag.String("control-plane", "http://127.0.0.1:8080", "control plane URL")
	interval := flag.Duration("interval", 5*time.Second, "sync interval, less than 20s")
	allowEmpty := flag.Bool("allow-empty", false, "allow missing GPU driver")
	topology := flag.String("topology-group", "", "intra-node topology group")
	listen := flag.String("listen", "127.0.0.1:9090", "agent HTTP endpoint")
	advertise := flag.String("advertise-host", "127.0.0.1", "private host/IP reachable from other nodes")
	network := flag.String("network-group", "lan-default", "trusted private network group")
	rpcListen := flag.String("rpc-listen", "127.0.0.1:50052", "RPC traffic proxy; PRIVATE network only")
	rpcBackend := flag.String("rpc-backend", "127.0.0.1:50053", "local RPC process address")
	modelBackend := flag.String("model-backend", "127.0.0.1:18081", "local model process address")
	rpcBinary := flag.String("rpc-binary", "", "ggml-rpc-server executable")
	serverBinary := flag.String("server-binary", "", "llama-server executable")
	modelDir := flag.String("model-dir", "models", "GGUF model directory on coordinator")
	engineVersion := flag.String("engine-version", "", "identical pinned llama.cpp revision on all nodes")
	flag.Parse()
	if *nodeID == "" || strings.ContainsAny(*nodeID, "/\\") {
		log.Fatal("valid --node-id required")
	}
	if *nodeName == "" {
		*nodeName = *nodeID
	}
	if *interval <= 0 || *interval >= 20*time.Second {
		log.Fatal("interval must be greater than zero and below 20s")
	}
	if err := validateControlPlaneURL(*control); err != nil {
		log.Fatal(err)
	}
	_, httpPort, err := net.SplitHostPort(*listen)
	if err != nil {
		log.Fatal(err)
	}
	_, rpcPort, err := net.SplitHostPort(*rpcListen)
	if err != nil {
		log.Fatal(err)
	}
	for _, addr := range []string{*rpcBackend, *modelBackend} {
		h, _, err := net.SplitHostPort(addr)
		if err != nil || h != "127.0.0.1" {
			log.Fatal("backend processes must bind to 127.0.0.1")
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	m := telemetry.New()
	agentID := *nodeID + "/agent"
	linkID := *nodeID + "/control-link"
	m.Register(telemetry.Service{ID: agentID, Name: "Edge agent API", Kind: "agent", NodeID: *nodeID, Address: "http://" + net.JoinHostPort(*advertise, httpPort), Status: "ready", HTTP: true, Traffic: true})
	m.Register(telemetry.Service{ID: linkID, Name: "Control-plane synchronization", Kind: "agent-sync", NodeID: *nodeID, Address: *control, HTTP: true, Traffic: true})
	ex := agent.NewExecutor(agent.ExecutorConfig{NodeID: *nodeID, RPCBinary: *rpcBinary, ServerBinary: *serverBinary, ModelDir: *modelDir, RPCListen: *rpcListen, RPCBackend: *rpcBackend, ModelBackend: *modelBackend}, m)
	if ex.Enabled() && *engineVersion == "" {
		log.Fatal("--engine-version must identify the pinned binaries")
	}
	defer ex.Close()
	token := strings.TrimSpace(os.Getenv("CONTROL_PLANE_TOKEN"))
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Write([]byte("ok"))
			return
		}
		if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "bearer token required", 401)
			return
		}
		if r.URL.Path == "/metrics" {
			m.Prometheus(w, r)
			return
		}
		if r.URL.Path == "/logs" && r.Method == "GET" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(ex.Logs())
			return
		}
		if strings.HasPrefix(r.URL.Path, "/inference/") {
			r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
			ex.ServeInference(w, r)
			return
		}
		http.NotFound(w, r)
	})
	httpServer := &http.Server{Addr: *listen, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" || r.URL.Path == "/healthz" || strings.HasPrefix(r.URL.Path, "/inference/") {
			handler.ServeHTTP(w, r)
		} else {
			m.HTTP(agentID, handler).ServeHTTP(w, r)
		}
	})}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	go func() {
		if err := httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Print(err)
			stop()
		}
	}()
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				ex.Tick()
			}
		}
	}()
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	hostProbe := &agent.HostProbe{}
	syncOnce := func() {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		gpus, probeErr := agent.Probe(probeCtx, *allowEmpty, *topology)
		cancel()
		node := platform.ResourceNode{ID: *nodeID, Name: *nodeName, Datacenter: *site, Region: *site, Runtime: *runtimeName, Health: platform.NodeDegraded, GPUs: gpus, LastHeartbeat: time.Now().UTC()}
		node.Host = hostProbe.Probe()
		if probeErr == nil && len(gpus) > 0 {
			node.Health = platform.NodeReady
			node.SchedulingEnabled = true
		}
		if ex.Enabled() {
			node.Agent = &platform.AgentEndpoint{URL: "http://" + net.JoinHostPort(*advertise, httpPort), RPCAddress: net.JoinHostPort(*advertise, rpcPort), EngineVersion: *engineVersion, NetworkGroup: *network}
			node.Agent.Backends = ex.Capabilities()
		}
		payload := map[string]any{"node": node, "reports": ex.Reports(), "services": m.List(false)}
		body, err := json.Marshal(payload)
		if err != nil {
			log.Print(err)
			return
		}
		req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(*control, "/")+"/api/v1/agents/"+*nodeID+"/sync", bytes.NewReader(body))
		if err != nil {
			log.Print(err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		start := time.Now()
		resp, err := client.Do(req)
		status := 503
		var data []byte
		if resp != nil {
			status = resp.StatusCode
			data, err = io.ReadAll(io.LimitReader(resp.Body, 2<<20))
			resp.Body.Close()
		}
		m.Record(linkID, status, len(data), len(body), time.Since(start))
		if err != nil || status != 200 {
			m.State(linkID, "degraded", fmt.Sprintf("sync failed: HTTP %d %v", status, err))
			log.Printf("sync failed: HTTP %d %v %s", status, err, string(data))
			return
		}
		var desired struct {
			Assignments []platform.WorkAssignment `json:"assignments"`
		}
		if err = json.Unmarshal(data, &desired); err != nil {
			m.State(linkID, "degraded", err.Error())
			return
		}
		if err = ex.Apply(desired.Assignments); err != nil {
			m.State(linkID, "degraded", err.Error())
			return
		}
		m.State(linkID, "ready", "")
	}
	syncOnce()
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			ex.Close()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			httpServer.Shutdown(shutdownCtx)
			cancel()
			return
		case <-ticker.C:
			syncOnce()
		}
	}
}
