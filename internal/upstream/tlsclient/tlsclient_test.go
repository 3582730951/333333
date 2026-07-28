package tlsclient

import (
	"context"
	"io"
	stdhttp "net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"codex-account-pool/internal/bodysource"
	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/tls-client/profiles"
)

func TestResolveProfileFallsBackToChrome(t *testing.T) {
	// ClientProfile holds function fields, so compare by the stable ClientHello id
	// rather than reflect.DeepEqual (which never reports two profiles equal). Str() is a
	// pointer method, so the id must live in an addressable local first.
	//
	// Chrome_120 is the deliberate default: it matches the curl_cffi sidecar's
	// impersonate=chrome120 default exactly, so flipping egress_fingerprint_engine to
	// "inprocess" is fingerprint-identical by construction. ProfileNode/ProfileRustls are
	// A3 placeholders that also resolve to Chrome_120 until a validated capture replaces
	// them, so an unvalidated Kiro profile can never silently present a partial fingerprint.
	wantID := profiles.Chrome_120.GetClientHelloId()
	want := wantID.Str()
	for _, name := range []string{ProfileChrome, "", "totally-unknown", ProfileNode, ProfileRustls} {
		gotID := ResolveProfile(name, "").GetClientHelloId()
		if got := gotID.Str(); got != want {
			t.Fatalf("ResolveProfile(%q) ClientHello = %q, want %q (a mis-set profile must never silently disable fingerprinting)", name, got, want)
		}
	}
}

// TestResolveProfileNamedOverride verifies the explicit named-profile override path: an
// operator JA3Override that names a profile in profiles.MappedTLSClients selects that
// profile, and an unrecognized override safely degrades to the Chrome_120 default rather
// than disabling fingerprinting.
func TestResolveProfileNamedOverride(t *testing.T) {
	ffID := profiles.Firefox_133.GetClientHelloId()
	wantFF := ffID.Str()
	gotFFID := ResolveProfile(ProfileChrome, "firefox_133").GetClientHelloId()
	if got := gotFFID.Str(); got != wantFF {
		t.Fatalf("named override firefox_133 = %q, want %q", got, wantFF)
	}
	chromeID := profiles.Chrome_120.GetClientHelloId()
	wantChrome := chromeID.Str()
	gotChromeID := ResolveProfile(ProfileChrome, "not-a-real-profile-name").GetClientHelloId()
	if got := gotChromeID.Str(); got != wantChrome {
		t.Fatalf("unknown override = %q, want Chrome_120 fallback %q", got, wantChrome)
	}
}

func TestLoweredLowercasesASCIIOnly(t *testing.T) {
	in := []string{"Content-Type", "X-Amz-Target", "already-lower"}
	want := []string{"content-type", "x-amz-target", "already-lower"}
	if got := lowered(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("lowered(%v) = %v, want %v", in, got, want)
	}
}

func TestToStdHeaderStripsOrderKeys(t *testing.T) {
	h := fhttp.Header{
		"Content-Type":        []string{"application/json"},
		"X-Amz-Target":        []string{"a", "b"},
		fhttp.HeaderOrderKey:  []string{"content-type"},
		fhttp.PHeaderOrderKey: []string{":method"},
	}
	out := toStdHeader(h)
	if _, ok := out[fhttp.HeaderOrderKey]; ok {
		t.Fatalf("toStdHeader leaked %s into the response header", fhttp.HeaderOrderKey)
	}
	if _, ok := out[fhttp.PHeaderOrderKey]; ok {
		t.Fatalf("toStdHeader leaked %s into the response header", fhttp.PHeaderOrderKey)
	}
	if got := out.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if got := out.Values("X-Amz-Target"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("X-Amz-Target = %v, want [a b]", got)
	}
	// mutating the source afterwards must not alter the copy
	h["X-Amz-Target"][0] = "mutated"
	if out.Values("X-Amz-Target")[0] != "a" {
		t.Fatalf("toStdHeader did not deep-copy values")
	}
}

