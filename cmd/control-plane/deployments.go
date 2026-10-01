package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/resource-adjust/compute-platform/internal/platform"
	"github.com/resource-adjust/compute-platform/internal/telemetry"
)

func (s *apiServer) initMetrics() {
	s.initOnce.Do(func() {
		if s.metrics == nil {
			s.metrics = telemetry.New()
		}
		s.metrics.Register(telemetry.Service{ID: "control-plane", Name: "Control plane API", Kind: "control-plane", Status: "ready", HTTP: true, Traffic: true})
		s.metrics.Register(telemetry.Service{ID: "web-console", Name: "Web console", Kind: "web-console", Status: "ready", HTTP: true, Traffic: true})
	})
}

type bufferedResponse struct {
	header http.Header
	body   bytes.Buffer
	code   int
}

func (b *bufferedResponse) Header() http.Header { return b.header }
func (b *bufferedResponse) WriteHeader(c int) {
	if b.code == 0 {
		b.code = c
	}
}
func (b *bufferedResponse) Write(p []byte) (int, error) {
	if b.code == 0 {
		b.code = 200
	}
	return b.body.Write(p)
}

func (s *apiServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.initMetrics()
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" || r.Method == "OPTIONS" || strings.Contains(r.URL.Path, "/inference/") {
			s.serveAPI(w, r)
			return
		}
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		b := &bufferedResponse{header: make(http.Header)}
		s.serveAPI(b, r)
		if b.code >= 200 && b.code < 300 {
			if err := s.controller.Save(s.stateFile); err != nil {
				s.metrics.State("control-plane", "degraded", "state checkpoint failed")
				writeError(w, 503, fmt.Errorf("state checkpoint failed: %w", err))
				return
			}
		}
		for k, v := range b.header {
			w.Header()[k] = v
		}
		if b.code == 0 {
			b.code = 200
		}
		w.WriteHeader(b.code)
		_, _ = b.body.WriteTo(w)
	})
	if r.URL.Path == "/metrics" || r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
		handler.ServeHTTP(w, r)
		return
	}
	id := "control-plane"
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		id = "web-console"
	}
	// Inference has a dedicated service counter; do not double count its body.
	if strings.Contains(r.URL.Path, "/inference/") {
		handler.ServeHTTP(w, r)
		return
	}
	s.metrics.HTTP(id, handler).ServeHTTP(w, r)
}

func (s *apiServer) expectServices() {
	for _, n := range s.controller.Nodes.ListNodes() {
		s.metrics.Expect(telemetry.Service{ID: n.ID + "/agent", Name: "Edge agent", Kind: "agent", NodeID: n.ID, Status: "starting", HTTP: true, Traffic: true})
	}
	for _, d := range s.controller.ListDeployments() {
		s.metrics.Register(telemetry.Service{ID: "gateway/" + d.ID, Name: d.Spec.Name + " gateway", Kind: "inference-gateway", DeploymentID: d.ID, HTTP: true, Traffic: true})
		s.metrics.State("gateway/"+d.ID, d.Phase, d.Message)
		for _, p := range d.Plan.Placements {
			if d.Spec.Backend != platform.BackendLocal {
				s.metrics.Expect(telemetry.Service{ID: p.NodeID + "/rpc/" + d.ID, Name: "GPU RPC worker", Kind: "rpc-worker", NodeID: p.NodeID, DeploymentID: d.ID, Address: p.RPCAddress, Traffic: true})
			}
			if p.Coordinator {
				s.metrics.Expect(telemetry.Service{ID: p.NodeID + "/model/" + d.ID, Name: d.Spec.Name, Kind: "model-server", NodeID: p.NodeID, DeploymentID: d.ID, Address: p.AgentURL + "/inference", HTTP: true, Traffic: true})
			}
		}
	}
}

