// Package sysmetrics reads host + process resource metrics from /proc (Linux) with the
// standard library only — no external deps. It powers the admin "系统监控" view so an
// operator can see VPS CPU/memory/disk and how much memory the registration tasks
// (the node/Chrome/Xvfb subprocesses the orchestrator spawns) are consuming, entirely
// from the web UI. On a non-Linux host (no /proc) Collect returns Supported=false and
// the page degrades gracefully.
package sysmetrics

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"codex-account-pool/internal/supervisor"
)

// ProcInfo is one registration-related process and its resident memory.
type ProcInfo struct {
	PID   int    `json:"pid"`
	Comm  string `json:"comm"`
	RSSKB int64  `json:"rss_kb"`
	Kind  string `json:"kind"` // node | chrome | xvfb | other
}

// Metrics is the full host + process snapshot returned to the admin UI.
type Metrics struct {
	Supported bool  `json:"supported"`
	Timestamp int64 `json:"timestamp"`
	CPU       struct {
		Scope    string  `json:"scope"` // cgroup | host
		Cores    int     `json:"cores"`
		Load1    float64 `json:"load1"`
		Load5    float64 `json:"load5"`
		Load15   float64 `json:"load15"`
		UsagePct float64 `json:"usage_pct"` // sampled busy% across all cores
	} `json:"cpu"`
	Mem struct {
		Scope       string  `json:"scope"` // cgroup | host
		TotalKB     int64   `json:"total_kb"`
		AvailableKB int64   `json:"available_kb"`
		UsedKB      int64   `json:"used_kb"`
		UsedPct     float64 `json:"used_pct"`
	} `json:"mem"`
	Disk struct {
		Path       string  `json:"path"`
		TotalBytes uint64  `json:"total_bytes"`
		FreeBytes  uint64  `json:"free_bytes"`
		UsedBytes  uint64  `json:"used_bytes"`
		UsedPct    float64 `json:"used_pct"`
	} `json:"disk"`
	Network struct {
		Interfaces       int      `json:"interfaces"`
		InterfaceNames   []string `json:"interface_names"`
		RXBytes          uint64   `json:"rx_bytes"`
		TXBytes          uint64   `json:"tx_bytes"`
		RXBytesPerSec    float64  `json:"rx_bytes_per_sec"`
		TXBytesPerSec    float64  `json:"tx_bytes_per_sec"`
		TotalBytesPerSec float64  `json:"total_bytes_per_sec"`
	} `json:"network"`
	UptimeSeconds int64 `json:"uptime_seconds"`
	Go            struct {
		AllocBytes uint64 `json:"alloc_bytes"`
		SysBytes   uint64 `json:"sys_bytes"`
		Goroutines int    `json:"goroutines"`
		NumGC      uint32 `json:"num_gc"`
	} `json:"go"`
	Registration struct {
		Node       int        `json:"node"`
		Chrome     int        `json:"chrome"`
		Xvfb       int        `json:"xvfb"`
		Other      int        `json:"other"`
		TotalRSSKB int64      `json:"total_rss_kb"`
		Procs      []ProcInfo `json:"procs"`
	} `json:"registration"`
}

// Collect gathers a single snapshot. dataDir is the path whose filesystem usage is
// reported (the pool's data dir); falls back to "/" when empty.
var sharedSnapshot atomic.Value
var samplerOnce sync.Once
var lastAccessNanos atomic.Int64

// samplerIdleWindow is how long after the most recent Collect the background
// sampler keeps refreshing. Beyond it the sampler idles (no /proc scan, no
// STW MemStats read) until an admin polls again — an admin page open at any
// reasonable interval keeps it warm, an unwatched server stops paying for it.
const samplerIdleWindow = 30 * time.Second

