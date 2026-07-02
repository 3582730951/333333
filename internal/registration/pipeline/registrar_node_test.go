package pipeline

import "testing"

func TestBuildNodeJobConfigIsolation(t *testing.T) {
	base := map[string]interface{}{
		"heroSmsApiKey": "k",
		"mailDomain":    "ex.com",
		"proxyHost":     "leftover", // must be overridden by the per-job egress
	}
	job := nodeJob{
		ProxyURL:        "http://u:p@1.2.3.4:3128",
		ProfileDir:      "/tmp/job/profile",
		TokenDir:        "/tmp/job/tokens",
		FingerprintSeed: "seed-abc",
	}
	cfg := buildNodeJobConfig(base, job)

	// Base config preserved.
	if cfg["heroSmsApiKey"] != "k" || cfg["mailDomain"] != "ex.com" {
		t.Fatalf("base config not preserved: %v", cfg)
	}
	// Per-job egress overrides the proxy → unique outbound IP.
	if cfg["proxyHost"] != "1.2.3.4" || cfg["proxyPort"] != 3128 || cfg["proxyUsername"] != "u" || cfg["proxyPassword"] != "p" {
		t.Fatalf("proxy override wrong: host=%v port=%v user=%v pass=%v", cfg["proxyHost"], cfg["proxyPort"], cfg["proxyUsername"], cfg["proxyPassword"])
	}
	// Throwaway profile + cookie purge invariants.
	if cfg["browserUserDataDir"] != "/tmp/job/profile" {
		t.Fatalf("profile dir = %v", cfg["browserUserDataDir"])
	}
	if cfg["browserClearChatGptSession"] != true {
		t.Fatalf("session clear must be on for cookie purge")
	}
	if cfg["tokenOutputDir"] != "/tmp/job/tokens" {
		t.Fatalf("token dir = %v", cfg["tokenOutputDir"])
	}
	// Fingerprint isolation.
	if cfg["fingerprintSeed"] != "seed-abc" {
		t.Fatalf("fingerprint seed = %v", cfg["fingerprintSeed"])
	}
}

func TestSplitProxyURL(t *testing.T) {
	host, port, user, pass, ok := splitProxyURL("http://user:pw@host.example:8080")
	if !ok || host != "host.example" || port != 8080 || user != "user" || pass != "pw" {
		t.Fatalf("got host=%q port=%d user=%q pass=%q ok=%v", host, port, user, pass, ok)
	}
	if _, _, _, _, ok := splitProxyURL(""); ok {
		t.Fatalf("empty URL must return ok=false")
	}
	// No-auth proxy still parses.
	if h, p, _, _, ok := splitProxyURL("socks5://1.1.1.1:1080"); !ok || h != "1.1.1.1" || p != 1080 {
		t.Fatalf("no-auth parse wrong: %q %d %v", h, p, ok)
	}
}
