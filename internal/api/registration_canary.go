package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"codex-account-pool/internal/registration/pipeline"
	"codex-account-pool/internal/registration/provider"
)

var errRegistrationCanaryRequired = errors.New("registration method requires a successful canary for the current configuration")

type registrationMethodReadiness struct {
	Method      string   `json:"method"`
	Ready       bool     `json:"ready"`
	CanaryReady bool     `json:"canary_ready"`
	Blockers    []string `json:"blockers"`
	Fingerprint string   `json:"-"`
}

func (h *Handler) registrationMethodReadiness(ctx context.Context, req pipeline.RegisterRequest) (registrationMethodReadiness, error) {
	out := registrationMethodReadiness{
		Method:   strings.ToLower(strings.TrimSpace(req.Method)),
		Blockers: make([]string, 0, 4),
	}
	hasher := sha256.New()
	writeRegistrationFingerprint(hasher, "method", out.Method)
	writeRegistrationFingerprint(hasher, "identity_mode", strings.ToLower(strings.TrimSpace(req.IdentityMode)))

	for _, artifact := range registrationArtifacts(out.Method) {
		digest, err := secureArtifactDigest(artifact)
		if err != nil {
			out.Blockers = append(out.Blockers, "required registrar artifact is unavailable")
			continue
		}
		writeRegistrationFingerprint(hasher, "artifact", filepath.Base(artifact)+":"+digest)
	}
	if runtimeName := registrationRuntime(out.Method); runtimeName != "" {
		runtimePath, err := exec.LookPath(runtimeName)
		if err != nil {
			out.Blockers = append(out.Blockers, "required registrar runtime is unavailable")
		} else {
			writeRegistrationFingerprint(hasher, "runtime", filepath.Base(runtimePath))
		}
	}

	providerRows, err := h.registrationProviderFingerprint(ctx)
	if err != nil {
		return out, err
	}
	for _, row := range providerRows {
		writeRegistrationFingerprint(hasher, "provider", row)
	}
	providerCounts := map[string]int{}
	for _, row := range providerRows {
		parts := strings.SplitN(row, "\x00", 3)
		if len(parts) >= 2 {
			providerCounts[parts[0]]++
			providerCounts[parts[0]+"/"+parts[1]]++
		}
	}
	manager, err := provider.BuildManagerWithError(ctx, h.store, h.httpClient)
	if err != nil {
		return out, err
	}
	providerCounts["sms"] = len(manager.SMS)
	providerCounts["mailbox"] = len(manager.Mailbox)
	providerCounts["captcha"] = len(manager.Captcha)
	emailOTPReady, err := h.authenticatedEmailOTPProviderReady(ctx)
	if err != nil {
		return out, err
	}
	mailboxRelayReady, selectedMailbox, err := h.mailboxRelayProviderReady(ctx, req.MailboxProvider, manager)
	if err != nil {
		return out, err
	}
	writeRegistrationFingerprint(hasher, "mailbox_provider", selectedMailbox)
	writeRegistrationFingerprint(hasher, "mailbox_domain", strings.ToLower(strings.TrimSpace(req.MailboxDomain)))
	switch out.Method {
	case "protocol":
		if strings.EqualFold(req.IdentityMode, "email") {
			if providerCounts["mailbox"] == 0 {
				out.Blockers = append(out.Blockers, "mailbox provider is not configured")
			}
		} else if providerCounts["sms"] == 0 {
			out.Blockers = append(out.Blockers, "SMS provider is not configured")
		}
	case "protocol_v2":
		if !emailOTPReady && !mailboxRelayReady {
			out.Blockers = append(out.Blockers, "authenticated email OTP provider is not configured")
		}
	case "node", "browser":
		if providerCounts["sms"] == 0 {
			out.Blockers = append(out.Blockers, "SMS provider is not configured")
		}
	case "browser_v3":
		if !emailOTPReady && !mailboxRelayReady {
			out.Blockers = append(out.Blockers, "authenticated email OTP provider is not configured")
		}
		if providerCounts["sms"] == 0 {
			out.Blockers = append(out.Blockers, "SMS provider is not configured for the phone challenge")
		}
	}

	if err := h.registrationEgressFingerprint(ctx, req, hasher); err != nil {
		out.Blockers = append(out.Blockers, "registration egress is not ready")
	}
	out.Fingerprint = hex.EncodeToString(hasher.Sum(nil))
	out.Ready = len(out.Blockers) == 0
	if out.Ready {
		out.CanaryReady, err = h.store.RegistrationCanaryPassed(ctx, out.Method, out.Fingerprint)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

func (h *Handler) mailboxRelayProviderReady(
	ctx context.Context,
	requested string,
	manager *provider.Manager,
) (bool, string, error) {
	selected := strings.ToLower(strings.TrimSpace(requested))
	if selected == "" {
		value, ok, err := h.store.GetSetting(ctx, "reg_default_mailbox")
		if err != nil {
			return false, "", err
		}
		if ok {
			selected = strings.ToLower(strings.TrimSpace(value))
		}
	}
	if selected == "" || manager == nil {
		return false, selected, nil
	}
	for _, candidate := range manager.Mailbox {
		name := strings.ToLower(strings.TrimSpace(candidate.Name()))
		if name == selected ||
			(selected == "tempmail" && name == "tempmail_lol") ||
			(selected == "tempmaillol" && name == "tempmail_lol") {
			return true, selected, nil
		}
	}
	return false, selected, nil
}

func (h *Handler) authenticatedEmailOTPProviderReady(ctx context.Context) (bool, error) {
	var configJSON, authJSON string
	err := h.store.DB().QueryRowContext(ctx, `
SELECT config_json,auth_json FROM provider_settings
WHERE provider_type='email' AND provider_key='hotmail_otp' AND enabled=1
LIMIT 1`).Scan(&configJSON, &authJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	config := map[string]interface{}{}
	if strings.TrimSpace(configJSON) != "" {
		if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
			return false, err
		}
	}
	secrets, err := h.store.OpenProviderAuthJSON("email", "hotmail_otp", authJSON)
	if err != nil {
		return false, err
	}
	for field, value := range secrets {
		config[field] = value
	}
	baseEmail, _ := config["base_email"].(string)
	otpURL, _ := config["otp_url"].(string)
	authToken, _ := config["auth_token"].(string)
	parsed, err := url.Parse(strings.TrimSpace(otpURL))
	if err != nil || parsed.Hostname() == "" {
		return false, nil
	}
	secureTransport := parsed.Scheme == "https" ||
		(parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname()))
	return strings.Contains(strings.TrimSpace(baseEmail), "@") &&
		strings.TrimSpace(authToken) != "" && secureTransport, nil
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func registrationArtifacts(method string) []string {
	switch method {
	case "protocol_v2":
		script := firstNonEmpty(os.Getenv("CODEX_REG_PROTOCOL_SCRIPT"), "services/codex_register/protocol_register.py")
		return []string{script, filepath.Join(filepath.Dir(script), "requirements.txt")}
	case "node":
		dir := firstNonEmpty(os.Getenv("CODEX_REG_NODE_DIR"), "workers/node-registrar")
		entry := firstNonEmpty(os.Getenv("CODEX_REG_NODE_ENTRY"), "index.js")
		if !filepath.IsAbs(entry) {
			entry = filepath.Join(dir, entry)
		}
		return []string{
			entry,
			filepath.Join(dir, "package.json"),
			filepath.Join(dir, "package-lock.json"),
			filepath.Join(dir, "sbom.cdx.json"),
			filepath.Join(dir, "node_modules", "playwright", "package.json"),
			filepath.Join(dir, "node_modules", "playwright-core", "package.json"),
		}
	case "browser":
		script := firstNonEmpty(os.Getenv("CODEX_REG_SCRIPT"), "services/codex_register/browser_register.py")
		return []string{script, filepath.Join(filepath.Dir(script), "requirements.txt")}
	case "browser_v3":
		script := firstNonEmpty(os.Getenv("CODEX_REG_V3_SCRIPT"), "services/codex_register/reg_v3.py")
		return []string{
			script,
			filepath.Join(filepath.Dir(script), "phone_verify.py"),
			filepath.Join(filepath.Dir(script), "requirements.txt"),
		}
	default:
		return nil
	}
}

func registrationRuntime(method string) string {
	switch method {
	case "protocol_v2", "browser", "browser_v3":
		return firstNonEmpty(os.Getenv("CODEX_REG_PYTHON"), "python3")
	case "node":
		return firstNonEmpty(os.Getenv("CODEX_REG_NODE"), "node")
	default:
		return ""
	}
}

func secureArtifactDigest(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", errors.New("registrar artifact is not a regular file")
	}
	if info.Mode().Perm()&0o022 != 0 || info.Size() > 16<<20 {
		return "", errors.New("registrar artifact permissions or size are unsafe")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, 16<<20+1)); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (h *Handler) registrationProviderFingerprint(ctx context.Context) ([]string, error) {
	rows, err := h.store.DB().QueryContext(ctx, `
SELECT provider_type,provider_key,config_json,auth_json,updated_at
FROM provider_settings WHERE enabled=1 ORDER BY provider_type,provider_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0, 8)
	for rows.Next() {
		var providerType, providerKey, configJSON, authJSON string
		var updatedAt int64
		if err := rows.Scan(&providerType, &providerKey, &configJSON, &authJSON, &updatedAt); err != nil {
			return nil, err
		}
		out = append(out, strings.Join([]string{
			strings.ToLower(strings.TrimSpace(providerType)),
			strings.ToLower(strings.TrimSpace(providerKey)),
			configJSON, authJSON, fmt.Sprint(updatedAt),
		}, "\x00"))
	}
	return out, rows.Err()
}

func (h *Handler) registrationEgressFingerprint(ctx context.Context, req pipeline.RegisterRequest, hash io.Writer) error {
	if strings.TrimSpace(req.EgressID) != "" {
		egress, err := h.store.GetEgressProfile(ctx, req.EgressID)
		if err != nil || strings.TrimSpace(egress.Endpoint) == "" ||
			strings.EqualFold(egress.Type, "direct") ||
			strings.EqualFold(egress.Health, "disabled") {
			return errors.New("egress unavailable")
		}
		raw, _ := json.Marshal(egress)
		writeRegistrationFingerprint(hash, "egress", string(raw))
		return nil
	}
	pool, err := h.store.GetEgressPool(ctx, req.RegistrationEgressPoolID)
	if err != nil || !strings.EqualFold(strings.TrimSpace(pool.Purpose), "registration") {
		return errors.New("registration egress pool unavailable")
	}
	usable := 0
	for _, member := range pool.Members {
		if !member.Enabled || strings.TrimSpace(member.Egress.Endpoint) == "" ||
			strings.EqualFold(member.Egress.Type, "direct") ||
			strings.EqualFold(member.Egress.Health, "disabled") {
			continue
		}
		usable++
	}
	if usable == 0 {
		return errors.New("registration egress pool has no usable members")
	}
	raw, _ := json.Marshal(pool)
	writeRegistrationFingerprint(hash, "egress_pool", string(raw))
	return nil
}

func writeRegistrationFingerprint(dst io.Writer, domain, value string) {
	_, _ = io.WriteString(dst, domain)
	_, _ = io.WriteString(dst, "\x00")
	_, _ = io.WriteString(dst, value)
	_, _ = io.WriteString(dst, "\x00")
}

func (h *Handler) HandleCanary(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		canaries, err := h.store.ListRegistrationCanaries(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"canaries": canaries})
	case http.MethodPost:
		var req pipeline.RegisterRequest
		if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		jobID, err := h.StartCanary(r.Context(), req)
		if err != nil {
			status := http.StatusServiceUnavailable
			if errors.Is(err, errPaymentFeatureRemoved) {
				status = http.StatusGone
			} else if errors.Is(err, errRegistrationDisabled) {
				status = http.StatusForbidden
			} else if errors.Is(err, errInvalidRegisterRequest) {
				status = http.StatusBadRequest
			}
			writeError(w, status, err)
			return
		}
		w.Header().Set("Location", "/admin/register/jobs/"+jobID)
		writeJSON(w, http.StatusAccepted, map[string]string{"job_id": jobID, "status": "queued"})
	default:
		methodNotAllowed(w)
	}
}

func sortedRegistrationMethods() []string {
	methods := []string{"protocol", "protocol_v2", "node", "browser", "browser_v3"}
	sort.Strings(methods)
	return methods
}
