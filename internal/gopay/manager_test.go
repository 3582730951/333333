package gopay

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

func TestWriteConfigRenderAndRedact(t *testing.T) {
	store, err := storage.OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg := config.Default()
	cfg.GopayDir = dir
	cfg.GopayAutoStart = false // Start() only renders config, never spawns python
	m := NewManager(cfg, store)

	if _, err := m.SaveSettings(ctx, Settings{
		PhoneNumber: "8123456789", Pin: "123456", OTPMode: "sms_api",
		SMSAPIKey: "secret-key", SMSBaseURL: "https://sms.example",
		ProxyURL: "socks5://1.2.3.4:1080", OTPTimeout: 120,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatalf("start/writeConfig: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	var c map[string]interface{}
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("config.json invalid: %v", err)
	}
	if c["proxy"] != "socks5://1.2.3.4:1080" {
		t.Fatalf("proxy not rendered: %v", c["proxy"])
	}
	if otp := c["otp"].(map[string]interface{}); otp["mode"] != "sms_api" {
		t.Fatalf("otp.mode = %v", otp["mode"])
	}

	// Stored value is real; Redacted() masks it.
	if got := m.GetSettings(ctx); got.Pin != "123456" {
		t.Fatalf("stored pin lost: %q", got.Pin)
	}
	if got := m.GetSettings(ctx).Redacted(); got.Pin == "123456" || got.SMSAPIKey == "secret-key" {
		t.Fatalf("secrets not redacted: %+v", got)
	}

	// Saving with a masked pin must NOT overwrite the real one.
	if _, err := m.SaveSettings(ctx, Settings{Pin: "******", PhoneNumber: "8999"}); err != nil {
		t.Fatalf("save2: %v", err)
	}
	if got := m.GetSettings(ctx); got.Pin != "123456" || got.PhoneNumber != "8999" {
		t.Fatalf("masked-save clobbered pin or missed phone: %+v", got)
	}
}

func TestSpawnWatchesProcessExitAndClearsRunning(t *testing.T) {
	cfg := config.Default()
	cfg.GopayAutoStart = false
	m := NewManager(cfg, nil)

	cmd := exec.Command(os.Args[0], "-test.run=TestGopayHelperProcess", "--")
	cmd.Env = append(os.Environ(), "GO_WANT_GOPAY_HELPER=1")
	m.mu.Lock()
	m.procs = []*exec.Cmd{cmd}
	m.running = true
	m.mu.Unlock()

	if err := m.spawn(cmd); err != nil {
		t.Fatalf("spawn helper: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !m.Running() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if m.Running() {
		t.Fatal("manager still running after watched subprocess exited")
	}

	m.mu.Lock()
	logs := strings.Join(m.logs, "\n")
	m.mu.Unlock()
	if !strings.Contains(logs, "gopay helper output") {
		t.Fatalf("process stdout was not captured: %q", logs)
	}
	if !strings.Contains(logs, "exited") || !strings.Contains(logs, "exit status 42") {
		t.Fatalf("process exit was not logged with status: %q", logs)
	}
}

func TestGopayHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_GOPAY_HELPER") != "1" {
		return
	}
	fmt.Println("gopay helper output")
	os.Exit(42)
}