func Collect(dataDir string) Metrics {
	lastAccessNanos.Store(time.Now().UnixNano())
	samplerOnce.Do(func() {
		sharedSnapshot.Store(collectNow(dataDir))
		supervisor.GoOnce("system-metrics-sampler", func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for range ticker.C {
				// Skip the full snapshot when nobody has polled recently, so an
				// idle gateway does not scan the entire /proc tree and STW-read
				// MemStats every second in perpetuity. The next Collect re-arms it.
				if time.Since(time.Unix(0, lastAccessNanos.Load())) > samplerIdleWindow {
					continue
				}
				sharedSnapshot.Store(collectNow(dataDir))
			}
		})
	})
	if value := sharedSnapshot.Load(); value != nil {
		return value.(Metrics)
	}
	return collectNow(dataDir)
}

func collectNow(dataDir string) Metrics {
	var m Metrics
	m.Timestamp = time.Now().Unix()
	if _, err := os.Stat("/proc"); err != nil {
		m.Supported = false
		return m
	}
	m.Supported = true
	m.CPU.Cores = runtime.NumCPU()
	readLoadAvg(&m)
	m.CPU.UsagePct = sampleCPUUsage()
	readMemInfo(&m)
	applyCgroupLimits(&m)
	readDisk(&m, dataDir)
	readNetwork(&m)
	m.UptimeSeconds = readUptime()
	readGoRuntime(&m)
	readRegistrationProcs(&m)
	return m
}

var networkSample struct {
	sync.Mutex
	rx, tx uint64
	at     time.Time
}

// readNetwork aggregates all non-loopback interfaces. Rates use the same shared
// one-second sampler as the rest of the system page, so polling the admin endpoint
// never performs a blocking measurement or creates extra sampling work.
func readNetwork(m *Metrics) {
	raw, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return
	}
	var rx, tx uint64
	names := make([]string, 0, 4)
	for _, line := range strings.Split(string(raw), "\n") {
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		name := strings.TrimSpace(parts[0])
		if name == "" || name == "lo" {
			continue
		}
		fields := strings.Fields(parts[1])
		if len(fields) < 16 {
			continue
		}
		rxValue, rxErr := strconv.ParseUint(fields[0], 10, 64)
		txValue, txErr := strconv.ParseUint(fields[8], 10, 64)
		if rxErr != nil || txErr != nil {
			continue
		}
		names = append(names, name)
		rx += rxValue
		tx += txValue
	}
	sort.Strings(names)
	m.Network.Interfaces = len(names)
	m.Network.InterfaceNames = names
	m.Network.RXBytes = rx
	m.Network.TXBytes = tx

	now := time.Now()
	networkSample.Lock()
	previousRX, previousTX, previousAt := networkSample.rx, networkSample.tx, networkSample.at
	networkSample.rx, networkSample.tx, networkSample.at = rx, tx, now
	networkSample.Unlock()
	if previousAt.IsZero() || rx < previousRX || tx < previousTX {
		return
	}
	seconds := now.Sub(previousAt).Seconds()
	if seconds <= 0 {
		return
	}
	m.Network.RXBytesPerSec = float64(rx-previousRX) / seconds
	m.Network.TXBytesPerSec = float64(tx-previousTX) / seconds
	m.Network.TotalBytesPerSec = m.Network.RXBytesPerSec + m.Network.TXBytesPerSec
}

func readLoadAvg(m *Metrics) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return
	}
	f := strings.Fields(string(b))
	if len(f) >= 3 {
		m.CPU.Load1, _ = strconv.ParseFloat(f[0], 64)
		m.CPU.Load5, _ = strconv.ParseFloat(f[1], 64)
		m.CPU.Load15, _ = strconv.ParseFloat(f[2], 64)
	}
}

var cpuSample struct {
	sync.Mutex
	idle, total uint64
}

// sampleCPUUsage uses the previous shared sample and never sleeps in an HTTP handler.
func sampleCPUUsage() float64 {
	idle2, total2, ok := readCPUStat()
	if !ok {
		return -1
	}
	cpuSample.Lock()
	idle1, total1 := cpuSample.idle, cpuSample.total
	cpuSample.idle, cpuSample.total = idle2, total2
	cpuSample.Unlock()
	if total1 == 0 || total2 <= total1 {
		return 0
	}
	dTotal := float64(total2 - total1)
	dIdle := float64(idle2 - idle1)
	usage := (1 - dIdle/dTotal) * 100
	if usage < 0 {
		usage = 0
	}
	if usage > 100 {
		usage = 100
	}
	return roundPct(usage)
}

