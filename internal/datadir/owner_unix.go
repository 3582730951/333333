//go:build !windows

package datadir

import (
	"fmt"
	"os"
	"syscall"
)

func verifyOwner(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot determine owner")
	}
	if int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("owner uid %d does not match process uid %d", stat.Uid, os.Geteuid())
	}
	return nil
}

func verifyCredentialOwner(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot determine owner")
	}
	if int(stat.Uid) != os.Geteuid() && stat.Uid != 0 {
		return fmt.Errorf("owner uid %d is neither process uid %d nor root", stat.Uid, os.Geteuid())
	}
	return nil
}
