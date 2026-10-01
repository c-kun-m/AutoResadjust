package agent

import (
	"github.com/resource-adjust/compute-platform/internal/platform"
	"sync"
)

type HostProbe struct {
	mu          sync.Mutex
	total, idle uint64
}

func (p *HostProbe) Probe() *platform.HostResources {
	p.mu.Lock()
	defer p.mu.Unlock()
	h, total, idle, err := readHostResources()
	if err != nil {
		return nil
	}
	if total > p.total && p.total != 0 && idle >= p.idle && idle-p.idle <= total-p.total {
		v := 100 * (1 - float64(idle-p.idle)/float64(total-p.total))
		h.CPUUtilizationPct = &v
	}
	p.total, p.idle = total, idle
	return h
}
