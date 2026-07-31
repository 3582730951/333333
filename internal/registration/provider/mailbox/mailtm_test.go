package mailbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMailTMProviderContract(t *testing.T) {
	var createdAddress string
	var deleted atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/domains":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"hydra:member": []map[string]interface{}{
					{"domain": "disabled.test", "isActive": false},
					{"domain": "mail.example.test", "isActive": true},
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/accounts":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			createdAddress = body["address"]
			if body["password"] == "" {
				t.Error("account password is empty")
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "account-1"})
		case r.Method == http.MethodPost && r.URL.Path == "/token":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "mail-token"})
		case r.Method == http.MethodGet && r.URL.Path == "/messages":
			if r.Header.Get("Authorization") != "Bearer mail-token" {
				t.Errorf("messages authorization=%q", r.Header.Get("Authorization"))
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"hydra:member": []map[string]interface{}{
					{"id": "message-1", "subject": "Verify your address"},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/messages/message-1":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"text": "Your verification code is 246810.",
			})
		case r.Method == http.MethodDelete && r.URL.Path == "/accounts/account-1":
			if r.Header.Get("Authorization") != "Bearer mail-token" {
				t.Errorf("delete authorization=%q", r.Header.Get("Authorization"))
			}
			deleted.Store(true)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	mailbox := NewMailTMProvider("mailtm-fixture", server.URL, "", server.Client())
	email, password, lease, err := mailbox.CreateEmail(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if email != createdAddress || !strings.HasSuffix(email, "@mail.example.test") ||
		password == "" || lease == "" {
		t.Fatalf("email=%q created=%q password=%t lease=%t", email, createdAddress, password != "", lease != "")
	}
	code, err := mailbox.WaitOTP(context.Background(), lease, 2*time.Second)
	if err != nil || code != "246810" {
		t.Fatalf("code=%q err=%v", code, err)
	}
	if err := mailbox.DeleteEmail(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	if !deleted.Load() {
		t.Fatal("mail.tm account was not deleted")
	}
}

func TestMailTMProviderRejectsOversizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", mailboxHTTPResponseBodyLimit+1)))
	}))
	defer server.Close()
	mailbox := NewMailTMProvider("mailtm-fixture", server.URL, "", server.Client())
	if _, _, _, err := mailbox.CreateEmail(context.Background()); err == nil {
		t.Fatal("oversized provider response was accepted")
	}
}
