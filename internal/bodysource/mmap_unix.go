//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package bodysource

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func mapFileReadOnly(path string, size int64) ([]byte, error) {
	if size <= 0 || size > int64(int(^uint(0)>>1)) {
		return nil, errors.New("spool mapping size is unsupported")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return unix.Mmap(int(file.Fd()), 0, int(size), unix.PROT_READ, unix.MAP_SHARED)
}

func unmapFile(view []byte) error {
	if len(view) == 0 {
		return nil
	}
	return unix.Munmap(view)
}