func TestClientForPoolsByKey(t *testing.T) {
	f := New()
	c1, err := f.clientFor(Request{Profile: ProfileChrome, ProxyURL: "", CookieJarKey: "acct-1"})
	if err != nil {
		t.Fatalf("clientFor: %v", err)
	}
	c1again, err := f.clientFor(Request{Profile: ProfileChrome, ProxyURL: "", CookieJarKey: "acct-1"})
	if err != nil {
		t.Fatalf("clientFor: %v", err)
	}
	if c1 != c1again {
		t.Fatalf("same transport key returned distinct clients; keep-alive pool is not reused")
	}
	c2, err := f.clientFor(Request{Profile: ProfileChrome, ProxyURL: "", CookieJarKey: "acct-2"})
	if err != nil {
		t.Fatalf("clientFor: %v", err)
	}
	if c1 != c2 {
		t.Fatalf("cookie-jar keys fragmented the transport client pool")
	}
}

func TestCookieJarReusesOnlyKeyedState(t *testing.T) {
	f := New()
	a := f.cookieJar("k")
	b := f.cookieJar("k")
	if a != b {
		t.Fatalf("keyed jar not reused across calls")
	}
	if f.cookieJar("") != nil {
		t.Fatalf("empty key unexpectedly retained cookie state")
	}
}

func TestCookieJarCapacityDoesNotEvictOnHit(t *testing.T) {
	f := New()
	f.cookieJarMax = 2
	a := f.cookieJar("a")
	f.cookieJar("b")
	if got := f.cookieJar("a"); got != a {
		t.Fatalf("jar hit returned a replacement")
	}
	if len(f.jars) != 2 || f.jars["a"] == nil || f.jars["b"] == nil {
		t.Fatalf("jar hit evicted fresh state: keys=%v", f.jars)
	}
	f.cookieJar("c")
	if len(f.jars) != 2 || f.jars["a"] == nil || f.jars["c"] == nil || f.jars["b"] != nil {
		t.Fatalf("capacity eviction did not remove the LRU key: keys=%v", f.jars)
	}
}

// TestDoRoundTrip exercises the full Do path (request build, header add, header-order
// injection, response conversion, streaming body) against a local h1 server. It validates
// the wrapper plumbing; TLS/JA3 fidelity itself is validated separately by the A2
// reflector diagnostic, not a unit test.
func TestDoRoundTrip(t *testing.T) {
	var gotMethod, gotCT, gotTarget string
	var gotBody []byte
	srv := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		gotMethod = r.Method
		gotCT = r.Header.Get("Content-Type")
		gotTarget = r.Header.Get("X-Amz-Target")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("X-Reply", "pong")
		w.WriteHeader(stdhttp.StatusTeapot)
		_, _ = w.Write([]byte("hello-stream"))
	}))
	defer srv.Close()

	f := New()
	hdr := stdhttp.Header{}
	hdr.Set("Content-Type", "application/x-amz-json-1.0")
	hdr.Set("X-Amz-Target", "AmazonCodeWhispererStreamingService.GenerateAssistantResponse")

	resp, err := f.Do(context.Background(), Request{
		Method:      stdhttp.MethodPost,
		URL:         srv.URL,
		Header:      hdr,
		HeaderOrder: []string{"Content-Type", "X-Amz-Target"},
		Body:        bodysource.Bytes([]byte(`{"ping":true}`)),
		Timeout:     10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if gotMethod != stdhttp.MethodPost {
		t.Fatalf("method = %q, want POST", gotMethod)
	}
	if gotCT != "application/x-amz-json-1.0" {
		t.Fatalf("Content-Type = %q", gotCT)
	}
	if gotTarget == "" {
		t.Fatalf("X-Amz-Target not forwarded")
	}
	if string(gotBody) != `{"ping":true}` {
		t.Fatalf("body = %q", string(gotBody))
	}
	if resp.StatusCode != stdhttp.StatusTeapot {
		t.Fatalf("status = %d, want 418", resp.StatusCode)
	}
	if resp.Header.Get("X-Reply") != "pong" {
		t.Fatalf("response header not converted")
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello-stream" {
		t.Fatalf("body = %q, want hello-stream", string(body))
	}
}
