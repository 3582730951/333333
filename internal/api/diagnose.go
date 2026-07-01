package api

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
)

// describeNoAccount turns the scheduler's opaque "no active account available" into
// an actionable 503 message. The overwhelmingly common cause of "I have GPT accounts
// in the pool but the API key says there is no candidate" is a GROUP MISMATCH:
// /v1/models is group-blind (it advertises the whole pool's catalog), so the model
// list looks correct while the key actually routes to a group that holds none of
// those accounts. This re-derives the high-signal facts — how many usable accounts
// the routed group has (by provider), and which OTHER groups DO have active accounts
// — so the operator can immediately see and fix the misroute. Best-effort and
// read-only; for a strict-sticky failure or any non-ErrNoAccount error (incl. DB
// errors) it returns the original error unchanged so status mapping stays correct.
func (s *Server) describeNoAccount(ctx context.Context, group, provider, model string, err error) error {
	if err == nil || !errors.Is(err, scheduler.ErrNoAccount) {
		return err
	}
	all, lerr := s.store.ListAccounts(ctx)
	if lerr != nil {
		return err
	}
	type gcount struct{ active, parked, codex, claude, custom int }
	byGroup := map[string]*gcount{}
	now := storage.Now()
	for _, a := range all {
		g := byGroup[a.GroupName]
		if g == nil {
			g = &gcount{}
			byGroup[a.GroupName] = g
		}
		if a.Status != "active" || a.QuarantineUntil > now {
			g.parked++
			continue
		}
		g.active++
		switch a.Provider {
		case "claude":
			g.claude++
		case "", "codex":
			g.codex++
		default:
			g.custom++
		}
	}
	prov := provider
	if prov == "" {
		prov = "any"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "no available account for this request (group=%s, provider=%s", group, prov)
	if model != "" {
		fmt.Fprintf(&b, ", model=%s", model)
	}
	b.WriteString("). ")
	cur := byGroup[group]
	switch {
	case cur == nil || (cur.active == 0 && cur.parked == 0):
		fmt.Fprintf(&b, "Group %q has no accounts. ", group)
	case cur.active == 0:
		fmt.Fprintf(&b, "Group %q has %d account(s) but all are quarantined or disabled. ", group, cur.parked)
	default:
		fmt.Fprintf(&b, "Group %q: %d active (codex=%d claude=%d custom=%d), %d quarantined/disabled. ", group, cur.active, cur.codex, cur.claude, cur.custom, cur.parked)
		if provider == "codex" && cur.codex == 0 {
			b.WriteString("None of the active accounts are Codex/GPT accounts. ")
		} else if provider == "claude" && cur.claude == 0 {
			b.WriteString("None of the active accounts are Claude accounts. ")
		}
	}
	// Where ARE the usable accounts? The smoking gun for a group mismatch.
	var others []string
	for g, c := range byGroup {
		if g == group || c.active == 0 {
			continue
		}
		others = append(others, fmt.Sprintf("%q(%d active)", g, c.active))
	}
	if len(others) > 0 {
		sort.Strings(others)
		fmt.Fprintf(&b, "Active accounts ARE present in other group(s): %s. Point this API key's group there, or import accounts into %q.", strings.Join(others, ", "), group)
	}
	// Capability hint: accounts present in-group but none advertise the requested model.
	if model != "" && cur != nil && cur.active > 0 {
		if m, e := s.store.AccountsWithModel(ctx, group, model); e == nil && len(m) == 0 {
			fmt.Fprintf(&b, " Note: no account in %q has model %q probed (other models may still work).", group, model)
		}
	}
	return errors.New(strings.TrimSpace(b.String()))
}

func (s *Server) writePublicNoAccountError(ctx context.Context, w http.ResponseWriter, status int, group, provider, model string, err error) {
	if err == nil {
		writePublicUnavailable(w, status)
		return
	}
	if !errors.Is(err, scheduler.ErrNoAccount) && !errors.Is(err, scheduler.ErrStrictUnavailable) {
		writeError(w, status, err)
		return
	}
	detail := err.Error()
	if d := s.describeNoAccount(ctx, group, provider, model, err); d != nil {
		detail = d.Error()
	}
	_ = s.store.InsertAuditLog(ctx, storage.AuditLogRow{
		Action: "routing_unavailable",
		Reason: "no_public_account_detail",
		Detail: fmt.Sprintf("group_hash=%s provider=%s model=%s status=%d detail=%s",
			shortHash(group), provider, model, status, bodySnippet([]byte(detail), 600)),
	})
	writePublicUnavailable(w, status)
}

func writePublicUnavailable(w http.ResponseWriter, status int) {
	if status <= 0 {
		status = http.StatusServiceUnavailable
	}
	errorBody := map[string]interface{}{
		"message": "The model is temporarily unavailable. Please retry shortly.",
		"type":    "server_error",
	}
	if requestID := strings.TrimSpace(w.Header().Get(requestIDHeader)); requestID != "" {
		errorBody["request_id"] = requestID
	}
	writeJSON(w, status, map[string]interface{}{"error": errorBody})
}

func shortHash(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(v))
	return fmt.Sprintf("%x", sum[:6])
}
