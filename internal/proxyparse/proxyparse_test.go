package proxyparse

import (
	"strings"
	"testing"
)

func TestParseLineFormats(t *testing.T) {
	cases := []struct {
		in    string
		host  string
		port  string
		user  string
		pass  string
		isErr bool
	}{
		{in: "1.2.3.4:1080:alice:secret", host: "1.2.3.4", port: "1080", user: "alice", pass: "secret"},
		{in: "jp.example.com:1080", host: "jp.example.com", port: "1080"},
		{in: "host:1080:onlyuser", host: "host", port: "1080", user: "onlyuser"},
		{in: "alice:secret@1.2.3.4:1080", host: "1.2.3.4", port: "1080", user: "alice", pass: "secret"},
		{in: "proxy.example:1080@alice:secret", host: "proxy.example", port: "1080", user: "alice", pass: "secret"},
		{in: "socks5h://bob:p@ss@5.6.7.8:1080", host: "5.6.7.8", port: "1080", user: "bob", pass: "p@ss"},
		{in: "host:1080:user:pa:ss:word", host: "host", port: "1080", user: "user", pass: "pa:ss:word"},
		{in: "", isErr: true},
		{in: "nonsense", isErr: true},
	}
	for _, c := range cases {
		d, err := ParseLine(c.in)
		if c.isErr {
			if err == nil {
				t.Fatalf("%q: expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if d.Host != c.host || d.Port != c.port || d.Username != c.user || d.Password != c.pass {
			t.Fatalf("%q: got %+v", c.in, d)
		}
	}
}

func TestEndpoint(t *testing.T) {
	d := Draft{Host: "1.2.3.4", Port: "1080", Username: "alice", Password: "secret"}
	if got := d.Endpoint("socks5h_proxy"); got != "socks5h://alice:secret@1.2.3.4:1080" {
		t.Fatalf("socks5h endpoint = %q", got)
	}
	if got := (Draft{Host: "h", Port: "80"}).Endpoint("http_proxy"); got != "http://h:80" {
		t.Fatalf("http endpoint = %q", got)
	}
}

func TestParseLinesBatch(t *testing.T) {
	text := "1.2.3.4:1080:a:b\n# comment\n\n5.6.7.8:1080\nbroken-line\n"
	drafts, errs := ParseLines(text)
	if len(drafts) != 2 {
		t.Fatalf("expected 2 drafts, got %d (%+v)", len(drafts), drafts)
	}
	if len(errs) != 1 {
		t.Fatalf("expected 1 error for the broken line, got %d (%v)", len(errs), errs)
	}
}

func TestFromFieldsEndpointSchemes(t *testing.T) {
	tests := []struct {
		name       string
		egressType string
		want       string
	}{
		{name: "http proxy", egressType: "http_proxy", want: "http://user:pass@proxy.example:8000"},
		{name: "socks5h proxy", egressType: "socks5h_proxy", want: "socks5h://user:pass@proxy.example:8000"},
		{name: "socks5 proxy", egressType: "socks5_proxy", want: "socks5://user:pass@proxy.example:8000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := FromFields("proxy.example", "8000", "user", "pass")
			if err != nil {
				t.Fatalf("FromFields: %v", err)
			}
			if got := d.Endpoint(tt.egressType); got != tt.want {
				t.Fatalf("endpoint = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNormalizeEndpointInfersSchemeAndEscapesCredentials(t *testing.T) {
	endpoint, typ, err := NormalizeEndpoint("socks5://user:p@ss@proxy.example:1080", "http_proxy")
	if err != nil {
		t.Fatal(err)
	}
	if typ != "socks5_proxy" {
		t.Fatalf("type = %q", typ)
	}
	if endpoint != "socks5://user:p%40ss@proxy.example:1080" {
		t.Fatalf("endpoint = %q", endpoint)
	}
}

func TestParseLineRejectsInvalidPortWithoutEchoingCredential(t *testing.T) {
	_, err := ParseLine("proxy.example:not-a-port:user:do-not-echo")
	if err == nil {
		t.Fatal("expected invalid port")
	}
	if strings.Contains(err.Error(), "do-not-echo") {
		t.Fatalf("error exposed credential: %v", err)
	}
}
