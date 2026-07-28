//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package bodysource

import "errors"

func mapFileReadOnly(string, int64) ([]byte, error) {
	return nil, errors.New("read-only spool mapping is unsupported on this platform")
}

func unmapFile([]byte) error { return nil }