func readCPUStat() (idle, total uint64, ok bool) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		f := strings.Fields(line)[1:] // user nice system idle iowait irq softirq steal ...
		var sum uint64
		for i, v := range f {
			n, _ := strconv.ParseUint(v, 10, 64)
			sum += n
			if i == 3 { // idle
				idle = n
			}
			if i == 4 { // iowait counts as idle-ish
				idle += n
			}
		}
		return idle, sum, true
	}
	return 0, 0, false
}

func readMemInfo(m *Metrics) {
	m.Mem.Scope = "host"
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return
	}
	defer f.Close()
	var total, avail int64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			total = parseMemKB(line)
		case strings.HasPrefix(line, "MemAvailable:"):
			avail = parseMemKB(line)
		}
	}
	m.Mem.TotalKB = total
	m.Mem.AvailableKB = avail
	if total > 0 {
		m.Mem.UsedKB = total - avail
		m.Mem.UsedPct = roundPct(float64(total-avail) / float64(total) * 100)
	}
}

var cgroupCPUSample struct {
	sync.Mutex
	usage int64
	at    time.Time
}

// applyCgroupLimits makes the primary system figures describe the resources that the
// service can actually consume. /proc reports the host on many container runtimes,
// which previously made the UI show (for example) 64 GiB and 32 cores for a 2 GiB / 2
// core service. Unlimited cgroups deliberately retain the host figures.
func applyCgroupLimits(m *Metrics) {
	m.CPU.Scope = "host"
	maxRaw, maxErr := os.ReadFile("/sys/fs/cgroup/memory.max")
	currentRaw, currentErr := os.ReadFile("/sys/fs/cgroup/memory.current")
	if maxErr == nil && currentErr == nil && strings.TrimSpace(string(maxRaw)) != "max" {
		limit, lerr := strconv.ParseInt(strings.TrimSpace(string(maxRaw)), 10, 64)
		used, uerr := strconv.ParseInt(strings.TrimSpace(string(currentRaw)), 10, 64)
		if lerr == nil && uerr == nil && limit > 0 {
			if used < 0 {
				used = 0
			}
			if used > limit {
				used = limit
			}
			m.Mem.Scope = "cgroup"
			m.Mem.TotalKB = limit / 1024
			m.Mem.UsedKB = used / 1024
			m.Mem.AvailableKB = (limit - used) / 1024
			m.Mem.UsedPct = roundPct(100 * float64(used) / float64(limit))
		}
	}

	cpuMax, err := os.ReadFile("/sys/fs/cgroup/cpu.max")
	if err != nil {
		return
	}
	fields := strings.Fields(string(cpuMax))
	if len(fields) != 2 || fields[0] == "max" {
		return
	}
	quota, qerr := strconv.ParseFloat(fields[0], 64)
	period, perr := strconv.ParseFloat(fields[1], 64)
	if qerr != nil || perr != nil || quota <= 0 || period <= 0 {
		return
	}
	cores := quota / period
	m.CPU.Scope = "cgroup"
	m.CPU.Cores = max(1, int(cores+0.999999))
	if usage, ok := readCgroupCPUUsage(); ok {
		m.CPU.UsagePct = usage
	}
}

