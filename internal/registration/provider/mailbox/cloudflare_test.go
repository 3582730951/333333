package mailbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestCloudflareTempEmail exercises the provider against a mock cloudflare_temp_email
// worker: address creation returns a jwt, and the mail list yields a 6-digit code.
func TestCloudflareTempEmail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/new_address" && r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"jwt": "jwt123", "name": "tester"})
		case r.URL.Path == "/api/mails" && r.Method == http.MethodGet:
			if got := r.Header.Get("Authorization"); got != "Bearer jwt123" {
				t.Errorf("mails Authorization = %q, want Bearer jwt123", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"results": []map[string]interface{}{
					{"id": 1, "subject": "OpenAI", "raw": "Your verification code is 123456. Thanks."},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := NewCloudflareTempEmailProvider(srv.URL, "", "example.com", srv.Client())

	email, _, jwt, err := p.CreateEmail(context.Background())
	if err != nil {
		t.Fatalf("CreateEmail: %v", err)
	}
	if jwt != "jwt123" {
		t.Fatalf("jwt = %q, want jwt123", jwt)
	}
	if !strings.HasSuffix(email, "@example.com") {
		t.Fatalf("email = %q, want @example.com suffix", email)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	code, err := p.WaitOTP(ctx, jwt, 10*time.Second)
	if err != nil {
		t.Fatalf("WaitOTP: %v", err)
	}
	if code != "123456" {
		t.Fatalf("code = %q, want 123456", code)
	}
}

func TestGenericHTTPExecuteStepFallsBackToLimitedText(t *testing.T) {
	body := strings.Repeat("x", mailboxHTTPResponseBodyLimit*2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	g, err := NewGenericHTTP(map[string]interface{}{}, map[string]interface{}{"api_url": srv.URL}, "")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := g.executeStep(context.Background(), map[string]interface{}{
		"method": "GET",
		"path":   "/plain",
	})
	if err != nil {
		t.Fatal(err)
	}
	m, ok := resp.(map[string]interface{})
	if !ok {
		t.Fatalf("response = %T, want map", resp)
	}
	text, _ := m["_text"].(string)
	if len(text) != mailboxHTTPResponseBodyLimit {
		t.Fatalf("fallback text length = %d, want %d", len(text), mailboxHTTPResponseBodyLimit)
	}
}
