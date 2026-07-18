package admission

import (
	"bufio"
	"context"
	"errors"
	"os"
	"runtime"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"codex-account-pool/internal/supervisor"
)

var ErrCapacity = errors.New("resource headroom cannot accept another waiter")

type Snapshot struct {
	Timestamp       int64   `json:"timestamp"`
	Paused          bool    `json:"paused"`
	Reason          string  `json:"reason,omitempty"`
	CPUUsedPercent  float64 `json:"cpu_used_percent"`
	MemUsedPercent  float64 `json:"memory_used_percent"`
	FDUsedPercent   float64 `json:"fd_used_percent"`
	GoAllocBytes    uint64  `json:"go_alloc_bytes"`
	Goroutines      int     `json:"goroutines"`
	HeadroomPercent int     `json:"headroom_percent"`
	ReservedBytes   int64   `json:"reserved_bytes"`
}
type Controller struct {
	headroom            atomic.Int64
	snap                atomic.Value
	mu                  sync.Mutex
	changed             chan struct{}
	recoveries          int
	prevIdle, prevTotal uint64
	prevCGUsage         int64
	prevCGAt            time.Time
	reserved            atomic.Int64
	stop                chan struct{}
	stopOnce            sync.Once
}

func New(headroom int) *Controller {
	c := &Controller{changed: make(chan struct{}), stop: make(chan struct{})}
	c.SetHeadroom(headroom)
	c.snap.Store(Snapshot{Timestamp: time.Now().Unix(), HeadroomPercent: int(c.headroom.Load())})
	// Unit/integration test binaries construct hundreds of short-lived servers and
	// have no HTTP shutdown hook to close their handlers. Avoid leaking one sampler
	// per fixture; admission behavior itself has dedicated controller/load tests.
	if !strings.HasSuffix(os.Args[0], ".test") {
		supervisor.GoOnce("admission-resource-sampler", c.run)
	}
	return c
}
func (c *Controller) SetHeadroom(v int) {
	if v < 10 {
		v = 10
	}
	c.headroom.Store(int64(v))
}
func (c *Controller) Snapshot() Snapshot { return c.snap.Load().(Snapshot) }
func (c *Controller) Reserve(ctx context.Context, bytes int64) (func(), error) {
	if bytes < 0 {
		bytes = 0
	}
	if s := c.Snapshot(); s.Paused && (s.Reason == "memory" || s.Reason == "fd") {
		return nil, ErrCapacity
	}
	if err := c.Wait(ctx); err != nil {
		return nil, err
	}
	// Atomically account concurrent admissions against the same memory margin.
	// A sampled snapshot alone can let a burst of large requests all observe the
	// same free space and collectively cross the safety line.
	limit := memoryLimit()
	for {
		current := c.reserved.Load()
		if limit > 0 {
			s := c.Snapshot()
			baseUsed := s.MemUsedPercent - 100*float64(s.ReservedBytes)/float64(limit)
			projected := baseUsed + 100*float64(current+bytes)/float64(limit)
			if projected >= float64(100-c.Snapshot().HeadroomPercent) {
				return nil, ErrCapacity
			}
		}
		if c.reserved.CompareAndSwap(current, current+bytes) {
			break
		}
	}
	return func() { c.reserved.Add(-bytes) }, nil
}
func (c *Controller) Wait(ctx context.Context) error {
	for c.Snapshot().Paused {
		c.mu.Lock()
		ch := c.changed
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		}
	}
	return nil
}

// allocMetric reads live heap allocation via runtime/metrics, which — unlike
// runtime.ReadMemStats — does NOT stop the world. GoAllocBytes is reported for
// observability only (it never drives the pause/resume decision in sample()),
// so this swap removes a stop-the-world pause from the 4x/second control loop
// with no behavioral change. Guarded by a mutex because the sample slice is
// reused and metrics.Read mutates it.
var (
	allocMetricMu     sync.Mutex
	allocMetricSample = []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
)

func heapAllocBytes() uint64 {
	allocMetricMu.Lock()
	defer allocMetricMu.Unlock()
	metrics.Read(allocMetricSample)
	if allocMetricSample[0].Value.Kind() == metrics.KindUint64 {
		return allocMetricSample[0].Value.Uint64()
	}
	return 0
}

func (c *Controller) run() {
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		c.sample()
		select {
		case <-t.C:
		case <-c.stop:
			return
		}
	}
}

