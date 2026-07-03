package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	gatewayPIDFileName = "gateway.pid"
	gatewayLogFileName = "gateway.log"
)

func gatewayPIDPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), gatewayPIDFileName)
}

func gatewayLogPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), gatewayLogFileName)
}

func handleStartBackground(configPath string) int {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		fmt.Printf("Load config failed: %v\n", err)
		return 1
	}
	if strings.TrimSpace(cfg.DownstreamKey) == "" {
		fmt.Println("downstream_key not configured. Run: gateway init --key cap_xxx")
		return 1
	}
	if gatewayTCPReachable(cfg.ListenAddr) {
		fmt.Println("✓ Gateway already running:", cfg.ListenAddr)
		return 0
	}

	dir := filepath.Dir(configPath)
	if err := os.MkdirAll(dir, gatewayPrivateDirMode); err != nil {
		fmt.Printf("Create gateway dir failed: %v\n", err)
		return 1
	}
	if err := chmodGatewayPrivateDir(dir); err != nil {
		fmt.Printf("Harden gateway dir failed: %v\n", err)
		return 1
	}

	exe, err := os.Executable()
	if err != nil {
		fmt.Printf("Resolve gateway executable failed: %v\n", err)
		return 1
	}
	logPath := gatewayLogPath(configPath)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, gatewayPrivateFileMode)
	if err != nil {
		fmt.Printf("Open gateway log failed: %v\n", err)
		return 1
	}
	defer logFile.Close()

	cmdPath := exe
	cmdArgs := []string{"start"}
	if nohupPath, err := exec.LookPath("nohup"); err == nil {
		cmdPath = nohupPath
		cmdArgs = []string{exe, "start"}
	}
	cmd := exec.Command(cmdPath, cmdArgs...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		fmt.Printf("Start gateway failed: %v\n", err)
		return 1
	}
	pid := cmd.Process.Pid
	if err := os.WriteFile(gatewayPIDPath(configPath), []byte(strconv.Itoa(pid)+"\n"), gatewayConfigFileMode); err != nil {
		_ = terminatePID(pid)
		fmt.Printf("Write gateway pid failed: %v\n", err)
		return 1
	}
	_ = cmd.Process.Release()

	time.Sleep(350 * time.Millisecond)
	if !processAlive(pid) {
		_ = os.Remove(gatewayPIDPath(configPath))
		fmt.Printf("Gateway exited during startup. Check log: %s\n", logPath)
		return 1
	}
	fmt.Println("✓ Gateway started in background")
	fmt.Println("  PID:", pid)
	fmt.Println("  Listen:", cfg.ListenAddr)
	fmt.Println("  Log:", logPath)
	return 0
}

func handleStop(configPath string) int {
	cfg, cfgErr := LoadConfig(configPath)
	pids := make(map[int]struct{})
	if pid, ok := readGatewayPID(configPath); ok {
		pids[pid] = struct{}{}
	}
	if cfgErr == nil {
		for _, pid := range listenerPIDs(cfg.ListenAddr) {
			if processLooksLikeGateway(pid) {
				pids[pid] = struct{}{}
			}
		}
	}
	if len(pids) == 0 {
		_ = os.Remove(gatewayPIDPath(configPath))
		fmt.Println("✓ Gateway not running")
		return 0
	}

	var failed []string
	var stopped int
	for pid := range pids {
		if pid <= 0 || pid == os.Getpid() {
			continue
		}
		if !processAlive(pid) {
			stopped++
			continue
		}
		if err := terminatePID(pid); err != nil {
			failed = append(failed, fmt.Sprintf("%d: %v", pid, err))
			continue
		}
		stopped++
	}
	_ = os.Remove(gatewayPIDPath(configPath))
	if len(failed) > 0 {
		fmt.Println("Stop gateway failed:", strings.Join(failed, "; "))
		return 1
	}
	if stopped == 0 {
		fmt.Println("✓ Gateway not running")
	} else {
		fmt.Printf("✓ Gateway stopped (%d process%s)\n", stopped, pluralS(stopped))
	}
	return 0
}

