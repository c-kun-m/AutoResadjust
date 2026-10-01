package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/resource-adjust/compute-platform/internal/platform"
	"github.com/resource-adjust/compute-platform/internal/telemetry"
)

func TestPetalsCommandsPinIdentityAndHideLease(t *testing.T) {
	peer := &platform.PetalsEndpoint{PeerID: "Qm" + strings.Repeat("A", 44), Address: "10.0.0.1:31332", InitialPeers: []string{"/ip4/10.0.0.9/tcp/31330/p2p/Qm" + strings.Repeat("B", 44)}}
	dir := t.TempDir()
	cfg := ExecutorConfig{PetalsPython: "python", PetalsIdentity: filepath.Join(dir, "node.key"), PetalsStateDir: dir, PetalsHTTP: "127.0.0.1:18082", ModelBackend: "127.0.0.1:18081", PetalsEndpoint: peer, ModelDir: dir}
	p := platform.Placement{NodeID: "a", GPUID: "GPU-physical", Petals: peer, StartBlock: 0, EndBlock: 2, Coordinator: true}
	w := platform.WorkAssignment{DeploymentID: "dep-private", Token: "secret-lease", Placement: p, Peers: []platform.Placement{p}, Spec: platform.DeploymentSpec{Backend: platform.BackendPetals, ModelRef: "pinned", ModelFile: "sealed", ModelSHA256: strings.Repeat("a", 64), Layers: 2, ContextSize: 128, KVCacheMiB: 16, BlockMiB: 1, LoadRAMMiB: 1024, KVBytesPerTokenPerLayer: 1024}}
	for _, role := range []string{"worker", "gateway"} {
		cmd, err := petalsCommand(cfg, w, role)
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(cmd.Args[3])
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), w.Token) || strings.Contains(strings.Join(cmd.Args, " "), w.Token) {
			t.Fatal("lease leaked into subprocess")
		}
		var config map[string]any
		if err = json.Unmarshal(data, &config); err != nil {
			t.Fatal(err)
		}
		if config["model_dir"] != filepath.Join(dir, "sealed") || !strings.HasPrefix(config["prefix"].(string), "collab-") {
			t.Fatal(config)
		}
		device := "-1"
		if role == "worker" {
			device = "GPU-physical"
		}
		if cmd.Env[0] != "CUDA_VISIBLE_DEVICES="+device {
			t.Fatal("wrong GPU isolation")
		}
	}
	w.Placement.Petals = &platform.PetalsEndpoint{PeerID: "Qm" + strings.Repeat("C", 44), Address: peer.Address, InitialPeers: peer.InitialPeers}
	if _, err := petalsCommand(cfg, w, "worker"); err == nil {
		t.Fatal("different assigned identity allowed")
	}
}

func TestPetalsWorkerHealthIdentityCountersAndRuntimeFailure(t *testing.T) {
	var ready, wrong atomic.Bool
	ready.Store(true)
	peer := "Qm" + strings.Repeat("A", 44)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			w.WriteHeader(503)
			return
		}
		identity := peer
		if wrong.Load() {
			identity = "other"
		}
		json.NewEncoder(w).Encode(map[string]any{"ready": true, "profile": platform.PetalsProfile, "peer_id": identity, "start_block": 0, "end_block": 2, "sessions_total": 4, "rx_bytes_total": 31, "tx_bytes_total": 57, "active_sessions": 1, "errors_total": 1, "duration_seconds_total": 2.5, "kv_cache_bytes": 1024})
	}))
	defer server.Close()
	m := telemetry.New()
	m.Register(telemetry.Service{ID: "worker", Kind: "petals-worker", Traffic: true})
	e := NewExecutor(ExecutorConfig{PetalsHTTP: strings.TrimPrefix(server.URL, "http://")}, m)
	x := &execution{rpcID: "worker", backend: petalsBackend{}, rpc: &child{started: time.Now().Add(-time.Hour)}, work: platform.WorkAssignment{Placement: platform.Placement{Petals: &platform.PetalsEndpoint{PeerID: peer}, StartBlock: 0, EndBlock: 2}}}
	e.tickPetalsWorker(x)
	if x.report.RPCState != "ready" || !x.workerReady {
		t.Fatal(x.report)
	}
	s := m.List(false)[0]
	if s.RX != 31 || s.TX != 57 || s.Requests != 4 || s.Active != 1 || s.Engine["kv_cache_bytes"] != 1024 {
		t.Fatal(s)
	}
	ready.Store(false)
	e.tickPetalsWorker(x)
	if x.finished || x.report.RPCState != "degraded" {
		t.Fatal("runtime degradation misclassified as startup timeout")
	}
	ready.Store(true)
	wrong.Store(true)
	e.tickPetalsWorker(x)
	if x.report.RPCState == "ready" {
		t.Fatal("wrong process identity accepted")
	}
}
