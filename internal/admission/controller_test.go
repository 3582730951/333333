package admission

import (
	"testing"
	"time"
)

func TestAdmissionSampleIntervalAdaptsToPressure(t *testing.T) {
	normal := Snapshot{HeadroomPercent: 20, CPUUsedPercent: 20, MemUsedPercent: 30, FDUsedPercent: 10}
	if got := admissionSampleInterval(normal); got != 2*time.Second {
		t.Fatalf("normal interval = %s", got)
	}
	near := normal
	near.MemUsedPercent = 70
	if got := admissionSampleInterval(near); got != 250*time.Millisecond {
		t.Fatalf("near-limit interval = %s", got)
	}
	paused := normal
	paused.Paused = true
	if got := admissionSampleInterval(paused); got != 250*time.Millisecond {
		t.Fatalf("paused interval = %s", got)
	}
}