func readCgroupCPUUsage() (float64, bool) {
	raw, err := os.ReadFile("/sys/fs/cgroup/cpu.stat")
	if err != nil {
		return 0, false
	}
	var usage int64
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "usage_usec" {
			usage, err = strconv.ParseInt(fields[1], 10, 64)
			break
		}
	}
	if err != nil || usage <= 0 {
		return 0, false
	}
	now := time.Now()
	cgroupCPUSample.Lock()
	previous, previousAt := cgroupCPUSample.usage, cgroupCPUSample.at
	cgroupCPUSample.usage, cgroupCPUSample.at = usage, now
	cgroupCPUSample.Unlock()
	if previous <= 0 || previousAt.IsZero() || usage < previous {
		return 0, true
	}
	elapsed := now.Sub(previousAt).Microseconds()
	if elapsed <= 0 {
		return 0, true
	}
	// usage delta divided by wall time is effective CPU utilisation. It may exceed
	// 100% with several cores, so normalise by the cgroup quota read by the caller.
	cpuMax, err := os.ReadFile("/sys/fs/cgroup/cpu.max")
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(cpuMax))
	quota, _ := strconv.ParseFloat(fields[0], 64)
	period, _ := strconv.ParseFloat(fields[1], 64)
	capacity := float64(elapsed) * quota / period
	if capacity <= 0 {
		return 0, false
	}
	value := 100 * float64(usage-previous) / capacity
	if value < 0 {
		value = 0
	}
	if value > 100 {
		value = 100
	}
	return roundPct(value), true
}

func parseMemKB(line string) int64 {
	f := strings.Fields(line)
	if len(f) >= 2 {
		n, _ := strconv.ParseInt(f[1], 10, 64)
		return n
	}
	return 0
}

func readDisk(m *Metrics, dataDir string) {
	path := strings.TrimSpace(dataDir)
	if path == "" {
		path = "/"
	}
	if st, err := os.Stat(path); err != nil || !st.IsDir() {
		path = "/"
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return
	}
	bs := uint64(fs.Bsize)
	total := fs.Blocks * bs
	free := fs.Bavail * bs
	m.Disk.Path = path
	m.Disk.TotalBytes = total
	m.Disk.FreeBytes = free
	if total > 0 {
		m.Disk.UsedBytes = total - free
		m.Disk.UsedPct = roundPct(float64(total-free) / float64(total) * 100)
	}
}

func readUptime() int64 {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) >= 1 {
		sec, _ := strconv.ParseFloat(f[0], 64)
		return int64(sec)
	}
	return 0
}

func readGoRuntime(m *Metrics) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	m.Go.AllocBytes = ms.Alloc
	m.Go.SysBytes = ms.Sys
	m.Go.NumGC = ms.NumGC
	m.Go.Goroutines = runtime.NumGoroutine()
}

// readRegistrationProcs scans /proc for the subprocesses the registration orchestrator
// spawns (node worker + its Chrome + the per-process Xvfb) and sums their resident
// memory, so the operator sees exactly how much RAM auto-registration is using.
func readRegistrationProcs(m *Metrics) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	procs := make([]ProcInfo, 0)
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // not a pid dir
		}
		comm := readProcComm(pid)
		kind := classifyComm(comm)
		if kind == "" {
			continue
		}
		rss := readProcRSSKB(pid)
		procs = append(procs, ProcInfo{PID: pid, Comm: comm, RSSKB: rss, Kind: kind})
		switch kind {
		case "node":
			m.Registration.Node++
		case "chrome":
			m.Registration.Chrome++
		case "xvfb":
			m.Registration.Xvfb++
		default:
			m.Registration.Other++
		}
		m.Registration.TotalRSSKB += rss
	}
	sort.Slice(procs, func(i, j int) bool { return procs[i].RSSKB > procs[j].RSSKB })
	if len(procs) > 40 {
		procs = procs[:40]
	}
	m.Registration.Procs = procs
}

func classifyComm(comm string) string {
	c := strings.ToLower(comm)
	switch {
	case c == "node" || c == "nodejs":
		return "node"
	case strings.Contains(c, "chrome") || strings.Contains(c, "chromium"):
		return "chrome"
	case c == "xvfb":
		return "xvfb"
	}
	return ""
}

func readProcComm(pid int) string {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func readProcRSSKB(pid int) int64 {
	f, err := os.Open(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "VmRSS:") {
			return parseMemKB(line)
		}
	}
	return 0
}

func roundPct(v float64) float64 {
	return float64(int64(v*10+0.5)) / 10
}
