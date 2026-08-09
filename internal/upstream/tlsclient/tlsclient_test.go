package tlsclient

import (
	"bufio"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	stdhttp "net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/identity"
	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/tls-client/profiles"
	utls "github.com/bogdanfinn/utls"
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

func TestResolveClaudeProfileMatchesCapturedBunClientHello(t *testing.T) {
	profile := ResolveProfile(ProfileClaude, "")
	spec, err := profile.GetClientHelloSpec()
	if err != nil {
		t.Fatalf("GetClientHelloSpec: %v", err)
	}
	joinUint16 := func(values []uint16) string {
		out := make([]string, len(values))
		for i, value := range values {
			out[i] = strconv.Itoa(int(value))
		}
		return strings.Join(out, "-")
	}
	extensionIDs := make([]uint16, 0, len(spec.Extensions))
	var curves []uint16
	var points []uint16
	for _, extension := range spec.Extensions {
		var id uint16
		switch value := extension.(type) {
		case *utls.SNIExtension:
			id = 0
		case *utls.ExtendedMasterSecretExtension:
			id = 23
		case *utls.RenegotiationInfoExtension:
			id = 65281
		case *utls.SupportedCurvesExtension:
			id = 10
			for _, curve := range value.Curves {
				curves = append(curves, uint16(curve))
			}
		case *utls.SupportedPointsExtension:
			id = 11
			for _, point := range value.SupportedPoints {
				points = append(points, uint16(point))
			}
		case *utls.SessionTicketExtension:
			id = 35
		case *utls.SignatureAlgorithmsExtension:
			id = 13
		case *utls.KeyShareExtension:
			id = 51
		case *utls.PSKKeyExchangeModesExtension:
			id = 45
		case *utls.SupportedVersionsExtension:
			id = 43
		case *utls.ALPNExtension:
			t.Fatal("captured Claude ClientHello has no ALPN extension")
		default:
			t.Fatalf("unexpected Claude extension %T", extension)
		}
		extensionIDs = append(extensionIDs, id)
	}
	ja3 := fmt.Sprintf("771,%s,%s,%s,%s",
		joinUint16(spec.CipherSuites), joinUint16(extensionIDs), joinUint16(curves), joinUint16(points))
	if ja3 != identity.ClaudeJA3 {
		t.Fatalf("Claude profile JA3:\n got  %s\n want %s", ja3, identity.ClaudeJA3)
	}
	sum := md5.Sum([]byte(ja3))
	if got, want := hex.EncodeToString(sum[:]), "203503b7023848ab87b9836c336b8e81"; got != want {
		t.Fatalf("Claude profile JA3 MD5 = %s, want %s", got, want)
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

func TestCopyHeadersPreservesWireCaseAndDeepCopies(t *testing.T) {
	src := stdhttp.Header{
		"Content-Type":   {"application/json"},
		"anthropic-beta": {"one", "two"},
		"x-api-key":      {"secret"},
		"x-app":          {"cli"},
	}
	dst := fhttp.Header{}
	copyHeadersPreservingCase(dst, src)
	for _, key := range []string{"Content-Type", "anthropic-beta", "x-api-key", "x-app"} {
		if _, ok := dst[key]; !ok {
			t.Errorf("exact key %q missing from %#v", key, dst)
		}
	}
	for _, canonicalized := range []string{"Anthropic-Beta", "X-Api-Key", "X-App"} {
		if _, ok := dst[canonicalized]; ok {
			t.Errorf("copy canonicalized %q and changed wire casing", canonicalized)
		}
	}
	src["anthropic-beta"][0] = "mutated"
	if got := dst["anthropic-beta"][0]; got != "one" {
		t.Fatalf("copy shares value storage: got %q", got)
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

// TestForceHTTP1FragmentsTheClientPool: ForceHTTP1 is baked into a client's ALPN at
// construction time, so an h1-pinned and a non-pinned request that share profile+proxy must
// NOT share a cached client. If they did, whichever request created it first would silently
// decide the other's protocol — either putting h2 on Claude traffic (the exact leak the pin
// exists to remove) or dragging another provider down to HTTP/1.1.
func TestForceHTTP1FragmentsTheClientPool(t *testing.T) {
	f := New()
	h2Client, err := f.clientFor(Request{Profile: ProfileChrome})
	if err != nil {
		t.Fatalf("clientFor: %v", err)
	}
	h1Client, err := f.clientFor(Request{Profile: ProfileChrome, ForceHTTP1: true})
	if err != nil {
		t.Fatalf("clientFor: %v", err)
	}
	if h2Client == h1Client {
		t.Fatal("ForceHTTP1 is not part of the client cache key: an h1-pinned request reused an h2 client, so the ALPN pin is silently lost")
	}
	// Each variant still pools with its own kind.
	h1Again, err := f.clientFor(Request{Profile: ProfileChrome, ForceHTTP1: true})
	if err != nil {
		t.Fatalf("clientFor: %v", err)
	}
	if h1Again != h1Client {
		t.Fatal("two h1-pinned requests did not share a client; keep-alive pooling is broken")
	}
	if got, want := cacheKey(Request{Profile: ProfileChrome, ForceHTTP1: true}), cacheKey(Request{Profile: ProfileChrome}); got == want {
		t.Fatalf("cacheKey ignores ForceHTTP1: %q == %q", got, want)
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

func TestDoPreservesCapturedClaudeHeaderCaseAndOrderOnWire(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	rawRequest := make(chan string, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			rawRequest <- "accept error: " + acceptErr.Error()
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		var raw strings.Builder
		for {
			line, readErr := reader.ReadString('\n')
			raw.WriteString(line)
			if line == "\r\n" || readErr != nil {
				break
			}
		}
		rawRequest <- raw.String()
		_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
	}()

	headers := stdhttp.Header{
		"Accept":            {"application/json"},
		"Content-Type":      {"application/json"},
		"User-Agent":        {"claude-cli/2.1.226 (external, sdk-cli)"},
		"X-Stainless-OS":    {"Linux"},
		"anthropic-version": {"2023-06-01"},
		"x-api-key":         {"secret"},
		"x-app":             {"cli"},
		"Connection":        {"keep-alive"},
		"Accept-Encoding":   {"gzip, deflate, br, zstd"},
	}
	order := []string{"accept", "content-type", "user-agent", "x-stainless-os", "anthropic-version", "x-api-key", "x-app", "connection", "host", "accept-encoding", "content-length"}
	response, err := New().Do(context.Background(), Request{
		Method:      stdhttp.MethodPost,
		URL:         "http://" + listener.Addr().String() + "/v1/messages?beta=true",
		Header:      headers,
		HeaderOrder: order,
		Body:        bodysource.Bytes([]byte("{}")),
		Profile:     ProfileClaude,
		ForceHTTP1:  true,
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	response.Body.Close()
	raw := <-rawRequest
	fields := []string{
		"Accept: application/json\r\n",
		"Content-Type: application/json\r\n",
		"User-Agent: claude-cli/2.1.226 (external, sdk-cli)\r\n",
		"X-Stainless-OS: Linux\r\n",
		"anthropic-version: 2023-06-01\r\n",
		"x-api-key: secret\r\n",
		"x-app: cli\r\n",
		"Connection: keep-alive\r\n",
		"Host: " + listener.Addr().String() + "\r\n",
		"Accept-Encoding: gzip, deflate, br, zstd\r\n",
		"Content-Length: 2\r\n",
	}
	last := -1
	for _, field := range fields {
		position := strings.Index(raw, field)
		if position < 0 {
			t.Fatalf("wire field %q missing from:\n%s", field, raw)
		}
		if position <= last {
			t.Fatalf("wire field %q is out of order in:\n%s", field, raw)
		}
		last = position
	}
}