func (c *Controller) Close() { c.stopOnce.Do(func() { close(c.stop) }) }
func (c *Controller) sample() {
	head := int(c.headroom.Load())
	stop, resume := float64(100-head), float64(95-head)
	old := c.Snapshot()
	s := Snapshot{Timestamp: time.Now().Unix(), HeadroomPercent: head, Goroutines: runtime.NumGoroutine(), Paused: old.Paused}
	s.GoAllocBytes = heapAllocBytes()
	s.CPUUsedPercent = c.cpuPercent()
	if cg := c.cgroupCPUPercent(); cg > s.CPUUsedPercent {
		s.CPUUsedPercent = cg
	}
	s.MemUsedPercent = memoryPercent()
	s.ReservedBytes = c.reserved.Load()
	if limit := memoryLimit(); limit > 0 {
		s.MemUsedPercent += 100 * float64(s.ReservedBytes) / float64(limit)
	}
	s.FDUsedPercent = fdPercent()
	for _, r := range []struct {
		name string
		used float64
	}{{"cpu", s.CPUUsedPercent}, {"memory", s.MemUsedPercent}, {"fd", s.FDUsedPercent}} {
		if r.used >= stop {
			s.Paused = true
			s.Reason = r.name
			c.recoveries = 0
			break
		}
	}
	if s.Paused && s.Reason == "" {
		if s.CPUUsedPercent < resume && s.MemUsedPercent < resume && s.FDUsedPercent < resume {
			c.recoveries++
		} else {
			c.recoveries = 0
		}
		if c.recoveries >= 2 {
			s.Paused = false
			c.recoveries = 0
		} else {
			s.Reason = old.Reason
		}
	}
	c.snap.Store(s)
	if old.Paused != s.Paused {
		c.mu.Lock()
		close(c.changed)
		c.changed = make(chan struct{})
		c.mu.Unlock()
	}
}

func (c *Controller) cgroupCPUPercent() float64 {
	b, err := os.ReadFile("/sys/fs/cgroup/cpu.stat")
	if err != nil {
		return 0
	}
	var usage int64
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "usage_usec" {
			usage, _ = strconv.ParseInt(fields[1], 10, 64)
			break
		}
	}
	max, err := os.ReadFile("/sys/fs/cgroup/cpu.max")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(max))
	if len(fields) != 2 || fields[0] == "max" {
		return 0
	}
	quota, _ := strconv.ParseFloat(fields[0], 64)
	period, _ := strconv.ParseFloat(fields[1], 64)
	if quota <= 0 || period <= 0 {
		return 0
	}
	now := time.Now()
	prevUsage, prevAt := c.prevCGUsage, c.prevCGAt
	c.prevCGUsage, c.prevCGAt = usage, now
	if prevUsage == 0 || prevAt.IsZero() || usage < prevUsage {
		return 0
	}
	capacityUsec := float64(now.Sub(prevAt).Microseconds()) * quota / period
	if capacityUsec <= 0 {
		return 0
	}
	used := 100 * float64(usage-prevUsage) / capacityUsec
	if used > 100 {
		return 100
	}
	if used < 0 {
		return 0
	}
	return used
}
func (c *Controller) cpuPercent() float64 {
	f, e := os.Open("/proc/stat")
	if e != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return 0
	}
	v := strings.Fields(sc.Text())
	if len(v) < 5 {
		return 0
	}
	var total, idle uint64
	for i, x := range v[1:] {
		n, _ := strconv.ParseUint(x, 10, 64)
		total += n
		if i == 3 || i == 4 {
			idle += n
		}
	}
	pi, pt := c.prevIdle, c.prevTotal
	c.prevIdle, c.prevTotal = idle, total
	if pt == 0 || total <= pt {
		return 0
	}
	return 100 * (1 - float64(idle-pi)/float64(total-pt))
}
func memoryPercent() float64 {
	limit, used := readInt("/sys/fs/cgroup/memory.max"), readInt("/sys/fs/cgroup/memory.current")
	if limit <= 0 {
		limit, used = memInfo("MemTotal:"), memInfo("MemAvailable:")
		used = limit - used
	}
	if limit <= 0 {
		return 0
	}
	return 100 * float64(used) / float64(limit)
}
func memoryLimit() int64 {
	if n := readInt("/sys/fs/cgroup/memory.max"); n > 0 {
		return n
	}
	return memInfo("MemTotal:")
}
func fdPercent() float64 {
	var r syscall.Rlimit
	if syscall.Getrlimit(syscall.RLIMIT_NOFILE, &r) != nil || r.Cur == 0 {
		return 0
	}
	es, e := os.ReadDir("/proc/self/fd")
	if e != nil {
		return 0
	}
	return 100 * float64(len(es)) / float64(r.Cur)
}
func readInt(p string) int64 {
	b, e := os.ReadFile(p)
	if e != nil || strings.TrimSpace(string(b)) == "max" {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return n
}
func memInfo(key string) int64 {
	f, e := os.Open("/proc/meminfo")
	if e != nil {
		return 0
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if strings.HasPrefix(s.Text(), key) {
			v := strings.Fields(s.Text())
			if len(v) > 1 {
				n, _ := strconv.ParseInt(v[1], 10, 64)
				return n * 1024
			}
		}
	}
	return 0
}
