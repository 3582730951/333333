package pipeline

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"codex-account-pool/internal/registration/provider"
	"codex-account-pool/internal/supervisor"
)

// mailboxRelay adapts every Go MailboxProvider to the small authenticated JSON
// inbox contract consumed by the maintained protocol/browser subprocesses. It
// listens only on loopback, keeps the provider token in memory, and returns no
// mailbox credential to the child process.
type mailboxRelay struct {
	Email string
	URL   string
	Token string

	server    *http.Server
	listener  net.Listener
	cancel    context.CancelFunc
	provider  provider.MailboxProvider
	mailboxID string
	closeOnce sync.Once
}

type mailboxRelayState struct {
	mu       sync.RWMutex
	code     string
	received time.Time
	err      error
}

func relaySecret() string {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err == nil {
		return hex.EncodeToString(raw)
	}
	return hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
}

func (p *Pipeline) prepareMailboxRelay(ctx context.Context, req RegisterRequest) (*mailboxRelay, error) {
	if p == nil || p.providerMgr == nil || len(p.providerMgr.Mailbox) == 0 {
		return nil, provider.ErrNoProviderAvailable
	}
	mailboxProvider, email, _, mailboxID, err := p.providerMgr.GetMailboxWithConstraints(
		ctx, req.MailboxProvider, req.MailboxDomain,
	)
	if err != nil {
		return nil, err
	}
	relayCtx, cancel := context.WithCancel(ctx)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		cancel()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		_ = mailboxProvider.DeleteEmail(cleanupCtx, mailboxID)
		cleanupCancel()
		return nil, err
	}

	token := relaySecret()
	state := &mailboxRelayState{}
	go func() {
		defer supervisor.Recover("registration-mailbox-otp")
		code, waitErr := mailboxProvider.WaitOTP(relayCtx, mailboxID, 3*time.Minute)
		state.mu.Lock()
		state.code = strings.TrimSpace(code)
		state.received = time.Now().UTC()
		state.err = waitErr
		state.mu.Unlock()
	}()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		presented := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if len(presented) != len(token) ||
			subtle.ConstantTimeCompare([]byte(presented), []byte(token)) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"emails": []interface{}{}})
			return
		}
		state.mu.RLock()
		code, received, waitErr := state.code, state.received, state.err
		state.mu.RUnlock()
		if code == "" {
			payload := map[string]interface{}{"emails": []interface{}{}}
			if waitErr != nil && !errors.Is(waitErr, context.Canceled) {
				payload["status"] = "provider_wait_failed"
			} else {
				payload["status"] = "waiting"
			}
			_ = json.NewEncoder(w).Encode(payload)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "ready",
			"emails": []map[string]string{{
				"subject":    "Verification code",
				"body":       code,
				"receivedAt": received.Format(time.RFC3339),
				"时间":         received.Format(time.RFC3339),
			}},
		})
	})
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       15 * time.Second,
	}
	relay := &mailboxRelay{
		Email: email, URL: "http://" + listener.Addr().String(), Token: token,
		server: server, listener: listener, cancel: cancel,
		provider: mailboxProvider, mailboxID: mailboxID,
	}
	go func() {
		defer supervisor.Recover("registration-mailbox-relay")
		_ = server.Serve(listener)
	}()
	return relay, nil
}

func (r *mailboxRelay) Close(ctx context.Context) {
	if r == nil {
		return
	}
	r.closeOnce.Do(func() {
		if r.cancel != nil {
			r.cancel()
		}
		shutdownCtx, shutdownCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		if r.server != nil {
			_ = r.server.Shutdown(shutdownCtx)
		}
		if r.provider != nil {
			_ = r.provider.DeleteEmail(shutdownCtx, r.mailboxID)
		}
		shutdownCancel()
	})
}
