package main

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/resource-adjust/compute-platform/internal/platform"
)

func TestNetworkAPIAuthSyncPersistenceAndMetrics(t *testing.T) {
	s := &apiServer{controller: platform.NewController(nil), token: "test-secret", stateFile: filepath.Join(t.TempDir(), "state.json")}
	request := func(method, path, body string, auth bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if auth {
			r.Header.Set("Authorization", "Bearer test-secret")
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	if w := request("POST", "/api/v1/network-probes", `{}`, false); w.Code != 401 {
		t.Fatal(w.Code)
	}
	for _, id := range []string{"a", "b"} {
		s.controller.Nodes.UpsertNode(platform.ResourceNode{ID: id, Name: id, LastHeartbeat: time.Now(), Agent: &platform.AgentEndpoint{URL: "http://" + id + ":9090", NetworkProbe: true}})
	}
	w := request("POST", "/api/v1/network-groups", `{"id":"lan","name":"LAN","node_ids":["a","b"]}`, true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = request("POST", "/api/v1/network-probes", `{"source_id":"a","target_id":"b"}`, true)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	var p platform.NetworkProbe
	json.Unmarshal(w.Body.Bytes(), &p)
	n, _ := s.controller.Nodes.GetNode("a")
	b, _ := json.Marshal(map[string]any{"node": n})
	w = request("POST", "/api/v1/agents/a/sync", string(b), true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), p.ID) {
		t.Fatal("probe not delivered", w.Code, w.Body.String())
	}
	b, _ = json.Marshal(map[string]any{"node": n, "network_results": []platform.NetworkResult{{ProbeID: p.ID, RTTP95MS: 2, JitterMS: 1, UploadMbps: 1000, Samples: 5, PayloadBytes: platform.NetworkProbeBytes}}})
	w = request("POST", "/api/v1/agents/a/sync", string(b), true)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = request("GET", "/metrics", "", true)
	if !strings.Contains(w.Body.String(), `platform_network_upload_bits_per_second{source="a",target="b"} 1e+09`) {
		t.Fatal(w.Body.String())
	}
	restored, err := platform.LoadController(s.stateFile)
	if err != nil || len(restored.ListNetworkGroups()) != 1 || len(restored.ListNetworkLinks()) != 1 {
		t.Fatal("network checkpoint missing", err)
	}
	if restored.ListNetworkLinks()[0].Status != "stale" {
		t.Fatal("restored link incorrectly fresh")
	}
}
