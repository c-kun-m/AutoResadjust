package agent

import (
	"fmt"
	"github.com/resource-adjust/compute-platform/internal/platform"
	"runtime"
	"syscall"
	"unsafe"
)

func readHostResources() (*platform.HostResources, uint64, uint64, error) {
	kernel := syscall.NewLazyDLL("kernel32.dll")
	var mem struct {
		Length, Load                                                                          uint32
		TotalPhys, AvailPhys, TotalPage, AvailPage, TotalVirtual, AvailVirtual, AvailExtended uint64
	}
	mem.Length = uint32(unsafe.Sizeof(mem))
	ok, _, err := kernel.NewProc("GlobalMemoryStatusEx").Call(uintptr(unsafe.Pointer(&mem)))
	if ok == 0 {
		return nil, 0, 0, fmt.Errorf("host memory: %w", err)
	}
	h := &platform.HostResources{MemoryTotalMiB: int64(mem.TotalPhys / 1048576), MemoryAvailableMiB: int64(mem.AvailPhys / 1048576), CPUCount: runtime.NumCPU()}
	var idle, kern, user syscall.Filetime
	ok, _, _ = kernel.NewProc("GetSystemTimes").Call(uintptr(unsafe.Pointer(&idle)), uintptr(unsafe.Pointer(&kern)), uintptr(unsafe.Pointer(&user)))
	if ok == 0 {
		return h, 0, 0, nil
	}
	value := func(f syscall.Filetime) uint64 { return uint64(f.HighDateTime)<<32 | uint64(f.LowDateTime) }
	return h, value(kern) + value(user), value(idle), nil
}
