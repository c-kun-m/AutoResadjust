package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"time"

	"github.com/resource-adjust/compute-platform/internal/agent"
	"github.com/resource-adjust/compute-platform/internal/platform"
)

type nodeInspection struct {
	Version      int                     `json:"version"`
	MeasuredAt   time.Time               `json:"measured_at"`
	OS           string                  `json:"os"`
	Architecture string                  `json:"architecture"`
	Scope        string                  `json:"scope"`
	GPUs         []platform.GPU          `json:"gpus"`
	Host         *platform.HostResources `json:"host"`
	Errors       []string                `json:"errors"`
}

func inspectNode(ctx context.Context, output io.Writer) error {
	probe := &agent.HostProbe{}
	return writeInspection(ctx, output, func(ctx context.Context) ([]platform.GPU, error) {
		return agent.DiscoverGPU(ctx)
	}, probe.Probe)
}

func writeInspection(ctx context.Context, output io.Writer, gpuProbe func(context.Context) ([]platform.GPU, error), hostProbe func() *platform.HostResources) error {
	result := nodeInspection{Version: 1, MeasuredAt: time.Now().UTC(), OS: runtime.GOOS, Architecture: runtime.GOARCH,
		Scope: "measured in this process runtime; container RAM may be capped; GPU discovery does not prove model compatibility, physical host count or network reachability",
		GPUs:  []platform.GPU{}, Errors: []string{}}
	gpus, err := gpuProbe(ctx)
	if err != nil {
		result.Errors = append(result.Errors, err.Error())
	} else if len(gpus) == 0 {
		result.Errors = append(result.Errors, "no GPU discovered")
	} else {
		result.GPUs = gpus
	}
	result.Host = hostProbe()
	if result.Host == nil {
		result.Errors = append(result.Errors, "host resources unavailable")
	}
	if err := json.NewEncoder(output).Encode(result); err != nil {
		return err
	}
	if len(result.Errors) > 0 {
		return fmt.Errorf("node inspection incomplete; see errors in the JSON report")
	}
	return nil
}
