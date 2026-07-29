package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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

func TestGatewayInstallScriptRestartsManagedGateway(t *testing.T) {
	app := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/install-gateway.sh?key=cap_abc123", nil)
	req.Host = "pool.example"
	rec := httptest.NewRecorder()
	app.handleGatewayInstallScript(rec, req)

	resp := rec.Result()
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	script := string(body)
	for _, want := range []string{
		`gateway stop || true`,
		`"$GATEWAY_BIN" stop`,
		`"$GATEWAY_BIN" start-background`,
		`if [ "$(id -u)" = "0" ]`,
		`command -v sudo`,
		`GATEWAY_BIN="$HOME/.local/bin/gateway"`,
		"gateway.pid",
		"gateway.log",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("install-gateway script should manage background gateway; missing %q\n---\n%s", want, script)
		}
	}
	if strings.Contains(script, `$GATEWAY_BIN start &`) {
		t.Fatalf("install-gateway script should not tell users to manually background start\n---\n%s", script)
	}
}

func TestGatewayInstallScriptQuotesOriginAndKey(t *testing.T) {
	app := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/install-gateway.sh?key=cap_a%27b%24%28touch%20BAD%29", nil)
	req.Host = "pool.example"
	req.Header.Set("X-Forwarded-Host", "attacker.example")
	rec := httptest.NewRecorder()
	app.handleGatewayInstallScript(rec, req)

	script := rec.Body.String()
	if !strings.Contains(script, `POOL_URL='http://pool.example'`) {
		t.Fatalf("installer did not use validated request origin\n---\n%s", script)
	}
	if strings.Contains(script, "attacker.example") {
		t.Fatalf("installer trusted unconfigured forwarded host\n---\n%s", script)
	}
	if !strings.Contains(script, `API_KEY='cap_a'"'"'b$(touch BAD)'`) {
		t.Fatalf("installer did not shell-quote API key\n---\n%s", script)
	}
	p := filepath.Join(t.TempDir(), "install-gateway.sh")
	if err := os.WriteFile(p, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("bash", "-n", p).CombinedOutput(); err != nil {
		t.Fatalf("installer shell syntax: %v\n%s", err, out)
	}
}
