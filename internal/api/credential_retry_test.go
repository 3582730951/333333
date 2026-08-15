package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/upstream"
)

type retryTrackingBody struct {
	io.Reader
	closed bool
}

func (b *retryTrackingBody) Close() error { b.closed = true; return nil }

func TestAccountCredentialRetryIsOptInReplaySafeAndBounded(t *testing.T) {
	server := &Server{}
	account := storage.Account{RetryMaxAttempts: 3}
	attempts := 0
	var intermediate []*retryTrackingBody
	resp, err, used := server.doAccountCredentialRetry(context.Background(), account, true, func() (*upstream.Response, error) {
		attempts++
		if attempts < 3 {
			body := &retryTrackingBody{Reader: strings.NewReader("temporary")}
			intermediate = append(intermediate, body)
			return &upstream.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{}, Body: body}, nil
		}
		return &upstream.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	if err != nil || resp == nil || resp.StatusCode != http.StatusOK || used != 3 || attempts != 3 {
		t.Fatalf("retry result resp=%+v err=%v used=%d attempts=%d", resp, err, used, attempts)
	}
	for _, body := range intermediate {
		if !body.closed {
			t.Fatal("intermediate retry body was not closed")
		}
	}

	attempts = 0
	_, _, used = server.doAccountCredentialRetry(context.Background(), account, false, func() (*upstream.Response, error) {
		attempts++
		return nil, errors.New("ambiguous delivery")
	})
	if used != 1 || attempts != 1 {
		t.Fatalf("unsafe request retried: used=%d attempts=%d", used, attempts)
	}

	attempts = 0
	_, _, used = server.doAccountCredentialRetry(context.Background(), storage.Account{}, true, func() (*upstream.Response, error) {
		attempts++
		return &upstream.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("temporary"))}, nil
	})
	if used != 1 || attempts != 1 {
		t.Fatalf("default account policy retried: used=%d attempts=%d", used, attempts)
	}

	attempts = 0
	_, _, used = server.doAccountCredentialRetry(context.Background(), account, true, func() (*upstream.Response, error) {
		attempts++
		return &upstream.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("quota"))}, nil
	})
	if used != 1 || attempts != 1 {
		t.Fatalf("rate limit was replayed: used=%d attempts=%d", used, attempts)
	}
}
