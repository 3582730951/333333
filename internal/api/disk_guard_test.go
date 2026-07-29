package api

import "testing"

func TestDiskGuardThresholdsAndHysteresis(t *testing.T) {
	cases := []struct {
		free           float64
		bytes          uint64
		previous, want string
	}{
		{20, 8 << 30, "normal", "normal"},
		{9.9, 8 << 30, "normal", "pressure"},
		{20, (2 << 30) - 1, "normal", "pressure"},
		{4.9, 8 << 30, "pressure", "critical"},
		{20, (512 << 20) - 1, "pressure", "critical"},
		{1.9, 8 << 30, "critical", "emergency"},
		{20, (128 << 20) - 1, "critical", "emergency"},
		{14.9, 8 << 30, "critical", "pressure"},
		{20, (4 << 30) - 1, "pressure", "pressure"},
		{15, 4 << 30, "pressure", "normal"},
	}
	for _, tc := range cases {
		if got := diskGuardLevel(tc.free, tc.bytes, tc.previous); got != tc.want {
			t.Errorf("free=%v bytes=%d previous=%s got=%s want=%s", tc.free, tc.bytes, tc.previous, got, tc.want)
		}
	}
}