func readGatewayPID(configPath string) (int, bool) {
	data, err := os.ReadFile(gatewayPIDPath(configPath))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

func gatewayTCPReachable(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func terminatePID(pid int) error {
	if pid <= 0 || pid == os.Getpid() {
		return nil
	}
	if !processAlive(pid) {
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		return err
	}
	for i := 0; i < 20; i++ {
		if !processAlive(pid) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return err
	}
	for i := 0; i < 20; i++ {
		if !processAlive(pid) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("pid still alive after SIGKILL")
}

func listenerPIDs(addr string) []int {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || strings.TrimSpace(port) == "" {
		return nil
	}
	pids := pidsFromProcNet(port)
	if len(pids) > 0 {
		return pids
	}
	return pidsFromExternalPortTools(port)
}

func pidsFromProcNet(port string) []int {
	portNum, err := strconv.Atoi(port)
	if err != nil || portNum <= 0 || portNum > 65535 {
		return nil
	}
	wantHex := strings.ToUpper(fmt.Sprintf("%04X", portNum))
	inodes := map[string]struct{}{}
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		lines := strings.Split(string(data), "\n")
		for _, line := range lines[1:] {
			fields := strings.Fields(line)
			if len(fields) < 10 || fields[3] != "0A" {
				continue
			}
			local := fields[1]
			colon := strings.LastIndex(local, ":")
			if colon < 0 || strings.ToUpper(local[colon+1:]) != wantHex {
				continue
			}
			inodes[fields[9]] = struct{}{}
		}
	}
	if len(inodes) == 0 {
		return nil
	}
	return pidsForSocketInodes(inodes)
}

func pidsForSocketInodes(inodes map[string]struct{}) []int {
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	pids := map[int]struct{}{}
	for _, proc := range procs {
		if !proc.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(proc.Name())
		if err != nil || pid <= 0 {
			continue
		}
		fds, err := os.ReadDir(filepath.Join("/proc", proc.Name(), "fd"))
		if err != nil {
			continue
		}
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join("/proc", proc.Name(), "fd", fd.Name()))
			if err != nil || !strings.HasPrefix(link, "socket:[") || !strings.HasSuffix(link, "]") {
				continue
			}
			inode := strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]")
			if _, ok := inodes[inode]; ok {
				pids[pid] = struct{}{}
			}
		}
	}
	out := make([]int, 0, len(pids))
	for pid := range pids {
		out = append(out, pid)
	}
	return out
}

func pidsFromExternalPortTools(port string) []int {
	pids := map[int]struct{}{}
	if path, err := exec.LookPath("lsof"); err == nil {
		out, err := exec.Command(path, "-nP", "-iTCP:"+port, "-sTCP:LISTEN", "-t").Output()
		if err == nil {
			addPIDs(pids, string(out))
		}
	}
	if len(pids) == 0 {
		if path, err := exec.LookPath("fuser"); err == nil {
			out, err := exec.Command(path, "-n", "tcp", port).CombinedOutput()
			if err == nil {
				addPIDs(pids, string(out))
			}
		}
	}
	out := make([]int, 0, len(pids))
	for pid := range pids {
		out = append(out, pid)
	}
	return out
}

func processLooksLikeGateway(pid int) bool {
	exe, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	if err == nil && strings.Contains(strings.ToLower(filepath.Base(exe)), "gateway") {
		return true
	}
	cmdline, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false
	}
	for _, part := range strings.Split(string(cmdline), "\x00") {
		if strings.Contains(strings.ToLower(filepath.Base(part)), "gateway") {
			return true
		}
	}
	return false
}

func addPIDs(dst map[int]struct{}, raw string) {
	for _, field := range strings.Fields(raw) {
		field = strings.Trim(field, ":")
		pid, err := strconv.Atoi(field)
		if err == nil && pid > 0 {
			dst[pid] = struct{}{}
		}
	}
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "es"
}
