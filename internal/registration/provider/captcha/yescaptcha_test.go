package captcha

import (
	"strings"
	"testing"
)

func TestNewYesCaptchaProviderDefaultsHTTPClient(t *testing.T) {
	p := NewYesCaptchaProvider("key", nil)
	if p.httpClient == nil {
		t.Fatal("http client is nil")
	}
}

func TestReadCaptchaProviderBodyIsLimited(t *testing.T) {
	body := readCaptchaProviderBody(strings.NewReader(strings.Repeat("x", captchaProviderResponseBodyLimit*2)))
	if len(body) != captchaProviderResponseBodyLimit {
		t.Fatalf("body length = %d, want %d", len(body), captchaProviderResponseBodyLimit)
	}
}
