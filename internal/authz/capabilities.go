// Package authz defines the stable authorization vocabulary shared by the API
// and browser bootstrap. Roles are only a convenient capability bundle; route
// protection always asks for an explicit capability.
package authz

import "strings"

type Capability string

const (
	PortalRead              Capability = "portal.read"
	PortalKeysManageSelf    Capability = "portal.keys.manage_self"
	PortalProfileManageSelf Capability = "portal.profile.manage_self"
	AdminAccountsRead       Capability = "admin.accounts.read"
	AdminAccountsWrite      Capability = "admin.accounts.write"
	AdminUsersManage        Capability = "admin.users.manage"
	AdminSettingsManage     Capability = "admin.settings.manage"
	AdminExportsSensitive   Capability = "admin.exports.sensitive"
	AdminBootstrapRecover   Capability = "admin.bootstrap.recover"
)

var userCapabilities = []Capability{
	PortalRead,
	PortalKeysManageSelf,
	PortalProfileManageSelf,
}

var adminCapabilities = []Capability{
	PortalRead,
	PortalKeysManageSelf,
	PortalProfileManageSelf,
	AdminAccountsRead,
	AdminAccountsWrite,
	AdminUsersManage,
	AdminSettingsManage,
	AdminExportsSensitive,
	AdminBootstrapRecover,
}

// ForRole returns a copy so callers cannot mutate the process-wide bundles.
func ForRole(role string) []Capability {
	var source []Capability
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "admin":
		source = adminCapabilities
	case "user":
		source = userCapabilities
	default:
		return []Capability{}
	}
	return append([]Capability(nil), source...)
}

func Allows(role string, required Capability) bool {
	for _, granted := range ForRole(role) {
		if granted == required {
			return true
		}
	}
	return false
}

func safeMethod(method string) bool {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case "GET", "HEAD", "OPTIONS":
		return true
	default:
		return false
	}
}

// RequiredForRoute is the central route-to-method capability registry. The
// /admin catch-all is intentional: newly registered admin handlers are denied by
// default even if their author forgets a handler-local authorization call.
func RequiredForRoute(method, path string) (Capability, bool) {
	path = "/" + strings.TrimLeft(strings.TrimSpace(path), "/")
	if path == "/user" || strings.HasPrefix(path, "/user/") {
		switch {
		case strings.HasPrefix(path, "/user/api-keys"):
			return PortalKeysManageSelf, true
		case strings.HasPrefix(path, "/user/profile"), strings.HasPrefix(path, "/user/sessions"):
			return PortalProfileManageSelf, true
		default:
			return PortalRead, true
		}
	}
	if path != "/admin" && !strings.HasPrefix(path, "/admin/") {
		return "", false
	}

	switch {
	case strings.HasPrefix(path, "/admin/accounts/export"),
		strings.HasPrefix(path, "/admin/export/"):
		return AdminExportsSensitive, true
	case strings.HasPrefix(path, "/admin/users"),
		strings.HasPrefix(path, "/admin/tenants"),
		strings.HasPrefix(path, "/admin/projects"),
		strings.HasPrefix(path, "/admin/api-keys"),
		strings.HasPrefix(path, "/admin/user-groups"):
		return AdminUsersManage, true
	case strings.HasPrefix(path, "/admin/settings"),
		strings.HasPrefix(path, "/admin/config"),
		strings.HasPrefix(path, "/admin/pricing"),
		strings.HasPrefix(path, "/admin/thinking"),
		strings.HasPrefix(path, "/admin/moderation"),
		strings.HasPrefix(path, "/admin/model-instructions"),
		strings.HasPrefix(path, "/admin/super-instruct"):
		return AdminSettingsManage, true
	case strings.HasPrefix(path, "/admin/accounts"):
		if safeMethod(method) {
			return AdminAccountsRead, true
		}
		return AdminAccountsWrite, true
	default:
		if safeMethod(method) {
			return AdminAccountsRead, true
		}
		return AdminAccountsWrite, true
	}
}
