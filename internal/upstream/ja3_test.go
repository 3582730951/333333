package upstream

import (
	"strings"
	"testing"

	"codex-account-pool/internal/identity"
)

// TestSanitizeJA3 pins the SCSV-stripping behavior: the 0xFF (255) and 0x5600 (22016)
// signalling pseudo-ciphers must be removed from the cipher field (so curl_cffi/BoringSSL
// don't reject the JA3 with "Cipher 0xff is not found"), every other field untouched, and
// malformed input returned verbatim.
func TestSanitizeJA3(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"strips trailing 0xFF SCSV", "771,4866-4865-255,43-0-10,29-23-24,0", "771,4866-4865,43-0-10,29-23-24,0"},
		{"strips leading 0xFF SCSV", "771,255-4866-4865,43,29,0", "771,4866-4865,43,29,0"},
		{"strips 0x5600 fallback SCSV", "771,22016-4865,43,29,0", "771,4865,43,29,0"},
		{"strips both SCSVs", "771,255-4865-22016-4866,43,29,0", "771,4865-4866,43,29,0"},
		{"does not touch the extensions field", "771,4865,0-23-65281-255-10,29,0", "771,4865,0-23-65281-255-10,29,0"},
		{"no-op when no SCSV present", "771,4865-4866-4867,0-23-10,29-23-24,0", "771,4865-4866-4867,0-23-10,29-23-24,0"},
		{"empty input", "", ""},
		{"only version (malformed)", "771", "771"},
		{"empty cipher field", "771,,43,29,0", "771,,43,29,0"},
	}
	for _, c := range cases {
		if got := sanitizeJA3(c.in); got != c.want {
			t.Errorf("%s: sanitizeJA3(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}

	// The real Codex JA3 (vanilla rustls) must lose its 0xFF SCSV cipher...
	codex := sanitizeJA3(identity.CodexJA3)
	if strings.Contains(codex, "-255,") || strings.HasSuffix(codex, "-255") || strings.Contains(codex, ",255-") {
		t.Errorf("sanitized Codex JA3 still lists the 0xFF SCSV cipher 255: %q", codex)
	}
	// ...while the real Claude JA3 (no SCSV in its cipher list) is unchanged: the 65281
	// renegotiation_info lives in the EXTENSIONS field, which sanitize never touches.
	if got := sanitizeJA3(identity.ClaudeJA3); got != identity.ClaudeJA3 {
		t.Errorf("sanitizeJA3 must be a no-op for Claude JA3:\n got  %q\n want %q", got, identity.ClaudeJA3)
	}
}
