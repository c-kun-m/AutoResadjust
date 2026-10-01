package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/resource-adjust/compute-platform/internal/inference"
	"github.com/resource-adjust/compute-platform/internal/platform"
)

func chatFixture(t *testing.T, target string) (*apiServer, []string) {
	t.Helper()
	c := platform.NewController(nil)
	_, err := c.RegisterArtifact(platform.ModelArtifact{ID: "test", Name: "Test", Format: "gguf", File: "test.gguf", Revision: "fixed", SHA256: strings.Repeat("a", 64), Architecture: "llama", Quantization: "Q8", WeightMiB: 100, Layers: 10, ContextLimit: 1024})
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, id := range []string{"a", "b"} {
		n := platform.ResourceNode{ID: id, Name: id, Datacenter: "dc", Runtime: "docker", SchedulingEnabled: true, Health: platform.NodeReady, LastHeartbeat: time.Now(), GPUs: []platform.GPU{{ID: "gpu-" + id, Vendor: "NVIDIA", MemoryMiB: 8192, FreeMemoryMiB: 8192}}, Host: &platform.HostResources{MemoryTotalMiB: 32000, MemoryAvailableMiB: 25000}, Agent: &platform.AgentEndpoint{URL: target, EngineVersion: "fixed", NetworkGroup: "lan", Backends: []string{platform.BackendLocal}}}
		if err := c.Nodes.UpsertNode(n); err != nil {
			t.Fatal(err)
		}
		d, _, err := c.CreateDeployment(platform.DeploymentSpec{Name: id, Backend: platform.BackendLocal, ModelRef: "test", RAMMiB: 2048, GPULayers: 5, NetworkGroup: "lan", NodeIDs: []string{id}})
		if err != nil || len(d.Plan.Placements) != 1 {
			t.Fatalf("%+v %v", d, err)
		}
		if err := c.ReportWorkers(id, []platform.WorkerReport{{NodeID: id, DeploymentID: d.ID, Token: d.Token, RPCState: "ready", ModelState: "ready"}}); err != nil {
			t.Fatal(err)
		}
		c.ReconcileDeployments()
		ids = append(ids, d.ID)
	}
	if _, err := c.PutModelPool(platform.ModelPool{ID: "chat", Backend: platform.BackendLocal, ModelRef: "test", DeploymentIDs: ids}); err != nil {
		t.Fatal(err)
	}
	s := &apiServer{controller: c, token: "secret"}
	s.initMetrics()
	return s, ids
}
func chatRequest(t *testing.T, s http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

const chatBody = `{"model":"chat","messages":[{"role":"user","content":"hi"}]}`

func completeChat(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello!\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
}

func TestUnifiedModelsAuthAndUpstreamIdentity(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path != "/inference/v1/chat/completions" || body["stream"] != true || body["model"] != r.Header.Get("X-Deployment-ID") || r.Header.Get("X-Assignment-Token") == "" || r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("X-Request-ID") == "" {
			t.Error("invalid upstream contract")
		}
		completeChat(w)
	}))
	defer up.Close()
	s, ids := chatFixture(t, up.URL)
	for _, path := range []string{"/v1/models", "/v1/chat/completions"} {
		r := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatal("unauthenticated v1", w.Code)
		}
	}
	w := chatRequest(t, s, "/v1/chat/completions", chatBody)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Hello!") || !strings.Contains(w.Body.String(), `"model":"chat"`) || w.Header().Get("X-Request-ID") == "" {
		t.Fatal(w.Code, w.Body.String())
	}
	w = chatRequest(t, s, "/api/v1/deployments/"+ids[0]+"/inference/v1/chat/completions", chatBody)
	if w.Code != 200 || calls.Load() != 2 {
		t.Fatal(w.Code, w.Body.String())
	}
	if s.inference.Snapshot()[0].Completed < 1 {
		t.Fatal("legacy endpoint bypassed router")
	}
	for _, bad := range []string{chatBody + " {}", `{"model":"chat","n":2,"messages":[{}]}`, `{"model":"chat","stream":"false","messages":[{}]}`} {
		if w := chatRequest(t, s, "/v1/chat/completions", bad); w.Code != 400 {
			t.Fatal(w.Code)
		}
	}
	if calls.Load() != 2 {
		t.Fatal("malformed request dispatched")
	}
}
func TestUnifiedAndLegacyShareQueueAndPropagateCancellation(t *testing.T) {
	entered := make(chan string, 4)
	canceled := make(chan struct{}, 4)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		entered <- r.Header.Get("X-Deployment-ID")
		<-r.Context().Done()
		canceled <- struct{}{}
	}))
	defer up.Close()
	s, ids := chatFixture(t, up.URL)
	p := inference.DefaultPolicy()
	p.QueueSize = 0
	s.inference = inference.New(p)
	server := httptest.NewServer(s)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan struct{}, 2)
	for i := 0; i < 2; i++ {
		go func() {
			defer func() { finished <- struct{}{} }()
			r, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/v1/chat/completions", strings.NewReader(chatBody))
			r.Header.Set("Authorization", "Bearer secret")
			resp, err := server.Client().Do(r)
			if err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
	}
	first, second := <-entered, <-entered
	if first == second {
		t.Fatal("requests not distributed")
	}
	for _, path := range []string{"/v1/chat/completions", "/api/v1/deployments/" + ids[0] + "/inference/v1/chat/completions"} {
		if w := chatRequest(t, s, path, chatBody); w.Code != 429 {
			t.Fatal("limit bypass", w.Code, w.Body.String())
		}
	}
	cancel()
	for i := 0; i < 2; i++ {
		select {
		case <-canceled:
		case <-time.After(time.Second):
			t.Fatal("upstream not canceled")
		}
		<-finished
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		active := 0
		for _, st := range s.inference.Snapshot() {
			active += st.Active
		}
		if active == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("active slots leaked")
}

func TestQueuedRequestRechecksStopWithoutBlockingControlWrites(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); completeChat(w) }))
	defer up.Close()
	s, ids := chatFixture(t, up.URL)
	_, err := s.controller.PutModelPool(platform.ModelPool{ID: "chat", ModelRef: "test", Backend: platform.BackendLocal, DeploymentIDs: ids[:1]})
	if err != nil {
		t.Fatal(err)
	}
	first, _ := s.inference.Acquire(context.Background(), ids[:1])
	defer first.Finish("completed")
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { result <- chatRequest(t, s, "/v1/chat/completions", chatBody) }()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if s.inference.Snapshot()[0].Queued == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if s.inference.Snapshot()[0].Queued != 1 {
		t.Fatal("request never queued")
	}
	stopped := make(chan *httptest.ResponseRecorder, 1)
	go func() { stopped <- chatRequest(t, s, "/api/v1/deployments/"+ids[0]+"/stop", `{}`) }()
	select {
	case w := <-stopped:
		if w.Code != 202 {
			t.Fatal(w.Code, w.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("inference blocked control-plane mutation")
	}
	first.Finish("completed")
	select {
	case w := <-result:
		if w.Code != 503 {
			t.Fatal(w.Code, w.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("queued request did not finish")
	}
	if calls.Load() != 0 {
		t.Fatal("stopped deployment received queued work")
	}
}
