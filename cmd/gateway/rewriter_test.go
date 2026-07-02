package main

import (
	"strings"
	"testing"
	"time"
)

func TestRewriteBodyReplacesLocalRuntimeArtifactsButKeepsCWD(t *testing.T) {
	id := &CachedIdentity{
		Local: &LocalEnvironment{
			Username:   "realuser",
			Hostname:   "real-host",
			HomeDir:    "/home/realuser",
			WorkDir:    "/workspace/project",
			DNSServers: []string{"10.0.0.53", "10.0.0.54"},
		},
		Virtual: &VirtualIdentity{
			Username:   "virtuser",
			Hostname:   "virt-host",
			HomeDir:    "/home/virtuser",
			OSName:     "Linux",
			OSRelease:  "6.8.0",
			Arch:       "x86_64",
			Terminal:   "xterm-256color",
			DNSServers: []string{"1.1.1.1", "1.0.0.1"},
			ProcessInfo: ProcessInfo{
				CWD: "/home/virtuser/workspace/project",
			},
		},
		FetchedAt: time.Now(),
	}
	body := []byte(`{"metadata":{"user_id":"real"},"messages":[{"role":"user","content":"user realuser host real-host home /home/realuser cwd /workspace/project dns 10.0.0.53 10.0.0.54\n<env>\nPlatform: darwin\nOS Version: 23.0\nTerminal: Apple_Terminal\nHostname: real-host\nArchitecture: arm64\n</env>"}]}`)

	rewritten, err := rewriteBody(body, id)
	if err != nil {
		t.Fatal(err)
	}
	got := string(rewritten)
	for _, forbidden := range []string{"realuser", "real-host", "/home/realuser", "10.0.0.53", "10.0.0.54", "darwin", "Apple_Terminal", "arm64"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("rewritten body still contains %q\n---\n%s", forbidden, got)
		}
	}
	for _, want := range []string{"virtuser", "virt-host", "/home/virtuser", "cwd /workspace/project", "1.1.1.1", "1.0.0.1", "Linux", "6.8.0", "xterm-256color", "x86_64"} {
		if !strings.Contains(got, want) {
			t.Fatalf("rewritten body missing %q\n---\n%s", want, got)
		}
	}
	if strings.Contains(got, "cwd /home/virtuser/workspace/project") {
		t.Fatalf("cwd must not be rewritten to the virtual workspace\n---\n%s", got)
	}
}
