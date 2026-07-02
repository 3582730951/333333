package mailbox

import (
	"bytes"
	"strings"
	"testing"

	"github.com/emersion/go-imap"
)

func TestIMAPExtractBodyReadsTextPart(t *testing.T) {
	msg := newIMAPMessageLiteral("Content-Type: text/plain\r\n\r\nYour verification code is 123456.")

	got := (&IMAPProvider{}).extractBody(msg)
	if !strings.Contains(got, "123456") {
		t.Fatalf("body = %q, want verification code", got)
	}
}

func TestIMAPExtractBodyLimitsTextPart(t *testing.T) {
	rawBody := "Your verification code is 123456. " + strings.Repeat("x", imapTextPartBodyLimit*2)
	msg := newIMAPMessageLiteral("Content-Type: text/plain\r\n\r\n" + rawBody)

	got := (&IMAPProvider{}).extractBody(msg)
	if !strings.Contains(got, "123456") {
		t.Fatalf("body = %q, want code within retained prefix", got)
	}
	if len(got) != imapTextPartBodyLimit {
		t.Fatalf("body length = %d, want %d", len(got), imapTextPartBodyLimit)
	}
}

func newIMAPMessageLiteral(raw string) *imap.Message {
	return &imap.Message{
		Body: map[*imap.BodySectionName]imap.Literal{
			{}: bytes.NewBufferString(raw),
		},
	}
}
