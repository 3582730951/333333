package payment

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestNewCheckoutURLGeneratorUsesTimeout(t *testing.T) {
	g := NewCheckoutURLGenerator("")
	if g.client == nil {
		t.Fatal("client is nil")
	}
	if g.client.Timeout != checkoutHTTPTimeout {
		t.Fatalf("client timeout = %s, want %s", g.client.Timeout, checkoutHTTPTimeout)
	}
}

func TestCheckoutErrorBodiesAreLimited(t *testing.T) {
	largeBody := strings.Repeat("x", checkoutErrorBodyLimit*4)
	g := &CheckoutURLGenerator{
		client: &http.Client{
			Timeout: time.Second,
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusBadGateway,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(largeBody)),
					Request:    req,
				}, nil
			}),
		},
	}

	_, _, err := g.step1OpenAI(context.Background(), CheckoutRequest{AccessToken: "token"})
	assertLimitedCheckoutError(t, err, "openai checkout")

	_, err = g.step2Stripe(context.Background(), "cs_test", "en")
	assertLimitedCheckoutError(t, err, "stripe init")
}

func assertLimitedCheckoutError(t *testing.T, err error, context string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s returned nil error", context)
	}
	msg := err.Error()
	if !strings.Contains(msg, context) || !strings.Contains(msg, "HTTP 502") {
		t.Fatalf("error = %q, want %s status context", msg, context)
	}
	if strings.Count(msg, "x") != checkoutErrorBodyLimit {
		t.Fatalf("error body length = %d, want %d", strings.Count(msg, "x"), checkoutErrorBodyLimit)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
