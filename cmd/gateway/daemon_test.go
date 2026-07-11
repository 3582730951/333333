package main

import (
	"bytes"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestUsageMentionsManagedBackgroundLifecycle(t *testing.T) {
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	printUsage()
	_ = w.Close()
	os.Stdout = oldStdout
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	usage := buf.String()
	for _, want := range []string{"gateway start-background", "gateway stop"} {
		if !strings.Contains(usage, want) {
			t.Fatalf("usage should mention managed lifecycle command %q\n---\n%s", want, usage)
		}
	}
}

func TestGatewayManagedFilesLiveBesideConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if got := gatewayPIDPath(configPath); got != filepath.Join(dir, "gateway.pid") {
		t.Fatalf("pid path = %q", got)
	}
	if got := gatewayLogPath(configPath); got != filepath.Join(dir, "gateway.log") {
		t.Fatalf("log path = %q", got)
	}
}

func TestListenerPIDsFindsProcessWithoutPIDFile(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	pids := listenerPIDs(ln.Addr().String())
	for _, pid := range pids {
		if pid == os.Getpid() {
			return
		}
	}
	t.Fatalf("listenerPIDs(%q) = %v, want current pid %d", ln.Addr().String(), pids, os.Getpid())
}

func TestGatewayWindowsDownloadBinaryBuilds(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not available")
	}
	out := filepath.Join(t.TempDir(), "gateway-windows-amd64.exe")
	cmd := exec.Command(goBin, "build", "-buildvcs=false", "-trimpath", "-o", out, ".")
	cmd.Env = append(os.Environ(),
		"CGO_ENABLED=0",
		"GOOS=windows",
		"GOARCH=amd64",
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("windows gateway build failed: %v\n%s", err, output)
	}
}
