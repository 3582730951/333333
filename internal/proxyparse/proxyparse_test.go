package proxyparse

import "testing"

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