func (s *apiServer) extendedRoute(w http.ResponseWriter, r *http.Request, path string) bool {
	if s.networkRoute(w, r, path) {
		return true
	}
	switch {
	case path == "/metrics" && r.Method == "GET":
		s.expectServices()
		s.metrics.Prometheus(w, r)
		s.networkMetrics(w)
		for _, n := range s.controller.Nodes.ListNodes() {
			fresh := !n.LastHeartbeat.IsZero() && time.Since(n.LastHeartbeat) < 35*time.Second
			if fresh && n.Host != nil {
				labels := "node=" + strconv.Quote(n.ID)
				fmt.Fprintf(w, "platform_host_memory_total_bytes{%s} %d\nplatform_host_memory_available_bytes{%s} %d\nplatform_host_memory_reserved_bytes{%s} %d\nplatform_host_cpu_count{%s} %d\n", labels, n.Host.MemoryTotalMiB*1048576, labels, n.Host.MemoryAvailableMiB*1048576, labels, n.ReservedRAMMiB*1048576, labels, n.Host.CPUCount)
				if n.Host.CPUUtilizationPct != nil {
					fmt.Fprintf(w, "platform_host_cpu_utilization_percent{%s} %g\n", labels, *n.Host.CPUUtilizationPct)
				}
			}
			for _, g := range n.GPUs {
				labels := "node=" + strconv.Quote(n.ID) + ",gpu=" + strconv.Quote(g.ID) + ",model=" + strconv.Quote(g.Model)
				if !fresh {
					continue
				}
				fmt.Fprintf(w, "platform_gpu_memory_total_bytes{%s} %d\nplatform_gpu_memory_free_bytes{%s} %d\n", labels, g.MemoryMiB*1048576, labels, g.FreeMemoryMiB*1048576)
				if g.UtilizationPct != nil {
					fmt.Fprintf(w, "platform_gpu_utilization_percent{%s} %g\n", labels, *g.UtilizationPct)
				}
			}
		}
		return true
	case path == "/api/v1/model-artifacts" && r.Method == "GET":
		writeJSON(w, 200, s.controller.ListArtifacts())
		return true
	case path == "/api/v1/model-artifacts" && r.Method == "POST":
		var a platform.ModelArtifact
		if err := decodeJSON(r, &a); err != nil {
			writeError(w, 400, err)
			return true
		}
		a, err := s.controller.RegisterArtifact(a)
		if err != nil {
			writeError(w, 400, err)
		} else {
			writeJSON(w, 201, a)
		}
		return true
	case path == "/api/v1/monitor/services" && r.Method == "GET":
		s.expectServices()
		writeJSON(w, 200, map[string]any{"services": s.metrics.List(true), "sample_interval_seconds": 5, "history_points": 120, "traffic_scope": "HTTP/RPC payload bytes excluding transport headers; infrastructure uses project-scoped container NIC bytes; different layers must not be summed"})
		return true
	case path == "/api/v1/deployments" && r.Method == "GET":
		writeJSON(w, 200, s.controller.ListDeployments())
		return true
	case (path == "/api/v1/deployments" || path == "/api/v1/deployments/preview") && r.Method == "POST":
		var spec platform.DeploymentSpec
		if err := decodeJSON(r, &spec); err != nil {
			writeError(w, 400, err)
			return true
		}
		if path == "/api/v1/deployments/preview" {
			plan, err := s.controller.PlanDeployment(spec)
			status := 200
			msg := ""
			if err != nil {
				status = 422
				msg = err.Error()
			}
			writeJSON(w, status, map[string]any{"plan": plan, "error": msg})
			return true
		}
		if spec.IdempotencyKey == "" {
			spec.IdempotencyKey = r.Header.Get("Idempotency-Key")
		}
		d, replay, err := s.controller.CreateDeployment(spec)
		if err != nil {
			writeError(w, 400, err)
		} else {
			s.expectServices()
			status := 202
			if replay {
				status = 200
			}
			writeJSON(w, status, map[string]any{"deployment": d, "idempotent_replay": replay})
		}
		return true
	case strings.HasPrefix(path, "/api/v1/deployments/"):
		parts := strings.Split(strings.TrimPrefix(path, "/api/v1/deployments/"), "/")
		id := parts[0]
		if len(parts) == 2 && parts[1] == "stop" && r.Method == "POST" {
			d, err := s.controller.StopDeployment(id)
			if err != nil {
				writeError(w, 404, err)
			} else {
				writeJSON(w, 202, d)
			}
			return true
		}
		if len(parts) == 1 && r.Method == "GET" {
			d, ok := s.controller.GetDeployment(id)
			if !ok {
				http.NotFound(w, r)
			} else {
				writeJSON(w, 200, d)
			}
			return true
		}
		if len(parts) == 4 && parts[1] == "inference" && parts[2] == "v1" && parts[3] == "chat" {
			http.NotFound(w, r)
			return true
		}
		if len(parts) == 5 && parts[1] == "inference" && parts[2] == "v1" && parts[3] == "chat" && parts[4] == "completions" && r.Method == "POST" {
			s.proxyInference(w, r, id)
			return true
		}
	case strings.HasPrefix(path, "/api/v1/agents/") && strings.HasSuffix(path, "/sync") && r.Method == "POST":
		nodeID := strings.TrimSuffix(strings.TrimPrefix(path, "/api/v1/agents/"), "/sync")
		var payload struct {
			Node           platform.ResourceNode    `json:"node"`
			Reports        []platform.WorkerReport  `json:"reports"`
			Services       []telemetry.Service      `json:"services"`
			NetworkResults []platform.NetworkResult `json:"network_results"`
		}
		if err := decodeJSON(r, &payload); err != nil {
			writeError(w, 400, err)
			return true
		}
		if payload.Node.ID != nodeID || nodeID == "" || strings.ContainsAny(nodeID, "/\\") || len(payload.Services) > 128 || len(payload.NetworkResults) > 32 {
			writeError(w, 400, fmt.Errorf("invalid agent payload"))
			return true
		}
		for _, m := range payload.Services {
			if m.NodeID != nodeID || !strings.HasPrefix(m.ID, nodeID+"/") {
				writeError(w, 400, fmt.Errorf("service identity does not belong to agent"))
				return true
			}
		}
		payload.Node.LastHeartbeat = time.Now().UTC()
		if _, _, err := s.controller.ApplyHeartbeat(payload.Node); err != nil {
			writeError(w, 400, err)
			return true
		}
		if err := s.controller.ReportWorkers(nodeID, payload.Reports); err != nil {
			writeError(w, 409, err)
			return true
		}
		for _, m := range payload.Services {
			s.metrics.Ingest(m)
		}
		s.controller.ReportNetwork(nodeID, payload.NetworkResults)
		s.controller.ReconcileDeployments()
		writeJSON(w, 200, map[string]any{"assignments": s.controller.Assignments(nodeID), "network_probes": s.controller.NetworkAssignments(nodeID)})
		return true
	case strings.HasPrefix(path, "/api/v1/agents/") && strings.HasSuffix(path, "/logs") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api/v1/agents/"), "/logs")
		node, ok := s.controller.Nodes.GetNode(id)
		if !ok || node.Agent == nil {
			http.NotFound(w, r)
			return true
		}
		ctx := r.Context()
		req, err := http.NewRequestWithContext(ctx, "GET", node.Agent.URL+"/logs", nil)
		if err != nil {
			writeError(w, 502, err)
			return true
		}
		if s.token != "" {
			req.Header.Set("Authorization", "Bearer "+s.token)
		}
		client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := client.Do(req)
		if err != nil {
			writeError(w, 502, err)
			return true
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, io.LimitReader(resp.Body, 128*1024))
		return true
	}
	return false
}

