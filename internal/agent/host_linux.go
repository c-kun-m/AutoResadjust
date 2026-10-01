package agent

import (
	"fmt"
	"github.com/resource-adjust/compute-platform/internal/platform"
	"os"
	"runtime"
	"strconv"
	"strings"
)

func readHostResources() (*platform.HostResources, uint64, uint64, error) {
	mem, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return nil, 0, 0, err
	}
	h := &platform.HostResources{CPUCount: runtime.NumCPU()}
	for _, line := range strings.Split(string(mem), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseInt(f[1], 10, 64)
		if f[0] == "MemTotal:" {
			h.MemoryTotalMiB = v / 1024
		}
		if f[0] == "MemAvailable:" {
			h.MemoryAvailableMiB = v / 1024
		}
	}
	// Container limits are tighter than host capacity. Cgroup v1/v2 both apply.
	for _, pair := range [][2]string{{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory.current"}, {"/sys/fs/cgroup/memory/memory.limit_in_bytes", "/sys/fs/cgroup/memory/memory.usage_in_bytes"}} {
		limit, e1 := os.ReadFile(pair[0])
		used, e2 := os.ReadFile(pair[1])
		if e1 != nil || e2 != nil {
			continue
		}
		cap, e1 := strconv.ParseInt(strings.TrimSpace(string(limit)), 10, 64)
		usage, e2 := strconv.ParseInt(strings.TrimSpace(string(used)), 10, 64)
		if e1 != nil || e2 != nil || cap <= 0 {
			continue
		}
		if cap/1048576 < h.MemoryTotalMiB {
			h.MemoryTotalMiB = cap / 1048576
		}
		available := (cap - usage) / 1048576
		if available < 0 {
			available = 0
		}
		if available < h.MemoryAvailableMiB {
			h.MemoryAvailableMiB = available
		}
	}
	if h.MemoryTotalMiB <= 0 {
		return nil, 0, 0, fmt.Errorf("host memory unavailable")
	}
	stat, err := os.ReadFile("/proc/stat")
	if err != nil {
		return h, 0, 0, nil
	}
	f := strings.Fields(strings.SplitN(string(stat), "\n", 2)[0])
	var total, idle uint64
	for i := 1; i < len(f) && i <= 8; i++ {
		v, _ := strconv.ParseUint(f[i], 10, 64)
		total += v
		if i == 4 || i == 5 {
			idle += v
		}
	}
	return h, total, idle, nil
}
