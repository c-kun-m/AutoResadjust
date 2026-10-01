package main

import (
	"encoding/json"
	"github.com/resource-adjust/compute-platform/internal/platform"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestArtifactAPIAuthDurabilityAndLocalPreview(t *testing.T) {
	s := &apiServer{controller: platform.NewController(nil), stateFile: filepath.Join(t.TempDir(), "state.json"), token: "test-token"}
	a := platform.ModelArtifact{ID: "test", Name: "test", File: "test.gguf", Format: "gguf", Revision: "fixed", SHA256: strings.Repeat("a", 64), Architecture: "llama", Quantization: "Q8_0", WeightMiB: 14000, Layers: 40, ContextLimit: 2048}
	body, _ := json.Marshal(a)
	request := func(method, path, body string, auth bool) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if auth {
			r.Header.Set("Authorization", "Bearer test-token")
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	if w := request("POST", "/api/v1/model-artifacts", string(body), false); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := request("POST", "/api/v1/model-artifacts", string(body), true); w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	loaded, err := platform.LoadController(s.stateFile)
	if err != nil || len(loaded.ListArtifacts()) != 1 {
		t.Fatal("artifact not checkpointed", err)
	}
	n := platform.ResourceNode{ID: "local", Name: "local", Datacenter: "office", Runtime: "wsl2", Health: platform.NodeReady, SchedulingEnabled: true, LastHeartbeat: time.Now(), Host: &platform.HostResources{MemoryTotalMiB: 32768, MemoryAvailableMiB: 25000, CPUCount: 8}, GPUs: []platform.GPU{{ID: "gpu-a", Vendor: "NVIDIA", MemoryMiB: 8192, FreeMemoryMiB: 8192}}, Agent: &platform.AgentEndpoint{URL: "http://127.0.0.1:9999", NetworkGroup: "lan", EngineVersion: "rev", Backends: []string{platform.BackendLocal}}}
	s.controller.Nodes.UpsertNode(n)
	w := request("POST", "/api/v1/deployments/preview", `{"name":"test","backend":"llama_local","model_ref":"test","network_group":"lan","gpu_layers":10,"ram_mib":17000}`, true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ram_mib":17000`) {
		t.Fatal(w.Code, w.Body.String())
	}
	w = request("GET", "/metrics", "", true)
	if !strings.Contains(w.Body.String(), "platform_host_memory_available_bytes") {
		t.Fatal("host metrics missing")
	}
}
