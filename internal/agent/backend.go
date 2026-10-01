package agent

import (
	"context"
	"fmt"
	"net"

	"github.com/resource-adjust/compute-platform/internal/platform"
)

type ProcessCommand struct {
	Binary    string
	Args, Env []string
}

// Backend describes execution commands; Executor owns the shared lease,
// process lifetime, drain, health and telemetry boundaries for every backend.
type Backend interface {
	Kind() string
	Worker(ExecutorConfig, platform.WorkAssignment) (*ProcessCommand, error)
	Model(context.Context, ExecutorConfig, platform.WorkAssignment) (ProcessCommand, error)
}

type llamaBackend struct{ local bool }

func (b llamaBackend) Kind() string {
	if b.local {
		return platform.BackendLocal
	}
	return platform.BackendRPC
}
func (b llamaBackend) Worker(cfg ExecutorConfig, w platform.WorkAssignment) (*ProcessCommand, error) {
	if b.local {
		return nil, nil
	}
	if cfg.RPCBinary == "" {
		return nil, fmt.Errorf("RPC binary not configured")
	}
	_, port, err := net.SplitHostPort(cfg.RPCBackend)
	if err != nil {
		return nil, err
	}
	return &ProcessCommand{cfg.RPCBinary, []string{"--host", "127.0.0.1", "--port", port, "--device", "CUDA0"}, []string{"CUDA_VISIBLE_DEVICES=" + w.Placement.GPUID, "GGML_RPC_NO_RDMA=1"}}, nil
}
func (b llamaBackend) Model(ctx context.Context, cfg ExecutorConfig, w platform.WorkAssignment) (ProcessCommand, error) {
	if cfg.ServerBinary == "" {
		return ProcessCommand{}, fmt.Errorf("model server binary not configured")
	}
	args, err := modelArgs(ctx, cfg, w)
	device := "-1"
	if b.local {
		device = w.Placement.GPUID
	}
	return ProcessCommand{cfg.ServerBinary, args, []string{"CUDA_VISIBLE_DEVICES=" + device, "GGML_RPC_NO_RDMA=1"}}, err
}
func selectBackend(kind string) (Backend, error) {
	switch kind {
	case "", platform.BackendRPC:
		return llamaBackend{}, nil
	case platform.BackendLocal:
		return llamaBackend{local: true}, nil
	default:
		return nil, fmt.Errorf("backend %q is not enabled on this agent", kind)
	}
}

func (e *Executor) Capabilities() []string {
	out := []string{}
	if e.cfg.ServerBinary != "" {
		out = append(out, platform.BackendLocal)
		if e.cfg.RPCBinary != "" {
			out = append(out, platform.BackendRPC)
		}
	}
	return out
}
