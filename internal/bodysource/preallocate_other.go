//go:build !linux

package bodysource

import "os"

func preallocateFile(_ *os.File, _, _ int64) (bool, error) { return false, nil }
