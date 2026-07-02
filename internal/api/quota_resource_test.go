package api

import (
	"os"
	"strings"
	"testing"
)

func TestAdminQuotaDoesNotLoadAllAccounts(t *testing.T) {
	raw, err := os.ReadFile("admin_resources.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)
	start := strings.Index(source, "func (s *Server) adminQuota(")
	if start < 0 {
		t.Fatal("adminQuota handler not found")
	}
	rest := source[start+len("func (s *Server) adminQuota("):]
	end := strings.Index(rest, "\nfunc ")
	if end < 0 {
		end = len(rest)
	}
	body := rest[:end]
	if strings.Contains(body, ".ListAccounts(") {
		t.Fatal("adminQuota must not load the full account table; resolve labels only for quota snapshot account IDs")
	}
	if !strings.Contains(body, ".AccountLabelsByID(") {
		t.Fatal("adminQuota must resolve labels through AccountLabelsByID")
	}
}
