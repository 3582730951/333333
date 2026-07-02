package api

import (
	"net/http"
	"testing"
)

// admin_users_test.go covers P4 admin user management: an admin can create users,
// change role/status, reset passwords, and delete users (but not lock themselves
// out), and the registration toggle gates self-registration.
func TestAdminUserManagementAndRegistrationToggle(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	admin := jarClient(t)
	if resp, _ := doReq(t, admin, http.MethodPost, h.pool.URL+"/auth/register", `{"email":"admin@x.io","password":"hunter2hunter"}`, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("admin register: %d", resp.StatusCode)
	}
	csrf := csrfFor(t, admin, h.pool.URL)
	H := map[string]string{csrfHeaderName: csrf}
	_, me := doReq(t, admin, http.MethodGet, h.pool.URL+"/auth/me", "", nil)
	adminID, _ := me["id"].(string)

	// Admin creates a normal user.
	resp, body := doReq(t, admin, http.MethodPost, h.pool.URL+"/admin/users", `{"email":"u@x.io","password":"password123","role":"user"}`, H)
	if resp.StatusCode != http.StatusOK || body["role"] != "user" {
		t.Fatalf("create user: %d %v", resp.StatusCode, body)
	}
	uid, _ := body["id"].(string)
	if users := getArray(t, admin, h.pool.URL+"/admin/users"); len(users) != 2 {
		t.Fatalf("expected 2 users, got %d", len(users))
	}

	// Promote + disable + reset password in one PATCH.
	if resp, b := doReq(t, admin, http.MethodPatch, h.pool.URL+"/admin/users/"+uid, `{"role":"admin","status":"disabled","password":"newsecret1"}`, H); resp.StatusCode != http.StatusOK || b["role"] != "admin" || b["status"] != "disabled" {
		t.Fatalf("patch user: %d %v", resp.StatusCode, b)
	}

	// Self-lockout guard: admin cannot demote/disable their own account.
	if resp, _ := doReq(t, admin, http.MethodPatch, h.pool.URL+"/admin/users/"+adminID, `{"role":"user"}`, H); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("self-demote should be 400, got %d", resp.StatusCode)
	}

	// Delete the created user.
	if resp, _ := doReq(t, admin, http.MethodDelete, h.pool.URL+"/admin/users/"+uid, "", H); resp.StatusCode != http.StatusOK {
		t.Fatalf("delete user: %d", resp.StatusCode)
	}
	if users := getArray(t, admin, h.pool.URL+"/admin/users"); len(users) != 1 {
		t.Fatalf("expected 1 user after delete, got %d", len(users))
	}

	// Registration toggle: disable → a stranger cannot register; re-enable → can.
	if resp, _ := doReq(t, admin, http.MethodPatch, h.pool.URL+"/admin/settings", `{"allow_registration":false}`, H); resp.StatusCode != http.StatusOK {
		t.Fatalf("disable registration: %d", resp.StatusCode)
	}
	stranger := jarClient(t)
	if resp, _ := doReq(t, stranger, http.MethodPost, h.pool.URL+"/auth/register", `{"email":"c@x.io","password":"password123"}`, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("registration disabled should 403, got %d", resp.StatusCode)
	}
	if resp, _ := doReq(t, admin, http.MethodPatch, h.pool.URL+"/admin/settings", `{"allow_registration":true}`, H); resp.StatusCode != http.StatusOK {
		t.Fatalf("re-enable registration: %d", resp.StatusCode)
	}
	if resp, _ := doReq(t, stranger, http.MethodPost, h.pool.URL+"/auth/register", `{"email":"c@x.io","password":"password123"}`, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("registration re-enabled should succeed, got %d", resp.StatusCode)
	}
}
