package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/resource-adjust/compute-platform/internal/platform"
	"github.com/resource-adjust/compute-platform/internal/telemetry"
)

func TestReadinessReflectsCheckpointFailure(t *testing.T) {
	s := &apiServer{controller: platform.NewController(nil)}
	s.initMetrics()
	s.metrics.State("control-plane", "degraded", "state checkpoint failed")
	out := httptest.NewRecorder()
	s.ServeHTTP(out, httptest.NewRequest("GET", "/readyz", nil))
	if out.Code != 503 {
		t.Fatalf("reported ready after checkpoint failure: %d", out.Code)
	}
	s.metrics.State("control-plane", "ready", "")
	out = httptest.NewRecorder()
	s.ServeHTTP(out, httptest.NewRequest("GET", "/readyz", nil))
	if out.Code != 200 {
		t.Fatal(out.Code)
	}
}

func TestAPIGroupLifecyclePersistenceAndStreamingMetrics(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/inference/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("X-Assignment-Token") == "" {
			t.Error("proxy routing/auth headers incorrect")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	s := &apiServer{controller: platform.NewController(nil), token: "secret", stateFile: filepath.Join(t.TempDir(), "state.json")}
	s.initMetrics()
	server := httptest.NewServer(s)
	defer server.Close()
	request := func(method, path, body string, auth bool) (int, []byte) {
		t.Helper()
		r, _ := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		if auth {
			r.Header.Set("Authorization", "Bearer secret")
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	if code, _ := request("GET", "/api/v1/monitor/services", "", false); code != 401 {
		t.Fatal(code)
	}
	for _, id := range []string{"a", "b"} {
		node := platform.ResourceNode{ID: id, Name: id, Datacenter: "dc", Runtime: "wsl2-docker", Health: platform.NodeReady, SchedulingEnabled: true, LastHeartbeat: time.Now(), GPUs: []platform.GPU{{ID: "gpu-" + id, Vendor: "NVIDIA", FreeMemoryMiB: 8192, MemoryMiB: 8192}}, Agent: &platform.AgentEndpoint{URL: upstream.URL, RPCAddress: id + ":50052", NetworkGroup: "lan", EngineVersion: "rev"}}
		s.controller.Nodes.UpsertNode(node)
	}
	code, b := request("POST", "/api/v1/deployments", `{"name":"test","model_file":"test.gguf","weight_mib":12000,"network_group":"lan","idempotency_key":"one"}`, true)
	if code != 202 {
		t.Fatal(code, string(b))
	}
	var result struct {
		Deployment platform.Deployment `json:"deployment"`
	}
	json.Unmarshal(b, &result)
	d := result.Deployment
	for _, p := range d.Plan.Placements {
		node, _ := s.controller.Nodes.GetNode(p.NodeID)
		r := platform.WorkerReport{NodeID: p.NodeID, DeploymentID: d.ID, Token: d.Token, RPCState: "ready"}
		if p.Coordinator {
			r.ModelState = "ready"
		}
		payload, _ := json.Marshal(map[string]any{"node": node, "reports": []platform.WorkerReport{r}, "services": []telemetry.Service{{ID: p.NodeID + "/agent", NodeID: p.NodeID, Instance: "one", Status: "ready"}}})
		if code, b := request("POST", "/api/v1/agents/"+p.NodeID+"/sync", string(payload), true); code != 200 {
			t.Fatal(code, string(b))
		}
	}
	code, b = request("POST", "/api/v1/deployments/"+d.ID+"/inference/v1/chat/completions", `{"messages":[{"role":"user","content":"hi"}],"stream":true}`, true)
	if code != 200 || !strings.Contains(string(b), "[DONE]") {
		t.Fatal(code, string(b))
	}
	code, b = request("GET", "/metrics", "", true)
	if code != 200 || !strings.Contains(string(b), "gateway/"+d.ID) || !strings.Contains(string(b), "platform_service_transmit_bytes_total") {
		t.Fatal("metrics unavailable", string(b))
	}
	loaded, err := platform.LoadController(s.stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := loaded.GetDeployment(d.ID); !ok {
		t.Fatal("deployment not checkpointed")
	}
	request("POST", "/api/v1/deployments/"+d.ID+"/stop", "", true)
	code, _ = request("POST", "/api/v1/deployments/"+d.ID+"/inference/v1/chat/completions", `{"messages":[]}`, true)
	if code != 503 {
		t.Fatal("stopping service still routes", code)
	}
}
