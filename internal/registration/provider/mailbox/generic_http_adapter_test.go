package mailbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGenericHTTPAdapterKeepsConcurrentMailboxStateIsolated(t *testing.T) {
	var created atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/create":
			index := created.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"email":      fmt.Sprintf("child-%d@example.test", index),
				"account_id": fmt.Sprintf("account-%d", index),
			})
		case r.Method == http.MethodGet && r.URL.Path == "/mail/account-1":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"messages": []map[string]string{{"id": "one", "body": "Verification code 111111"}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/mail/account-2":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"messages": []map[string]string{{"id": "two", "body": "Verification code 222222"}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	pipeline := map[string]interface{}{
		"email_mode": "generate",
		"create_email": map[string]interface{}{
			"method": "POST", "path": "/create",
			"extract": map[string]interface{}{"email": "email", "account_id": "account_id"},
		},
		"list_emails": map[string]interface{}{"method": "GET", "path": "/mail/{account_id}"},
		"response_list_path":  "messages",
		"response_id_field":   "id",
		"response_body_fields": "body",
	}
	adapter, err := NewGenericHTTPAdapter(
		pipeline,
		map[string]interface{}{"api_url": server.URL, "domain": "example.test"},
		"",
		"generic-fixture",
	)
	if err != nil {
		t.Fatal(err)
	}
	_, _, firstLease, err := adapter.CreateEmail(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, _, secondLease, err := adapter.CreateEmail(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	results := make(chan string, 2)
	for _, lease := range []string{firstLease, secondLease} {
		wg.Add(1)
		go func(mailboxID string) {
			defer wg.Done()
			code, waitErr := adapter.WaitOTP(context.Background(), mailboxID, 2*time.Second)
			if waitErr != nil {
				results <- "error:" + waitErr.Error()
				return
			}
			results <- code
		}(lease)
	}
	wg.Wait()
	close(results)
	seen := map[string]bool{}
	for result := range results {
		seen[result] = true
	}
	if !seen["111111"] || !seen["222222"] || len(seen) != 2 {
		t.Fatalf("concurrent mailbox results=%v", seen)
	}
}
