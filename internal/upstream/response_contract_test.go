package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestEnforceResponseContractRejectsNilResponseAndBody(t *testing.T) {
	if resp, err := enforceResponseContract(nil, nil); resp != nil || !errors.Is(err, errInvalidResponseContract) {
		t.Fatalf("nil response contract result=(%v,%v)", resp, err)
	}
	if resp, err := enforceResponseContract(&Response{StatusCode: http.StatusOK}, nil); resp != nil || !errors.Is(err, errInvalidResponseContract) {
		t.Fatalf("nil body contract result=(%v,%v)", resp, err)
	}
	wantErr := errors.New("transport failed")
	if _, err := enforceResponseContract(nil, wantErr); !errors.Is(err, wantErr) {
		t.Fatalf("transport error changed: %v", err)
	}

	resp, err := enforceResponseContract(&Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("ok")),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header == nil {
		t.Fatal("valid response retained a nil header map")
	}
}

func TestRequestGuardNilBodyReturnsTypedErrorInsteadOfPanicking(t *testing.T) {
	_, guard := newRequestGuard(context.Background(), 0)
	body := guard.Wrap(nil)
	if _, err := body.Read(make([]byte, 1)); !errors.Is(err, errInvalidResponseContract) {
		t.Fatalf("nil body read error=%v", err)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
}
