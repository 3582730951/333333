package api

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"codex-account-pool/internal/storage"
)

// TestCodexSessionSelfContainedTurnAcrossRouteConflict reproduces the incident
// "unexpected status 409 Conflict: Codex session identity is no longer available".
//
// A self-contained Codex turn (no previous_response_id) that still resolves to a
// durable session binding is replaySafe, so the user-group router installs
// scheduler RouteChoices and the lease goes through SelectAcross. SelectAcross
// only honors route.RequiredAccountID/RequiredEgressID when the request carries a
// true-conversation affinity that has a scheduler affinity binding; a Session-Id
// identity (which the affinity derivation never reads) resolves the mapping with
// pins set but enters the across candidate evaluation, which returns an arbitrary
// pool account with the group's *current* primary egress. identitySnapshot then
// rejects the lease as an epoch conflict and the relay answers 409 — every
// fallback tier repeats the same conflict and the client retries the identical
// body, which is exactly the 409 cluster shape in the emergency export (all tiers
// permanent_4xx, stable request body).
//
// The incident's egress-profiles error window is modeled directly: the session
// commits its binding under group egress egr-e1; the group's primary egress then
// moves to egr-e2 (group-scoped bindings follow the group primary, so every
// across candidate for the bound account offers egr-e2). The next self-contained
// turn must still succeed on the committed (account, egr-e1) pair instead of
// 409ing forever.
func TestCodexSessionSelfContainedTurnAcrossRouteConflict(t *testing.T) {
	var calls int32
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		next := calls
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		if next == 0 {
			_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-across-1\",\"object\":\"response\",\"model\":\"gpt\",\"status\":\"in_progress\"}}\n\n")
			_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-across-1\",\"object\":\"response\",\"model\":\"gpt\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0,\"total_tokens\":1}}}\n\n")
			return
		}
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-across-2\",\"object\":\"response\",\"model\":\"gpt\",\"status\":\"in_progress\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-across-2\",\"object\":\"response\",\"model\":\"gpt\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0,\"total_tokens\":1}}}\n\n")
	})

	enableCodexSessionMappingForTest(h)
	ctx := context.Background()

	const plain = "cap_repro_across"
	const userGroupID = "ug_repro_across"
	if err := h.store.CreateUserGroupDefinition(ctx, storage.UserGroup{
		ID: userGroupID, Name: "Across Repro",
		Targets: []storage.TargetRef{{Kind: storage.TargetKindAccountPoolGroup, ID: "cyber"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertAPIKey(ctx, storage.APIKey{
		KeyHash: hashAPIKey(plain), Label: "across-repro", UserGroupID: userGroupID, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	// Two healthy egress outlets; the group primary moves from egr-e1 to egr-e2
	// between the two turns (the incident's egress-profiles health window). The
	// outlet the session is bound to stays healthy — the 409 must fire purely from
	// the across evaluation ignoring the session pins, not from any outage.
	for _, profile := range []storage.EgressProfile{
		{ID: "egr-e1", Type: "direct", StreamCapable: true, Health: "healthy", MaxConcurrency: 16},
		{ID: "egr-e2", Type: "direct", StreamCapable: true, Health: "healthy", MaxConcurrency: 16},
	} {
		if err := h.store.UpsertEgressProfile(ctx, profile); err != nil {
			t.Fatal(err)
		}
	}
	setGroupEgress := func(egressID string) {
		t.Helper()
		if err := h.store.UpdateGroup(ctx, storage.Group{Name: "cyber", EgressIDs: []string{egressID}}); err != nil {
			t.Fatal(err)
		}
	}
	setGroupEgress("egr-e1")
	h.importAccount(t, "across-repro", "upstream-across-repro", "access-across-repro")

	post := func(t *testing.T, body string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+plain)
		// Session-Id carries the durable identity but is invisible to the affinity
		// derivation, so SelectAcross never enters its true-conversation affinity
		// fast path for this request.
		req.Header.Set("Session-Id", "repro-session")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}

	// Turn 1: fresh root commits the durable binding under group egress egr-e1.
	firstStatus, firstBody := post(t, `{"model":"gpt","stream":true,"input":"start"}`)
	if firstStatus != http.StatusOK || !strings.Contains(firstBody, "resp-across-1") {
		t.Fatalf("turn 1 status=%d body=%s", firstStatus, firstBody)
	}
	req, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+plain)
	req.Header.Set("Session-Id", "repro-session")
	namespace := "client:" + downstreamClientScope(hashAPIKey(plain), req)
	rows, err := h.store.FindCodexSessionAlias(ctx, namespace, storage.CodexSessionAlias{Type: "response", Value: "resp-across-1"})
	if err != nil || len(rows) != 1 {
		audits, _ := h.store.ListAuditLog(ctx, 30)
		t.Fatalf("binding after turn 1 rows=%d err=%v\naudit: %+v", len(rows), err, audits)
	}
	if rows[0].EgressID != "egr-e1" || rows[0].AccountID == "" {
		t.Fatalf("binding did not commit under egr-e1: %+v", rows[0])
	}
	boundAccount := rows[0].AccountID

	// The incident: the group's primary egress moves while the session binding
	// still pins egr-e1.
	setGroupEgress("egr-e2")
	if g, gerr := h.store.GetGroup(ctx, "cyber"); gerr != nil {
		t.Fatalf("GetGroup: %v", gerr)
	} else {
		t.Logf("group cyber egress_ids=%v default=%q", g.EgressIDs, g.DefaultEgressID)
	}

	// Turn 2: self-contained (no previous_response_id) but resolvable to the
	// durable root. Must continue on the committed account+egress.
	before := calls
	secondStatus, secondBody := post(t, `{"model":"gpt","stream":true,"input":"continue"}`)
	if secondStatus != http.StatusOK {
		t.Fatalf("turn 2 status=%d body=%s (session binding account=%s egress=%s; group primary moved to egr-e2)", secondStatus, secondBody, boundAccount, rows[0].EgressID)
	}
	if !strings.Contains(secondBody, "resp-across-2") {
		t.Fatalf("turn 2 body=%s", secondBody)
	}
	if calls != before+1 {
		t.Fatalf("turn 2 upstream calls=%d want %d (started at %d)", calls, before+1, before)
	}
	afterRows, err := h.store.FindCodexSessionAlias(ctx, namespace, storage.CodexSessionAlias{Type: "root", Value: "repro-session"})
	t.Logf("after turn 2 root alias rows=%d err=%v", len(afterRows), err)
	for _, row := range afterRows {
		t.Logf("  account=%s egress=%s epoch=%d state=%s rootSession=%s", row.AccountID, row.EgressID, row.Epoch, row.State, row.RootSessionID)
	}
	audits, _ := h.store.ListAuditLog(ctx, 40)
	for _, a := range audits {
		t.Logf("audit action=%s reason=%s detail=%s", a.Action, a.Reason, a.Detail)
	}
	diags, _ := h.store.ListCodexUpstreamAttemptDiagnostics(ctx)
	for _, d := range diags {
		t.Logf("attempt account=%s egress=%s status=%d state=%s", d.AccountID, d.EgressID, d.StatusCode, d.State)
	}
}
