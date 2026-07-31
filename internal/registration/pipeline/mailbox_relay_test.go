package pipeline

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"codex-account-pool/internal/registration/provider"
)

type relayFixtureMailbox struct {
	code    chan string
	deleted atomic.Int32
}

func (p *relayFixtureMailbox) CreateEmail(context.Context) (string, string, string, error) {
	return "relay-child@example.test", "hidden-password", "fixture-mailbox-id", nil
}

func (p *relayFixtureMailbox) WaitOTP(ctx context.Context, _ string, _ time.Duration) (string, error) {
	select {
	case code := <-p.code:
		return code, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (p *relayFixtureMailbox) DeleteEmail(context.Context, string) error {
	p.deleted.Add(1)
	return nil
}

func (p *relayFixtureMailbox) Name() string { return "fixture-mailbox" }
func (p *relayFixtureMailbox) Type() string { return "mailbox" }
func (p *relayFixtureMailbox) MailboxDomains() []string {
	return []string{"example.test"}
}
func (p *relayFixtureMailbox) MailboxUsesCustomDomain() bool { return true }

func relayGET(t *testing.T, client *http.Client, relayURL, token string) (*http.Response, map[string]interface{}) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, relayURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		_ = resp.Body.Close()
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp, payload
}

func TestMailboxRelayAuthenticatesWaitsAndCleansUp(t *testing.T) {
	fixture := &relayFixtureMailbox{code: make(chan string, 1)}
	pipeline := NewPipeline(nil, &provider.Manager{
		Mailbox: []provider.MailboxProvider{fixture},
	}, nil, nil)
	relay, err := pipeline.prepareMailboxRelay(context.Background(), RegisterRequest{
		MailboxProvider: "fixture-mailbox",
		MailboxDomain:   "example.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { relay.Close(context.Background()) })
	if relay.Email != "relay-child@example.test" || relay.Token == "" || relay.URL == "" {
		t.Fatalf("relay=%+v", relay)
	}

	client := &http.Client{Timeout: 2 * time.Second}
	unauthorized, _ := relayGET(t, client, relay.URL, "")
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d", unauthorized.StatusCode)
	}
	waiting, payload := relayGET(t, client, relay.URL, relay.Token)
	if waiting.StatusCode != http.StatusOK || payload["status"] != "waiting" {
		t.Fatalf("waiting status=%d payload=%v", waiting.StatusCode, payload)
	}

	fixture.code <- "654321"
	deadline := time.Now().Add(2 * time.Second)
	for {
		ready, readyPayload := relayGET(t, client, relay.URL, relay.Token)
		if ready.StatusCode == http.StatusOK && readyPayload["status"] == "ready" {
			emails, ok := readyPayload["emails"].([]interface{})
			if !ok || len(emails) != 1 {
				t.Fatalf("ready payload=%v", readyPayload)
			}
			message, _ := emails[0].(map[string]interface{})
			if message["body"] != "654321" {
				t.Fatalf("ready message=%v", message)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("relay did not publish code: %v", readyPayload)
		}
		time.Sleep(10 * time.Millisecond)
	}

	relay.Close(context.Background())
	relay.Close(context.Background())
	if fixture.deleted.Load() != 1 {
		t.Fatalf("mailbox cleanup calls=%d want=1", fixture.deleted.Load())
	}
}
