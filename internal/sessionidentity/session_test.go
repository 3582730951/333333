package sessionidentity

import (
	"net/http"
	"testing"

	"codex-account-pool/internal/routing"
)

func TestProjectKeepsDeviceSeedStableButSeparatesLogicalSessions(t *testing.T) {
	body := func(session string) []byte {
		return []byte(`{"metadata":{"user_id":"{\"device_id\":\"one-device\",\"session_id\":\"` + session + `\"}"},"messages":[{"role":"user","content":"same"}]}`)
	}
	a := Project("account-seed", "fallback", nil, body("context-a"))
	a2 := Project("account-seed", "fallback", nil, body("context-a"))
	b := Project("account-seed", "fallback", nil, body("context-b"))
	if a == "" || a != a2 || a == b {
		t.Fatalf("session projection is not stable and isolated: a=%q a2=%q b=%q", a, a2, b)
	}
	if a == "context-a" || b == "context-b" {
		t.Fatal("raw logical session leaked through projection")
	}
}

func TestStrongestPrefersNativeSessionOverGatewayDevice(t *testing.T) {
	headers := http.Header{}
	headers.Set("X-Pool-Client-ID", "one-device")
	headers.Set("X-Claude-Code-Session-Id", "cli-context")
	signal := Strongest(headers, nil)
	if signal.Source != "x-claude-code-session-id" || signal.Value != "cli-context" || !signal.SessionScoped() {
		t.Fatalf("wrong signal: %+v", signal)
	}
}

func TestResolveProjectedReusesCloakedMetadataSession(t *testing.T) {
	body := []byte(`{"metadata":{"user_id":"{\"device_id\":\"virtual-device\",\"session_id\":\"virtual-session\"}"}}`)
	if got := ResolveProjected("seed", "fallback", nil, body, "virtual-device"); got != "virtual-session" {
		t.Fatalf("projected metadata was changed again: %q", got)
	}
}

func TestConversationAnchorMatchesRoutingWithoutFullHistoryDecode(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`{"messages":[{"role":"assistant","content":"prefix"},{"role":"user","content":"first"},{"role":"user","content":"later"}]}`),
		[]byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`),
		[]byte(`{"input":"plain input"}`),
	} {
		root := decodeRoot(body)
		if got, want := conversationAnchor(root), routing.ConversationAnchor(body); got != want {
			t.Fatalf("anchor mismatch: got=%q want=%q body=%s", got, want, body)
		}
	}
}
