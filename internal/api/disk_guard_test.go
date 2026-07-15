package api

import "testing"

func TestDiskGuardThresholdsAndHysteresis(t *testing.T) {
	cases := []struct {
		free           float64
		previous, want string
	}{
		{12, "normal", "normal"}, {7.9, "normal", "pressure"},
		{4.9, "pressure", "critical"}, {6, "critical", "pressure"},
		{9.9, "pressure", "pressure"}, {10, "pressure", "normal"},
	}
	for _, tc := range cases {
		if got := diskGuardLevel(tc.free, tc.previous); got != tc.want {
			t.Errorf("free=%v previous=%s got=%s want=%s", tc.free, tc.previous, got, tc.want)
		}
	}
}
