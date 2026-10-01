package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/resource-adjust/compute-platform/internal/platform"
	"github.com/resource-adjust/compute-platform/internal/telemetry"
)

type petalsBackend struct{}

func (petalsBackend) Kind() string { return platform.BackendPetals }
func (petalsBackend) Worker(cfg ExecutorConfig, w platform.WorkAssignment) (*ProcessCommand, error) {
	c, err := petalsCommand(cfg, w, "worker")
	return &c, err
}
func (petalsBackend) Model(ctx context.Context, cfg ExecutorConfig, w platform.WorkAssignment) (ProcessCommand, error) {
	if err := ctx.Err(); err != nil {
		return ProcessCommand{}, err
	}
	return petalsCommand(cfg, w, "gateway")
}

func petalsCommand(cfg ExecutorConfig, w platform.WorkAssignment, role string) (ProcessCommand, error) {
	if cfg.PetalsPython == "" || cfg.PetalsStateDir == "" || cfg.PetalsIdentity == "" || platform.ValidatePetalsEndpoint(cfg.PetalsEndpoint) != nil || platform.ValidatePetalsEndpoint(w.Placement.Petals) != nil {
		return ProcessCommand{}, fmt.Errorf("private Petals runtime is not configured")
	}
	expected, actual := *cfg.PetalsEndpoint, *w.Placement.Petals
	expected.InitialPeers = append([]string(nil), expected.InitialPeers...)
	actual.InitialPeers = append([]string(nil), actual.InitialPeers...)
	sort.Strings(expected.InitialPeers)
	sort.Strings(actual.InitialPeers)
	a, _ := json.Marshal(expected)
	b, _ := json.Marshal(actual)
	if string(a) != string(b) {
		return ProcessCommand{}, fmt.Errorf("assigned private identity or bootstrap differs from agent configuration")
	}
	if w.Spec.ModelRef == "" || w.Spec.ModelSHA256 == "" || w.Spec.ModelFile == "" || w.Spec.ModelFile == "." || w.Spec.ModelFile == ".." || strings.ContainsAny(w.Spec.ModelFile, "/\\:\x00") {
		return ProcessCommand{}, fmt.Errorf("sealed model basename and digest required")
	}
	model, err := filepath.Abs(filepath.Join(cfg.ModelDir, w.Spec.ModelFile))
	if err != nil {
		return ProcessCommand{}, err
	}
	state, err := filepath.Abs(cfg.PetalsStateDir)
	if err != nil {
		return ProcessCommand{}, err
	}
	identity, err := filepath.Abs(cfg.PetalsIdentity)
	if err != nil {
		return ProcessCommand{}, err
	}
	address := cfg.PetalsHTTP
	if role == "gateway" {
		address = cfg.ModelBackend
	}
	h, port, err := net.SplitHostPort(address)
	if err != nil || h != "127.0.0.1" {
		return ProcessCommand{}, fmt.Errorf("Petals HTTP process must bind to loopback")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return ProcessCommand{}, fmt.Errorf("invalid HTTP port")
	}
	assignment := sha256.Sum256([]byte(w.DeploymentID + "\x00" + w.Token))
	prefix := "collab-" + hex.EncodeToString(assignment[:])
	config := map[string]any{"deployment_id": w.DeploymentID, "prefix": prefix, "model_dir": model, "manifest_sha256": w.Spec.ModelSHA256, "initial_peers": actual.InitialPeers, "context_size": w.Spec.ContextSize, "http_port": number,
		"expected_metadata": map[string]any{"layers": w.Spec.Layers, "block_mib": w.Spec.BlockMiB, "load_ram_mib": w.Spec.LoadRAMMiB, "kv_bytes_per_token_per_layer": w.Spec.KVBytesPerTokenPerLayer}}
	device := "-1"
	if role == "worker" {
		h, port, _ = net.SplitHostPort(actual.Address)
		number, _ = strconv.Atoi(port)
		config["advertise_ip"], config["p2p_port"], config["identity_path"], config["peer_id"] = h, number, identity, actual.PeerID
		config["start_block"], config["end_block"], config["kv_cache_mib"] = w.Placement.StartBlock, w.Placement.EndBlock, w.Spec.KVCacheMiB
		device = w.Placement.GPUID
	} else {
		placements := []map[string]any{}
		for _, p := range w.Peers {
			if p.Petals == nil {
				return ProcessCommand{}, fmt.Errorf("peer is missing private identity")
			}
			placements = append(placements, map[string]any{"peer_id": p.Petals.PeerID, "start_block": p.StartBlock, "end_block": p.EndBlock})
		}
		config["placements"] = placements
	}
	data, err := json.Marshal(config)
	if err != nil {
		return ProcessCommand{}, err
	}
	if err = os.MkdirAll(state, 0700); err != nil {
		return ProcessCommand{}, err
	}
	path := filepath.Join(state, prefix+"-"+role+".json")
	// One executor owns this directory. Config contains public identities and a
	// hash of the assignment; never expose the control-plane or lease token.
	if err = os.WriteFile(path, data, 0600); err != nil {
		return ProcessCommand{}, err
	}
	return ProcessCommand{cfg.PetalsPython, []string{"-m", "petals_backend." + role, "--config", path}, []string{"CUDA_VISIBLE_DEVICES=" + device, "HF_HUB_OFFLINE=1", "TRANSFORMERS_OFFLINE=1", "PYTHONUNBUFFERED=1"}}, nil
}

func (e *Executor) tickPetalsWorker(x *execution) {
	resp, err := e.client.Get("http://" + e.cfg.PetalsHTTP + "/health")
	var data map[string]any
	ready := false
	if resp != nil {
		decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(&data)
		resp.Body.Close()
		if err == nil && decodeErr == nil && resp.StatusCode == 200 {
			ready = data["ready"] == true && data["profile"] == platform.PetalsProfile && x.work.Placement.Petals != nil && data["peer_id"] == x.work.Placement.Petals.PeerID && data["start_block"] == float64(x.work.Placement.StartBlock) && data["end_block"] == float64(x.work.Placement.EndBlock)
		}
	}
	if !ready {
		x.report.RPCState = "starting"
		e.metrics.State(x.rpcID, "loading", "waiting for verified blocks, matching peer identity and healthy runtime")
		if x.workerReady {
			x.report.RPCState = "degraded"
			e.metrics.State(x.rpcID, "degraded", "block runtime unavailable; new inference admission closed")
		}
		if !x.workerReady && time.Since(x.rpc.started) > 15*time.Minute {
			e.failLocked("Petals worker unavailable after startup budget")
		}
		return
	}
	x.workerReady = true
	x.report.RPCState = "ready"
	e.metrics.State(x.rpcID, "ready", "assigned blocks ready; inference payload excludes DHT and network framing")
	values := map[string]float64{}
	for k, raw := range data {
		if v, ok := raw.(float64); ok && !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 1<<53 {
			values[k] = v
		}
	}
	e.metrics.Update(x.rpcID, func(s *telemetry.Service) {
		s.Engine = values
		s.Requests = uint64(values["sessions_total"])
		s.Errors = uint64(values["errors_total"])
		s.Active = int64(values["active_sessions"])
		s.RX = uint64(values["rx_bytes_total"])
		s.TX = uint64(values["tx_bytes_total"])
		s.LatencySum = values["duration_seconds_total"]
	})
}
