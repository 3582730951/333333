package fingerprint

import (
	"testing"
)

func TestGeneratorDeterministic(t *testing.T) {
	// Same seed should produce same fingerprint
	seed := int64(12345)

	gen1 := NewGenerator(seed)
	fp1 := gen1.Generate()

	gen2 := NewGenerator(seed)
	fp2 := gen2.Generate()

	if fp1.UserAgent != fp2.UserAgent {
		t.Errorf("Same seed produced different user agents: %s vs %s", fp1.UserAgent, fp2.UserAgent)
	}

	if fp1.Platform != fp2.Platform {
		t.Errorf("Same seed produced different platforms: %s vs %s", fp1.Platform, fp2.Platform)
	}

	t.Logf("✓ Deterministic generation works")
}

func TestGeneratorDifferent(t *testing.T) {
	// Different seeds should produce different fingerprints
	gen1 := NewGenerator(1)
	fp1 := gen1.Generate()

	gen2 := NewGenerator(2)
	fp2 := gen2.Generate()

	// At least one field should be different
	if fp1.UserAgent == fp2.UserAgent &&
		fp1.Resolution == fp2.Resolution &&
		fp1.Timezone == fp2.Timezone &&
		fp1.Canvas == fp2.Canvas {
		t.Error("Different seeds produced identical fingerprints")
	}

	t.Logf("✓ Different seeds produce different fingerprints")
}

func TestFingerprintFields(t *testing.T) {
	gen := NewGeneratorFromTime()
	fp := gen.Generate()

	// Check all required fields are populated
	if fp.UserAgent == "" {
		t.Error("UserAgent is empty")
	}
	if fp.Platform == "" {
		t.Error("Platform is empty")
	}
	if fp.Language == "" {
		t.Error("Language is empty")
	}
	if fp.Resolution == "" {
		t.Error("Resolution is empty")
	}
	if fp.Timezone == "" {
		t.Error("Timezone is empty")
	}
	if fp.WebGL == "" {
		t.Error("WebGL is empty")
	}
	if fp.Canvas == "" {
		t.Error("Canvas is empty")
	}
	if fp.AudioContext == "" {
		t.Error("AudioContext is empty")
	}
	if len(fp.Headers) == 0 {
		t.Error("Headers are empty")
	}

	t.Logf("✓ All fingerprint fields populated")
	t.Logf("  UserAgent: %s", fp.UserAgent)
	t.Logf("  Platform: %s", fp.Platform)
	t.Logf("  Resolution: %s", fp.Resolution)
	t.Logf("  Timezone: %s", fp.Timezone)
}

func TestPlatformConsistency(t *testing.T) {
	// Platform should match user agent
	gen := NewGenerator(123)
	fp := gen.Generate()

	if contains(fp.UserAgent, "Windows") && fp.Platform != "Win32" {
		t.Errorf("Windows UA should have Win32 platform, got: %s", fp.Platform)
	}
	if contains(fp.UserAgent, "Macintosh") && fp.Platform != "MacIntel" {
		t.Errorf("Mac UA should have MacIntel platform, got: %s", fp.Platform)
	}
	if contains(fp.UserAgent, "Linux") && fp.Platform != "Linux x86_64" {
		t.Errorf("Linux UA should have Linux x86_64 platform, got: %s", fp.Platform)
	}

	t.Logf("✓ Platform consistency validated")
}

func TestHeadersMatch(t *testing.T) {
	gen := NewGenerator(456)
	fp := gen.Generate()

	// Headers should include user agent
	if fp.Headers["User-Agent"] != fp.UserAgent {
		t.Error("Headers User-Agent doesn't match fingerprint UserAgent")
	}

	// Should have required headers
	requiredHeaders := []string{
		"User-Agent",
		"Accept",
		"Accept-Language",
		"Accept-Encoding",
	}

	for _, h := range requiredHeaders {
		if fp.Headers[h] == "" {
			t.Errorf("Missing required header: %s", h)
		}
	}

	t.Logf("✓ Headers validated (%d total)", len(fp.Headers))
}

func BenchmarkGenerate(b *testing.B) {
	gen := NewGenerator(789)
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		gen.Generate()
	}
}
