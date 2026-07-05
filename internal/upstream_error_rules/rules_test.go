package upstream_error_rules

import (
	"net/http"
	"testing"

	"codex-account-pool/internal/storage"
)

func TestMatchFirstEnabledRuleByPriority(t *testing.T) {
	rules := []storage.UpstreamErrorRule{
		{ID: "later", Name: "later", Enabled: true, Priority: 20, Providers: []string{"codex"}, Entrypoints: []string{"responses"}, ModelPatterns: []string{"gpt-5*"}, StatusCodes: []int{429}, DownstreamAction: DownstreamActionPass},
		{ID: "winner", Name: "winner", Enabled: true, Priority: 10, Providers: []string{"codex"}, Entrypoints: []string{"responses"}, ModelPatterns: []string{"gpt-5.5"}, StatusCodes: []int{429}, BodyKeywords: []string{"QUOTA"}, MatchMode: MatchAny, AccountAction: AccountActionCooldown, DownstreamAction: DownstreamActionFailover},
		{ID: "disabled", Name: "disabled", Enabled: false, Priority: 1, Providers: []string{"codex"}, Entrypoints: []string{"responses"}, StatusCodes: []int{429}, DownstreamAction: DownstreamActionCustomError},
	}
	match, ok := Match(rules, MatchInput{Provider: "codex", Entrypoint: "responses", Model: "gpt-5.5", Status: 429, Body: []byte(`{"error":"quota exhausted"}`)})
	if !ok {
		t.Fatal("expected a matching rule")
	}
	if match.Rule.ID != "winner" {
		t.Fatalf("matched rule = %q, want winner", match.Rule.ID)
	}
	if match.AccountAction != AccountActionCooldown || match.DownstreamAction != DownstreamActionFailover {
		t.Fatalf("unexpected actions: %+v", match)
	}
}

func TestMatchModeAllRequiresEveryConfiguredConditionGroup(t *testing.T) {
	rule := storage.UpstreamErrorRule{
		ID: "all", Enabled: true, Priority: 1,
		Providers: []string{"claude"}, Entrypoints: []string{"claude_messages"}, ModelPatterns: []string{"claude-sonnet-*"},
		StatusCodes: []int{529}, BodyKeywords: []string{"overloaded"}, MatchMode: MatchAll,
	}
	if _, ok := Match([]storage.UpstreamErrorRule{rule}, MatchInput{Provider: "claude", Entrypoint: "claude_messages", Model: "claude-sonnet-4.5", Status: 529, Body: []byte("temporarily overloaded")}); !ok {
		t.Fatal("expected all-mode rule to match status and keyword")
	}
	if _, ok := Match([]storage.UpstreamErrorRule{rule}, MatchInput{Provider: "claude", Entrypoint: "claude_messages", Model: "claude-sonnet-4.5", Status: 529, Body: []byte("different")}); ok {
		t.Fatal("all-mode rule matched without required keyword")
	}
	if _, ok := Match([]storage.UpstreamErrorRule{rule}, MatchInput{Provider: "codex", Entrypoint: "claude_messages", Model: "claude-sonnet-4.5", Status: 529, Body: []byte("temporarily overloaded")}); ok {
		t.Fatal("provider mismatch should not match")
	}
}

func TestPreviewCustomErrorAndPassThrough(t *testing.T) {
	custom := storage.UpstreamErrorRule{ID: "custom", Enabled: true, DownstreamAction: DownstreamActionCustomError, ResponseStatus: 503, CustomMessage: "上游暂不可用"}
	preview := Preview(custom, MatchInput{Status: 529, Header: http.Header{"X-Upstream": []string{"raw"}}, Body: []byte(`{"secret":"quota"}`)})
	if preview.Status != 503 || preview.Body == "" || preview.DownstreamAction != DownstreamActionCustomError {
		t.Fatalf("bad custom preview: %+v", preview)
	}
	pass := storage.UpstreamErrorRule{ID: "pass", Enabled: true, DownstreamAction: DownstreamActionPass}
	preview = Preview(pass, MatchInput{Status: 418, Body: []byte("raw body")})
	if preview.Status != 418 || preview.Body != "raw body" {
		t.Fatalf("bad pass preview: %+v", preview)
	}
}
