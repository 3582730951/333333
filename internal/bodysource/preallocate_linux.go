//go:build linux

package bodysource

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func preallocateFile(file *os.File, offset, length int64) (bool, error) {
	if file == nil || length <= 0 {
		return false, nil
	}
	err := unix.Fallocate(int(file.Fd()), unix.FALLOC_FL_KEEP_SIZE, offset, length)
	if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) {
		return false, nil
	}
	return err == nil, err
}
