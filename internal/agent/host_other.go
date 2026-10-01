//go:build !linux && !windows

package agent

import (
	"fmt"
	"github.com/resource-adjust/compute-platform/internal/platform"
)

func readHostResources() (*platform.HostResources, uint64, uint64, error) {
	return nil, 0, 0, fmt.Errorf("host probe unsupported")
}
