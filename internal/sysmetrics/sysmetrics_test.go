package sysmetrics

import "testing"

func TestCollect(t *testing.T) {
	m := Collect("/")
	// On the Linux CI/dev/VPS host /proc exists, so Collect must report supported with
	// sane values. (If this ever runs on a non-Linux host, Supported is false and we skip.)
	if !m.Supported {
		t.Skip("no /proc on this host (non-Linux) — Collect correctly returned Supported=false")
	}
	if m.CPU.Cores <= 0 {
		t.Errorf("cores = %d, want > 0", m.CPU.Cores)
	}
	if m.Mem.TotalKB <= 0 {
		t.Errorf("mem total = %d kB, want > 0", m.Mem.TotalKB)
	}
	if m.Mem.UsedPct < 0 || m.Mem.UsedPct > 100 {
		t.Errorf("mem used%% = %v, want 0..100", m.Mem.UsedPct)
	}
	if m.Disk.TotalBytes == 0 {
		t.Errorf("disk total = 0, want > 0 for /")
	}
	if m.Go.Goroutines <= 0 {
		t.Errorf("goroutines = %d, want > 0", m.Go.Goroutines)
	}
	// Registration procs may legitimately be empty when no registration is running;
	// just assert the slice/counters are coherent.
	if m.Registration.Node+m.Registration.Chrome+m.Registration.Xvfb+m.Registration.Other != len(m.Registration.Procs) &&
		len(m.Registration.Procs) < 40 {
		t.Errorf("proc counters (%d) disagree with proc list (%d)",
			m.Registration.Node+m.Registration.Chrome+m.Registration.Xvfb+m.Registration.Other, len(m.Registration.Procs))
	}
}

func TestClassifyComm(t *testing.T) {
	cases := map[string]string{
		"node": "node", "nodejs": "node",
		"chrome": "chrome", "chromium": "chrome", "chrome_crashpad": "chrome",
		"Xvfb": "xvfb",
		"bash": "", "pool-server": "",
	}
	for comm, want := range cases {
		if got := classifyComm(comm); got != want {
			t.Errorf("classifyComm(%q) = %q, want %q", comm, got, want)
		}
	}
}
