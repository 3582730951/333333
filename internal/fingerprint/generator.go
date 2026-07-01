package fingerprint

import (
	"fmt"
	"math/rand"
	"time"
)

// FingerprintProfile represents a browser fingerprint
type FingerprintProfile struct {
	UserAgent    string            `json:"user_agent"`
	Platform     string            `json:"platform"`
	Language     string            `json:"language"`
	Resolution   string            `json:"resolution"`
	Timezone     string            `json:"timezone"`
	WebGL        string            `json:"webgl"`
	Canvas       string            `json:"canvas"`
	AudioContext string            `json:"audio_context"`
	Headers      map[string]string `json:"headers"`
}

// FingerprintMode defines when to generate new fingerprints
type FingerprintMode string

const (
	FingerprintModeNone       FingerprintMode = "none"        // No fingerprint
	FingerprintModePerAccount FingerprintMode = "per_account" // Each account gets unique fingerprint
	FingerprintModePerTask    FingerprintMode = "per_task"    // All accounts in task share fingerprint
)

// Generator creates browser fingerprints
type Generator struct {
	seed int64
	rng  *rand.Rand
}

// NewGenerator creates a new fingerprint generator with the given seed
func NewGenerator(seed int64) *Generator {
	return &Generator{
		seed: seed,
		rng:  rand.New(rand.NewSource(seed)),
	}
}

// NewGeneratorFromTime creates a generator using current timestamp
func NewGeneratorFromTime() *Generator {
	return NewGenerator(time.Now().UnixNano())
}

// Generate creates a complete browser fingerprint
func (g *Generator) Generate() *FingerprintProfile {
	userAgent := g.selectUserAgent()

	return &FingerprintProfile{
		UserAgent:    userAgent,
		Platform:     g.selectPlatform(userAgent),
		Language:     g.selectLanguage(),
		Resolution:   g.selectResolution(),
		Timezone:     g.selectTimezone(),
		WebGL:        g.generateWebGL(),
		Canvas:       g.generateCanvas(),
		AudioContext: g.generateAudioContext(),
		Headers:      g.generateHeaders(userAgent),
	}
}

// selectUserAgent picks a random user agent
func (g *Generator) selectUserAgent() string {
	userAgents := []string{
		// Chrome on Windows
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/121.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36",

		// Chrome on Mac
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/121.0.0.0 Safari/537.36",

		// Chrome on Linux
		"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
		"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/121.0.0.0 Safari/537.36",

		// Edge on Windows
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36 Edg/120.0.0.0",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/121.0.0.0 Safari/537.36 Edg/121.0.0.0",
	}

	return userAgents[g.rng.Intn(len(userAgents))]
}

// selectPlatform determines platform based on user agent
func (g *Generator) selectPlatform(userAgent string) string {
	if contains(userAgent, "Windows") {
		return "Win32"
	} else if contains(userAgent, "Macintosh") {
		return "MacIntel"
	} else if contains(userAgent, "Linux") {
		return "Linux x86_64"
	}
	return "Win32"
}

// selectLanguage picks a random language
func (g *Generator) selectLanguage() string {
	languages := []string{
		"en-US,en;q=0.9",
		"en-GB,en;q=0.9",
		"en-US,en;q=0.9,zh-CN;q=0.8,zh;q=0.7",
		"zh-CN,zh;q=0.9,en;q=0.8",
	}
	return languages[g.rng.Intn(len(languages))]
}

// selectResolution picks a common screen resolution
func (g *Generator) selectResolution() string {
	resolutions := []string{
		"1920x1080",
		"2560x1440",
		"1366x768",
		"1536x864",
		"1440x900",
		"3840x2160",
	}
	return resolutions[g.rng.Intn(len(resolutions))]
}

// selectTimezone picks a timezone
func (g *Generator) selectTimezone() string {
	timezones := []string{
		"America/New_York",
		"America/Los_Angeles",
		"America/Chicago",
		"Europe/London",
		"Asia/Shanghai",
		"Asia/Tokyo",
	}
	return timezones[g.rng.Intn(len(timezones))]
}

// generateWebGL generates WebGL renderer string
func (g *Generator) generateWebGL() string {
	renderers := []string{
		"ANGLE (NVIDIA GeForce GTX 1060 Direct3D11 vs_5_0 ps_5_0)",
		"ANGLE (Intel(R) UHD Graphics 630 Direct3D11 vs_5_0 ps_5_0)",
		"ANGLE (AMD Radeon RX 580 Series Direct3D11 vs_5_0 ps_5_0)",
		"Apple M1",
		"Apple M2",
	}
	return renderers[g.rng.Intn(len(renderers))]
}

// generateCanvas generates canvas fingerprint hash
func (g *Generator) generateCanvas() string {
	// In real implementation, this would be a hash of canvas rendering
	// For now, generate a random hex string
	return fmt.Sprintf("%016x", g.rng.Int63())
}

// generateAudioContext generates audio context fingerprint
func (g *Generator) generateAudioContext() string {
	// Similar to canvas, generate random fingerprint
	return fmt.Sprintf("%016x", g.rng.Int63())
}

// generateHeaders generates HTTP headers matching the fingerprint
func (g *Generator) generateHeaders(userAgent string) map[string]string {
	headers := map[string]string{
		"User-Agent":                userAgent,
		"Accept":                    "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8",
		"Accept-Language":           g.selectLanguage(),
		"Accept-Encoding":           "gzip, deflate, br",
		"Connection":                "keep-alive",
		"Upgrade-Insecure-Requests": "1",
		"Sec-Fetch-Dest":            "document",
		"Sec-Fetch-Mode":            "navigate",
		"Sec-Fetch-Site":            "none",
		"Sec-Fetch-User":            "?1",
		"Cache-Control":             "max-age=0",
	}

	// Add sec-ch-ua headers for Chromium-based browsers
	if contains(userAgent, "Chrome") {
		headers["sec-ch-ua"] = `"Not A(Brand";v="99", "Google Chrome";v="121", "Chromium";v="121"`
		headers["sec-ch-ua-mobile"] = "?0"
		headers["sec-ch-ua-platform"] = `"Windows"`
	}

	return headers
}

// contains checks if a string contains a substring
func contains(s, substr string) bool {
	return len(s) >= len(substr) &&
		(s == substr ||
			(len(s) > len(substr) &&
				indexOf(s, substr) >= 0))
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