func (s *apiServer) proxyInference(w http.ResponseWriter, r *http.Request, id string) {
	s.expectServices()
	if _, ok := s.controller.GetDeployment(id); !ok {
		http.NotFound(w, r)
		return
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target, err := s.controller.InferenceTarget(id)
		if err != nil {
			writeError(w, 503, err)
			return
		}
		d, _ := s.controller.GetDeployment(id)
		// Normalize the model alias to this deployment while preserving stream and
		// sampling parameters. The bounded request body is never logged.
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, 400, err)
			return
		}
		if body == nil {
			writeError(w, 400, fmt.Errorf("JSON object required"))
			return
		}
		body["model"], _ = json.Marshal(id)
		data, _ := json.Marshal(body)
		r.Body = io.NopCloser(bytes.NewReader(data))
		r.ContentLength = int64(len(data))
		u, _ := url.Parse(target)
		proxy := httputil.NewSingleHostReverseProxy(u)
		director := proxy.Director
		proxy.Director = func(q *http.Request) {
			director(q)
			q.URL.Path = "/inference/v1/chat/completions"
			q.URL.RawPath = ""
			q.Header.Set("X-Deployment-ID", id)
			q.Header.Set("X-Assignment-Token", d.Token)
			q.Header.Del("Authorization")
			if s.token != "" {
				q.Header.Set("Authorization", "Bearer "+s.token)
			}
		}
		proxy.FlushInterval = -1
		proxy.Transport = &http.Transport{ResponseHeaderTimeout: 10 * time.Minute, IdleConnTimeout: 30 * time.Second}
		proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			writeError(w, 502, fmt.Errorf("inference upstream: %w", err))
		}
		proxy.ServeHTTP(w, r)
	})
	s.metrics.HTTP("gateway/"+id, handler).ServeHTTP(w, r)
}
