//go:build linux

package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// diagnosticFileOpenBySameUID conservatively checks every process owned by the
// service UID. This prevents a promoted A/B worker from unlinking a legacy
// snapshot that the draining worker is still reading.
func diagnosticFileOpenBySameUID(path string) (bool, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return true, err
	}
	candidate, err := os.Stat(path)
	if err != nil {
		return true, err
	}
	processes, err := os.ReadDir("/proc")
	if err != nil {
		return true, err
	}
	uid := uint32(os.Geteuid())
	for _, process := range processes {
		if !process.IsDir() {
			continue
		}
		if _, parseErr := strconv.Atoi(process.Name()); parseErr != nil {
			continue
		}
		processPath := filepath.Join("/proc", process.Name())
		info, statErr := os.Stat(processPath)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return true, statErr
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uid {
			continue
		}
		fdPath := filepath.Join(processPath, "fd")
		fds, readErr := os.ReadDir(fdPath)
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return true, fmt.Errorf("inspect %s: %w", fdPath, readErr)
		}
		for _, fd := range fds {
			target, linkErr := os.Readlink(filepath.Join(fdPath, fd.Name()))
			if errors.Is(linkErr, os.ErrNotExist) {
				continue
			}
			if linkErr != nil {
				return true, fmt.Errorf("inspect %s/%s: %w", fdPath, fd.Name(), linkErr)
			}
			target = strings.TrimSuffix(target, " (deleted)")
			if target == path {
				return true, nil
			}
			targetInfo, targetErr := os.Stat(target)
			if targetErr == nil && os.SameFile(candidate, targetInfo) {
				return true, nil
			}
		}
	}
	return false, nil
}
