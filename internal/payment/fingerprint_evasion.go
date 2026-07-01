package payment

import (
	"fmt"
	"math/rand"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// FingerprintEvasion applies advanced anti-detection techniques to a rod.Page.
// This bypasses PayPal's bot detection by randomizing browser fingerprints:
//   - Canvas fingerprint noise injection
//   - WebGL renderer/vendor randomization
//   - Audio context fingerprint spoofing
//   - Navigator properties override (platform, hardwareConcurrency, etc.)
//   - WebRTC IP leak prevention
type FingerprintEvasion struct {
	page *rod.Page
	seed int64 // for deterministic randomization
}

// NewFingerprintEvasion creates an evasion layer for the given page.
func NewFingerprintEvasion(page *rod.Page) *FingerprintEvasion {
	return &FingerprintEvasion{
		page: page,
		seed: time.Now().UnixNano(),
	}
}

// Apply injects all fingerprint evasion scripts before page navigation.
func (f *FingerprintEvasion) Apply() error {
	// Must be called before navigation (onNewDocument)
	scripts := []string{
		f.canvasNoiseScript(),
		f.webglNoiseScript(),
		f.audioContextScript(),
		f.navigatorOverrideScript(),
		f.webrtcLeakScript(),
		f.pluginArrayScript(),
	}

	for _, script := range scripts {
		_, err := f.page.EvalOnNewDocument(script)
		if err != nil {
			return fmt.Errorf("inject evasion script: %w", err)
		}
	}

	return nil
}

// canvasNoiseScript injects random noise into canvas.toDataURL to prevent fingerprinting.
// PayPal/Stripe may render hidden canvas elements and hash the output.
func (f *FingerprintEvasion) canvasNoiseScript() string {
	noise := f.random(0, 10) // random noise magnitude
	return fmt.Sprintf(`
(function() {
  const originalToDataURL = HTMLCanvasElement.prototype.toDataURL;
  const originalToBlob = HTMLCanvasElement.prototype.toBlob;
  const originalGetImageData = CanvasRenderingContext2D.prototype.getImageData;
  const noise = %d;

  // Inject noise into getImageData (most common fingerprinting vector)
  CanvasRenderingContext2D.prototype.getImageData = function(...args) {
    const imageData = originalGetImageData.apply(this, args);
    for (let i = 0; i < imageData.data.length; i += 4) {
      imageData.data[i] += Math.floor(Math.random() * noise);     // R
      imageData.data[i+1] += Math.floor(Math.random() * noise);   // G
      imageData.data[i+2] += Math.floor(Math.random() * noise);   // B
    }
    return imageData;
  };

  // Noise in toDataURL
  HTMLCanvasElement.prototype.toDataURL = function(...args) {
    const context = this.getContext('2d');
    const imageData = context.getImageData(0, 0, this.width, this.height);
    for (let i = 0; i < imageData.data.length; i += 4) {
      imageData.data[i] += Math.floor(Math.random() * noise);
    }
    context.putImageData(imageData, 0, 0);
    return originalToDataURL.apply(this, args);
  };
})();
`, noise)
}

// webglNoiseScript randomizes WebGL renderer and vendor strings.
func (f *FingerprintEvasion) webglNoiseScript() string {
	vendors := []string{
		"Google Inc. (NVIDIA)",
		"Google Inc. (Intel)",
		"Google Inc. (AMD)",
		"Google Inc. (Apple)",
	}
	renderers := []string{
		"ANGLE (NVIDIA, NVIDIA GeForce RTX 3080 Direct3D11 vs_5_0 ps_5_0)",
		"ANGLE (Intel, Intel(R) UHD Graphics 630 Direct3D11 vs_5_0 ps_5_0)",
		"ANGLE (AMD, AMD Radeon RX 6800 Direct3D11 vs_5_0 ps_5_0)",
		"Apple M1",
	}

	vendor := vendors[f.random(0, len(vendors))]
	renderer := renderers[f.random(0, len(renderers))]

	return fmt.Sprintf(`
(function() {
  const getParameter = WebGLRenderingContext.prototype.getParameter;
  WebGLRenderingContext.prototype.getParameter = function(param) {
    if (param === 37445) return '%s';  // UNMASKED_VENDOR_WEBGL
    if (param === 37446) return '%s';  // UNMASKED_RENDERER_WEBGL
    return getParameter.call(this, param);
  };
  const getParameter2 = WebGL2RenderingContext.prototype.getParameter;
  WebGL2RenderingContext.prototype.getParameter = function(param) {
    if (param === 37445) return '%s';
    if (param === 37446) return '%s';
    return getParameter2.call(this, param);
  };
})();
`, vendor, renderer, vendor, renderer)
}

// audioContextScript randomizes AudioContext fingerprint.
func (f *FingerprintEvasion) audioContextScript() string {
	noise := float64(f.random(1, 5)) * 0.0001 // very small noise
	return fmt.Sprintf(`
(function() {
  const AudioContext = window.AudioContext || window.webkitAudioContext;
  if (!AudioContext) return;

  const originalCreateOscillator = AudioContext.prototype.createOscillator;
  AudioContext.prototype.createOscillator = function() {
    const oscillator = originalCreateOscillator.call(this);
    const originalStart = oscillator.start;
    oscillator.start = function(...args) {
      oscillator.frequency.value += %f;
      return originalStart.apply(this, args);
    };
    return oscillator;
  };
})();
`, noise)
}

// navigatorOverrideScript randomizes navigator properties.
func (f *FingerprintEvasion) navigatorOverrideScript() string {
	platforms := []string{"Win32", "MacIntel", "Linux x86_64"}
	cores := []int{4, 8, 12, 16}
	memories := []int{4, 8, 16, 32}

	platform := platforms[f.random(0, len(platforms))]
	core := cores[f.random(0, len(cores))]
	memory := memories[f.random(0, len(memories))]

	return fmt.Sprintf(`
(function() {
  Object.defineProperty(navigator, 'platform', {get: () => '%s'});
  Object.defineProperty(navigator, 'hardwareConcurrency', {get: () => %d});
  Object.defineProperty(navigator, 'deviceMemory', {get: () => %d});

  // Remove automation signals
  delete navigator.__proto__.webdriver;
  Object.defineProperty(navigator, 'webdriver', {get: () => undefined});
})();
`, platform, core, memory)
}

// webrtcLeakScript prevents WebRTC IP leak (shows proxy IP instead of real IP).
func (f *FingerprintEvasion) webrtcLeakScript() string {
	return `
(function() {
  const originalRTCPeerConnection = window.RTCPeerConnection;
  window.RTCPeerConnection = function(...args) {
    const pc = new originalRTCPeerConnection(...args);
    const originalCreateOffer = pc.createOffer;
    pc.createOffer = function() {
      return Promise.reject(new Error('WebRTC disabled'));
    };
    return pc;
  };
})();
`
}

// pluginArrayScript randomizes navigator.plugins (fingerprinting vector).
func (f *FingerprintEvasion) pluginArrayScript() string {
	return `
(function() {
  const plugins = [
    {name: 'Chrome PDF Plugin', filename: 'internal-pdf-viewer'},
    {name: 'Chrome PDF Viewer', filename: 'mhjfbmdgcfjbbpaeojofohoefgiehjai'},
    {name: 'Native Client', filename: 'internal-nacl-plugin'},
  ];
  Object.defineProperty(navigator, 'plugins', {
    get: () => plugins
  });
})();
`
}

// random returns a random int in [min, max).
func (f *FingerprintEvasion) random(min, max int) int {
	r := rand.New(rand.NewSource(f.seed))
	f.seed++ // increment for next call
	return min + r.Intn(max-min)
}

// SetTimezone sets the browser timezone (avoid timezone fingerprinting).
func (f *FingerprintEvasion) SetTimezone(timezone string) error {
	return proto.EmulationSetTimezoneOverride{TimezoneID: timezone}.Call(f.page)
}

// SetLocale sets the browser locale (match billing country).
func (f *FingerprintEvasion) SetLocale(locale string) error {
	return proto.EmulationSetLocaleOverride{Locale: locale}.Call(f.page)
}
