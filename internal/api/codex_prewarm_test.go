package api

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
)

func TestCodexResponsesGenerateFalseDetection(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"generate false", `{"type":"response.create","generate":false,"input":[]}`, true},
		{"generate true", `{"type":"response.create","generate":true,"input":[]}`, false},
		{"generate missing", `{"type":"response.create","input":[]}`, false},
		{"generate null", `{"type":"response.create","generate":null,"input":[]}`, false},
		{"generate string", `{"type":"response.create","generate":"false","input":[]}`, false},
		{"generate nested only", `{"type":"response.create","input":[{"generate":false}]}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source, meta, err := bodysource.CaptureJSON(context.Background(), strings.NewReader(tc.raw), bodysource.CaptureOptions{
				MaxBytes: 1 << 20, MemoryThreshold: 1 << 20, TempDir: t.TempDir(),
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			got := codexResponsesGenerateFalse(meta)
			if got != tc.want {
				t.Fatalf("codexResponsesGenerateFalse(meta) = %v, want %v", got, tc.want)
			}
		})
	}
}

// A prewarm frame must keep its generate:false control through the exact frame
// conversion the WebSocket handler applies before turn admission. If the
// conversion dropped it, codexAttempt would treat the frame as a normal
// inference turn.
func TestCodexResponsesPrewarmSurvivesFrameConversion(t *testing.T) {
	raw := `{"type":"response.create","generate":false,"stream":true,"input":[]}`
	source, meta, err := bodysource.CaptureJSON(context.Background(), strings.NewReader(raw), bodysource.CaptureOptions{
		MaxBytes: 1 << 20, MemoryThreshold: 1 << 20, TempDir: t.TempDir(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	kind, patched, patchedMeta, err := responsesWebSocketRequestToSource(context.Background(), source, meta, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	defer patched.Close()
	if kind != "response.create" {
		t.Fatalf("kind=%q", kind)
	}
	if !codexResponsesGenerateFalse(patchedMeta) {
		t.Fatalf("generate:false was lost during frame conversion: meta=%+v", patchedMeta)
	}
	body, err := patched.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	content, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), `"generate":false`) {
		t.Fatalf("generate control missing from converted frame: %s", content)
	}
}

func TestCodexPrewarmFrameContextFlag(t *testing.T) {
	ctx := context.Background()
	if codexPrewarmFrameFromContext(ctx) {
		t.Fatal("plain context must not report prewarm")
	}
	ctx = context.WithValue(ctx, codexResponsesWebSocketPrewarmKey{}, true)
	if !codexPrewarmFrameFromContext(ctx) {
		t.Fatal("flagged context must report prewarm")
	}
	ctx = context.WithValue(ctx, codexResponsesWebSocketPrewarmKey{}, false)
	if codexPrewarmFrameFromContext(ctx) {
		t.Fatal("explicit false must not report prewarm")
	}
}

// A prewarm frame must not commit any turn/session state. Even though the WS
// handler always forces stream:true (so the unary path is unreachable in
// practice), persistCodexStateBindings is the terminal guard that would
// otherwise record the prewarm response id as the conversation anchor.
func TestPersistCodexStateBindingsSkipsPrewarmFrame(t *testing.T) {
	store := driftTestStore(t)
	s := &Server{store: store}
	ctx := context.WithValue(context.Background(), codexResponsesWebSocketPrewarmKey{}, true)
	response := []byte(`{"id":"resp_prewarm_123","status":"completed","model":"gpt-5.6","output":[],"usage":{"input_tokens":100}}`)
	s.persistCodexStateBindings(ctx, nil, []byte(`{"input":[]}`), routing.AffinityKey{}, response, http.Header{}, scheduler.Lease{}, storage.EgressProfile{}, "gpt-5.6", false)

	rows, err := store.ListAuditLog(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	// The prewarm guard returns before any commit, goal, alias or audit write;
	// a non-empty audit log here means the guard did not hold.
	if len(rows) != 0 {
		t.Fatalf("prewarm frame must not write any state: %+v", rows)
	}
}
