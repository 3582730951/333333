package api

import (
	"bytes"
	"context"
	"net/http/httptest"
	"testing"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/routing"
)

func TestCodexBodySourceForAttemptRequiresOriginalBytes(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.6-sol","stream":true,"input":"keep"}`)
	source := bodysource.Bytes(raw)
	meta, err := bodysource.ScanJSON(context.Background(), source, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := contextWithBodyMeta(contextWithBodySource(context.Background(), source), meta)

	gotSource, gotMeta := codexBodySourceForAttempt(ctx, raw, raw)
	if gotSource != source || gotMeta == nil || gotMeta.Size != int64(len(raw)) {
		t.Fatalf("original body source was not retained: source=%T meta=%+v", gotSource, gotMeta)
	}
	clone := append([]byte(nil), raw...)
	gotSource, gotMeta = codexBodySourceForAttempt(ctx, raw, clone)
	if gotSource != source || gotMeta == nil {
		t.Fatal("byte-identical replay lost its source")
	}
	changed := bytes.Replace(clone, []byte(`"keep"`), []byte(`"edit"`), 1)
	if gotSource, gotMeta = codexBodySourceForAttempt(ctx, raw, changed); gotSource != nil || gotMeta != nil {
		t.Fatal("changed body reused stale source metadata")
	}
}

func TestGoalOriginalBodyDoesNotDuplicateOwnedRequestBytes(t *testing.T) {
	body := []byte(`{"input":"large-owned-request"}`)
	got := goalOriginalBody(withGoalOriginalBody(context.Background(), body), nil)
	if len(got) != len(body) || &got[0] != &body[0] {
		t.Fatal("goal context duplicated the request body")
	}
}

func TestBodyMetaHotValuesMatchLegacyScans(t *testing.T) {
	unicodeRaw := []byte(`{"model":"gpt-5.6-sol","stream":false,"input":"` + string([]rune{0x4f60, 0x597d, ' ', 0x4e16, 0x754c}) + `"}`)
	for _, raw := range [][]byte{
		[]byte(`{"model":"gpt-5.6-sol","stream":true,"input":"ascii"}`),
		unicodeRaw,
		[]byte(`{"model":"gpt-5.6-sol","input":"stream absent"}`),
		[]byte(`{"model":"gpt-5.6-sol","stream":"invalid","input":"same fallback semantics"}`),
	} {
		source := bodysource.Bytes(raw)
		meta, err := bodysource.ScanJSON(context.Background(), source, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := streamRequestWithMeta(raw, &meta), isStreamRequest(raw); got != want {
			t.Fatalf("stream mismatch for %s: got=%v want=%v", raw, got, want)
		}
		if got, want := estimatedTokensWithMeta(raw, &meta), estimatedTokensWithMeta(raw, nil); got != want {
			t.Fatalf("token estimate mismatch for %s: got=%d want=%d", raw, got, want)
		}
	}
}

func TestRequestBodyBytesReusesContiguousCapturedBody(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.6-sol","input":"large-owned-request"}`)
	source := bodysource.Bytes(raw)
	request := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(raw))
	request = request.WithContext(contextWithBodySource(request.Context(), source))
	got, err := requestBodyBytes(request, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(raw) || &got[0] != &raw[0] {
		t.Fatal("request body was copied despite a contiguous captured source")
	}
}

func TestAffinityWithMetaMatchesLegacyPrecedence(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		header map[string]string
	}{
		{name: "parent header", body: `{"thread_id":"body","prompt_cache_key":"cache"}`, header: map[string]string{"X-Codex-Parent-Thread-ID": "parent"}},
		{name: "thread header", body: `{"thread_id":"body"}`, header: map[string]string{"Thread-ID": "header-thread"}},
		{name: "thread body", body: `{"thread_id":"body"}`},
		{name: "conversation body", body: `{"conversation_id":"conversation"}`},
		{name: "window header", body: `{"prompt_cache_key":"cache"}`, header: map[string]string{"X-Codex-Window-ID": "window"}},
		{name: "prompt cache", body: `{"prompt_cache_key":"cache"}`},
		{name: "previous response", body: `{"previous_response_id":"resp_1"}`},
		{name: "turn metadata header", body: `{"input":"fallback"}`, header: map[string]string{"X-Codex-Turn-Metadata": "turn"}},
		{name: "fallback", body: `{"model":"gpt-5.6-sol","input":"fallback"}`, header: map[string]string{"Authorization": "Bearer downstream"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw := []byte(test.body)
			source := bodysource.Bytes(raw)
			meta, err := bodysource.ScanJSON(context.Background(), source, nil)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest("POST", "/v1/responses", nil)
			for key, value := range test.header {
				request.Header.Set(key, value)
			}
			got := affinityWithMeta(request, raw, &meta)
			want := routing.ExtractAffinityKey(request, raw)
			if got != want {
				t.Fatalf("affinity mismatch: got=%+v want=%+v", got, want)
			}
		})
	}
}
