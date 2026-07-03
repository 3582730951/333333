//go:build windows

package main

import "os"

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = proc.Release()
	return true
}

func terminatePID(pid int) error {
	if pid <= 0 || pid == os.Getpid() {
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	defer proc.Release()
	return proc.Kill()
}
