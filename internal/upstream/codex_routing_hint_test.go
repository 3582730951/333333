package upstream

import (
	"net/http"
	"testing"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

func TestCodexRoutingHintHeaderValue(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"model only", `{"model":"gpt-5.6-sol","input":"hi"}`, "model=gpt-5.6-sol"},
		{"model and tier", `{"model":"gpt-5.6-sol","service_tier":"priority","input":"hi"}`, "model=gpt-5.6-sol;tier=priority"},
		{"empty body", ``, ""},
		{"no model", `{"input":"hi"}`, ""},
		{"malformed JSON", `{broken`, ""},
		{"whitespace model trimmed", `{"model":"  gpt-5.6-sol  ","input":"hi"}`, "model=gpt-5.6-sol"},
		{"semicolon in model rejected", `{"model":"a;b","input":"hi"}`, ""},
		{"equals in model rejected", `{"model":"a=b","input":"hi"}`, ""},
		{"tier with semicolon dropped", `{"model":"gpt-5.6-sol","service_tier":"a;b","input":"hi"}`, "model=gpt-5.6-sol"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := Request{Body: testBody([]byte(tt.body))}
			if got := codexRoutingHintHeaderValue(spec); got != tt.want {
				t.Fatalf("hint = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCodexRoutingHintHeaderValueBodyMetaModelOnly(t *testing.T) {
	// Zero-copy/spooled requests expose the model directly and skip service_tier
	// (not a tracked BodyMeta scalar), which matches codex-rs sending the hint with
	// no tier when service_tier is None.
	spec := Request{BodyMeta: &bodysource.BodyMeta{Model: "gpt-5.6-sol"}}
	if got := codexRoutingHintHeaderValue(spec); got != "model=gpt-5.6-sol" {
		t.Fatalf("BodyMeta hint = %q, want model=gpt-5.6-sol", got)
	}
	if spec.BodyMeta.Model == "" {
		t.Fatal("fixture model was lost")
	}
}

func TestApplyCodexHeadersRoutingHintGatedToOAuthResponses(t *testing.T) {
	client := NewClient(config.Default())

	oauth := storage.AccountToken{AccessToken: "at-hint", RefreshToken: "rt-hint"}
	apiKey := storage.AccountToken{OpenAIAPIKey: "sk-hint"}

	spec := func(token storage.AccountToken, path, body string) Request {
		return Request{
			DownstreamPath: path,
			Body:           testBody([]byte(body)),
			Account:        storage.Account{ID: "acc-hint"},
			Token:          token,
			Egress:         storage.EgressProfile{Type: "direct", Health: "healthy"},
		}
	}

	// setHeaderPreserveCase stores under the exact key casing (so the wire carries
	// the lowercase header codex-rs sends), which http.Header.Get cannot read back
	// (Get canonicalizes the lookup key). Read with the case-insensitive fold, the
	// same reader production uses.
	hint := func(h http.Header) string { return getHeaderFold(h, "x-codex-routing-hint") }

	t.Run("oauth responses carries hint", func(t *testing.T) {
		h := http.Header{}
		if err := client.applyCodexHeaders(h, spec(oauth, "/v1/responses", `{"model":"gpt-5.6-sol","input":"hi"}`)); err != nil {
			t.Fatal(err)
		}
		if got := hint(h); got != "model=gpt-5.6-sol" {
			t.Fatalf("hint = %q, want model=gpt-5.6-sol", got)
		}
	})

	t.Run("api-key responses omits hint", func(t *testing.T) {
		h := http.Header{}
		if err := client.applyCodexHeaders(h, spec(apiKey, "/v1/responses", `{"model":"gpt-5.6-sol","input":"hi"}`)); err != nil {
			t.Fatal(err)
		}
		if got := hint(h); got != "" {
			t.Fatalf("API-key transport must not carry a session hint, got %q", got)
		}
	})

	t.Run("downstream hint stripped then synthesized", func(t *testing.T) {
		h := http.Header{}
		s := spec(oauth, "/v1/responses", `{"model":"gpt-5.6-sol","input":"hi"}`)
		s.Headers = http.Header{"X-Codex-Routing-Hint": []string{"model=gpt-4"}}
		if err := client.applyCodexHeaders(h, s); err != nil {
			t.Fatal(err)
		}
		if got := hint(h); got != "model=gpt-5.6-sol" {
			t.Fatalf("caller hint must be replaced by the body-derived value, got %q", got)
		}
	})

	t.Run("models discovery omits hint", func(t *testing.T) {
		h := http.Header{}
		if err := client.applyCodexHeaders(h, spec(oauth, "/models", `{}`)); err != nil {
			t.Fatal(err)
		}
		if got := hint(h); got != "" {
			t.Fatalf("/models discovery must not carry a routing hint, got %q", got)
		}
	})
}
