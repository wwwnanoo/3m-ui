package system

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

// ProcessUsage is RSS + CPU for a single OS process (panel or Mihomo core).
type ProcessUsage struct {
	PID           int     `json:"pid"`
	CPUPercent    float64 `json:"cpu_percent"`
	MemoryUsed    float64 `json:"memory_used"`    // PSS bytes (proportional, dedup shared libs)
	MemoryPercent float64 `json:"memory_percent"` // 0–100 of system RAM
}

var (
	procCPUMu   sync.Mutex
	procCPULast = map[int32]struct {
		pct float64
		at  time.Time
	}{}
)

// readPSS reads /proc/[pid]/smaps_rollup to get the Proportional Set Size
// (PSS). Unlike RSS, PSS divides shared library pages by the number of
// processes sharing them — so panel + mihomo PSS values add up correctly
// (matching systemd's cgroup Memory: total).
//
// If smaps_rollup is not available (older kernels, restricted containers),
// falls back to RSS from gopsutil.
func readPSS(pid int) float64 {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/smaps_rollup")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "Pss:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if kb, err := strconv.ParseFloat(fields[1], 64); err == nil {
					return kb * 1024 // KB → bytes
				}
			}
		}
	}
	return 0
}

// SampleProcessUsage returns CPU/memory for pid. CPU uses gopsutil's delta
// sampler (first call after a gap may be ~0); recent values are cached briefly.
//
// Memory uses PSS (Proportional Set Size) from /proc/[pid]/smaps_rollup when
// available — this accounts for shared memory proportionally, so panel + core
// PSS values sum correctly (matching systemd cgroup Memory: total).
// Falls back to RSS if smaps_rollup is unavailable.
func SampleProcessUsage(pid int) ProcessUsage {
	out := ProcessUsage{PID: pid}
	if pid <= 0 {
		return out
	}
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return out
	}

	// Try PSS first (accurate, deduplicates shared memory).
	memBytes := readPSS(pid)
	if memBytes == 0 {
		// Fallback: RSS (includes all shared library pages per-process).
		if mi, err := p.MemoryInfo(); err == nil && mi != nil {
			memBytes = float64(mi.RSS)
		}
	}
	out.MemoryUsed = memBytes

	// Memory percent = used / total system RAM * 100.
	if mp, err := p.MemoryPercent(); err == nil {
		// gopsutil's MemoryPercent uses RSS. If we have PSS, recompute.
		if memBytes > 0 {
			// Get total memory from the same source gopsutil uses.
			if vm, err := readMemTotal(); err == nil && vm > 0 {
				out.MemoryPercent = clampPercent(memBytes / vm * 100)
			} else {
				out.MemoryPercent = clampPercent(float64(mp))
			}
		} else {
			out.MemoryPercent = clampPercent(float64(mp))
		}
	}

	// Non-blocking percent since last sample for this PID.
	if pct, err := p.CPUPercent(); err == nil {
		v := clampPercent(pct)
		procCPUMu.Lock()
		procCPULast[int32(pid)] = struct {
			pct float64
			at  time.Time
		}{pct: v, at: time.Now()}
		procCPUMu.Unlock()
		out.CPUPercent = v
		return out
	}
	procCPUMu.Lock()
	if last, ok := procCPULast[int32(pid)]; ok && time.Since(last.at) < 30*time.Second {
		out.CPUPercent = last.pct
	}
	procCPUMu.Unlock()
	return out
}

// readMemTotal returns total system memory in bytes from /proc/meminfo.
func readMemTotal() (float64, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if kb, err := strconv.ParseFloat(fields[1], 64); err == nil {
					return kb * 1024, nil
				}
			}
		}
	}
	return 0, nil
}
