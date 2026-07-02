package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDownloadGatewayServesPrebuiltBinaryWithoutGoToolchain(t *testing.T) {
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(oldWD); err != nil {
			t.Fatalf("restore working directory: %v", err)
		}
	})

	binDir := filepath.Join(tmp, "prebuilt")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := []byte("fake gateway binary")
	if err := os.WriteFile(filepath.Join(binDir, "gateway-linux-amd64"), want, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GATEWAY_BIN_DIR", binDir)
	t.Setenv("PATH", "")

	app := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/download/gateway?os=linux&arch=amd64", nil)
	rec := httptest.NewRecorder()
	app.handleDownloadGateway(rec, req)

	resp := rec.Result()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "gateway-linux-amd64") {
		t.Fatalf("content disposition = %q, want linux/amd64 filename", cd)
	}
}

func TestSetupScriptDownloadsGatewayForDetectedPlatform(t *testing.T) {
	script := buildCodexConfigScript("https://pool.example/", "cap_abc123", "gpt-5.5", "", "", "")
	claudeInstall := scriptBetween(t, script, "install_gateway_binary() {", "\n}\n\nconfigure_claude()")
	if !strings.Contains(claudeInstall, `"$ORIGIN/download/gateway?os=$os&arch=$arch"`) {
		t.Fatalf("gateway installer should pass detected platform to download endpoint\n---\n%s", claudeInstall)
	}
}
