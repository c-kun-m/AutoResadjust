package agent

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/resource-adjust/compute-platform/internal/platform"
)

// DiscoverGPU reads the NVIDIA management CLI output available inside the WSL2
// runtime. The agent deliberately talks to nvidia-smi instead of the Windows
// host API so the same code works in a Linux container and in WSL2.
func DiscoverGPU(ctx context.Context, topologyGroup ...string) ([]platform.GPU, error) {
	cmd := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=index,name,uuid,memory.total,memory.used,utilization.gpu", "--format=csv,noheader,nounits")
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("nvidia-smi: %w", ctx.Err())
		}
		return nil, fmt.Errorf("nvidia-smi: %w", err)
	}
	group := ""
	if len(topologyGroup) > 0 {
		group = strings.TrimSpace(topologyGroup[0])
	}
	return ParseNvidiaSMIWithTopology(string(out), group)
}

// ParseNvidiaSMI parses csv output while tolerating whitespace around values.
// Unit conversion is kept in MiB because that is what the scheduler compares.
func ParseNvidiaSMI(output string) ([]platform.GPU, error) {
	return ParseNvidiaSMIWithTopology(output, "")
}

// ParseNvidiaSMIWithTopology is the testable parser used by the edge agent.
// topologyGroup is an operator-supplied grouping for multi-GPU scheduling.
func ParseNvidiaSMIWithTopology(output, topologyGroup string) ([]platform.GPU, error) {
	reader := csv.NewReader(strings.NewReader(output))
	reader.TrimLeadingSpace = true
	reader.FieldsPerRecord = 6
	reader.ReuseRecord = false

	var gpus []platform.GPU
	for {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse nvidia-smi csv: %w", err)
		}
		for i := range record {
			record[i] = strings.TrimSpace(record[i])
		}
		indexNum, err := strconv.Atoi(record[0])
		if err != nil || indexNum < 0 {
			return nil, fmt.Errorf("gpu index %q is invalid", record[0])
		}
		uuid := record[2]
		if uuid == "" || strings.EqualFold(uuid, "n/a") || strings.EqualFold(uuid, "na") {
			return nil, fmt.Errorf("gpu %d has no stable nvidia-smi uuid", indexNum)
		}
		memTotal, err := parseNonNegativeMiB(record[3])
		if err != nil {
			return nil, fmt.Errorf("gpu %s memory.total: %w", uuid, err)
		}
		memUsed, err := parseNonNegativeMiB(record[4])
		if err != nil {
			return nil, fmt.Errorf("gpu %s memory.used: %w", uuid, err)
		}
		if memUsed > memTotal {
			return nil, fmt.Errorf("gpu %s memory.used (%d) exceeds memory.total (%d)", uuid, memUsed, memTotal)
		}
		// NVIDIA can report utilization as N/A while the device is resetting or
		// while a MIG configuration is changing. It is an optional observation.
		var utilization *float64
		if value := strings.TrimSpace(record[5]); value != "" && !strings.EqualFold(value, "n/a") && !strings.EqualFold(value, "na") {
			v, err := strconv.ParseFloat(value, 64)
			if err != nil || math.IsNaN(v) || v < 0 || v > 100 {
				return nil, fmt.Errorf("gpu %s utilization %q is invalid", uuid, value)
			}
			utilization = &v
		}
		gpus = append(gpus, platform.GPU{
			ID:             uuid,
			Index:          indexNum,
			Model:          record[1],
			MemoryMiB:      int64(memTotal),
			FreeMemoryMiB:  int64(memTotal - memUsed),
			UtilizationPct: utilization,
			Vendor:         "NVIDIA",
			TopologyGroup:  strings.TrimSpace(topologyGroup),
		})
	}
	if len(gpus) == 0 {
		return nil, fmt.Errorf("nvidia-smi returned no GPUs")
	}
	seen := make(map[string]struct{}, len(gpus))
	for _, gpu := range gpus {
		if _, exists := seen[gpu.ID]; exists {
			return nil, fmt.Errorf("duplicate GPU uuid %q", gpu.ID)
		}
		seen[gpu.ID] = struct{}{}
	}
	if len(gpus) == 1 && strings.TrimSpace(topologyGroup) == "" {
		// A lone card does not need peer topology. Never use the old misleading
		// "default" value.
		gpus[0].TopologyGroup = "single"
	}
	return gpus, nil
}

func parseNonNegativeMiB(value string) (int, error) {
	if value == "" || strings.EqualFold(value, "n/a") || strings.EqualFold(value, "na") {
		return 0, fmt.Errorf("value %q is unavailable", value)
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("value %q is invalid", value)
	}
	return n, nil
}

// Probe falls back to an empty inventory when allowEmpty is true. This allows
// an agent process to be installed before the GPU driver is made available.
func Probe(ctx context.Context, allowEmpty bool, topologyGroup ...string) ([]platform.GPU, error) {
	gpus, err := DiscoverGPU(ctx, topologyGroup...)
	if err != nil && allowEmpty {
		return []platform.GPU{}, nil
	}
	if err != nil {
		return nil, err
	}
	return gpus, nil
}

var ErrUnsupportedRuntime = errors.New("GPU runtime is unavailable; install the NVIDIA driver and expose it through WSL2")

// HeartbeatInterval is intentionally shorter than the scheduler's stale-node
// timeout so a missed heartbeat can be detected without false eviction.
const HeartbeatInterval = 10 * time.Second
