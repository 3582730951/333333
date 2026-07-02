package sentinel

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestNewClientDefaultsHTTPTimeout(t *testing.T) {
	c := NewClient(nil, "ua", "device", "session")
	if c.httpClient == nil {
		t.Fatal("http client is nil")
	}
	if c.httpClient.Timeout != sentinelHTTPTimeout {
		t.Fatalf("timeout = %s, want %s", c.httpClient.Timeout, sentinelHTTPTimeout)
	}
}

func TestClientGetReportsStatusWithLimitedBody(t *testing.T) {
	largeBody := strings.Repeat("x", sentinelResponseBodyLimit*4)
	c := NewClient(&http.Client{
		Transport: sentinelRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Header.Get("Origin") != "https://sentinel.openai.com" {
				t.Fatalf("origin = %q", req.Header.Get("Origin"))
			}
			return &http.Response{
				StatusCode: http.StatusBadGateway,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(largeBody)),
				Request:    req,
			}, nil
		}),
	}, "ua", "device", "session")

	_, err := c.Get(context.Background(), FlowPasswordVerify)
	if err == nil {
		t.Fatal("Get returned nil error for non-2xx response")
	}
	msg := err.Error()
	if !strings.Contains(msg, "sentinel status 502") {
		t.Fatalf("error = %q, want status context", msg)
	}
	if strings.Count(msg, "x") != sentinelLogSnippetLimit {
		t.Fatalf("error body length = %d, want %d", strings.Count(msg, "x"), sentinelLogSnippetLimit)
	}
}

func TestClientGetBuildsTokens(t *testing.T) {
	c := NewClient(&http.Client{
		Transport: sentinelRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"token":"tok","so":"so-value"}`)),
				Request:    req,
			}, nil
		}),
	}, "ua", "device", "session")

	token, err := c.Get(context.Background(), FlowUsernamePasswordCreate)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(token.MainToken, `"c":"tok"`) || !strings.Contains(token.SOToken, `"so":"so-value"`) {
		t.Fatalf("unexpected token payloads: main=%s so=%s", token.MainToken, token.SOToken)
	}
}

type sentinelRoundTripFunc func(*http.Request) (*http.Response, error)

func (f sentinelRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
