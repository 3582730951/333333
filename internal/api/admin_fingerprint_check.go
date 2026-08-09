package api

import (
	"net/http"
	"strings"

	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/upstream/tlsclient"
)

// adminEgressFingerprintCheck is the A2 fidelity-diff diagnostic. It fetches a public
// TLS/HTTP2 reflector (default https://tls.peet.ws/api/all) through the in-process
// tls-client engine and, when the target egress is a curl_cffi_sidecar, ALSO through the
// sidecar, then diffs the observed JA3/JA4/Akamai fingerprints. This is how an operator
// verifies, post-deploy on the live VPS, that flipping egress_fingerprint_engine to
// "inprocess" did not change the fingerprint the upstream sees — the non-negotiable
// validation gate before the sidecar is retired.
//
// SECURITY: this makes an outbound request that reveals the chosen egress IP to the
// third-party reflector. It is admin-only and never on the request hot path. Point
// `reflector_url` at a self-hosted reflector to avoid the third-party leak.
//
// POST /admin/egress-fingerprint-check
//
//	{
//	  "egress_id":    "<optional; a curl_cffi_sidecar egress to also test via sidecar>",
//	  "profile":      "<optional; chrome|claude_bun|node|rustls, default chrome>",
//	  "reflector_url":"<optional; default https://tls.peet.ws/api/all>"
//	}
func (s *Server) adminEgressFingerprintCheck(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		EgressID     string `json:"egress_id"`
		Profile      string `json:"profile"`
		ReflectorURL string `json:"reflector_url"`
	}
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	profile := inProcessProfileName(req.Profile)

	// Resolve the egress. When none is supplied, synthesize a direct sidecar-shaped egress
	// so the in-process engine still runs (host-direct); the sidecar leg is then skipped.
	egress := storage.EgressProfile{Type: "curl_cffi_sidecar"}
	if id := strings.TrimSpace(req.EgressID); id != "" {
		got, err := s.store.GetEgressProfile(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		egress = got
	}

	result := map[string]interface{}{
		"profile":       profile,
		"reflector_url": firstNonEmpty(strings.TrimSpace(req.ReflectorURL), "https://tls.peet.ws/api/all"),
	}

	inproc := s.upstream.ReflectFingerprint(r.Context(), egress, "inprocess", profile, req.ReflectorURL)
	result["inprocess"] = inproc

	// Only run the sidecar leg for a sidecar-typed egress with an endpoint; otherwise there
	// is nothing to compare against and the sidecar call would just error.
	if strings.EqualFold(strings.TrimSpace(egress.Type), "curl_cffi_sidecar") && strings.TrimSpace(egress.Endpoint) != "" {
		sc := s.upstream.ReflectFingerprint(r.Context(), egress, "sidecar", profile, req.ReflectorURL)
		result["sidecar"] = sc
		result["match"] = map[string]interface{}{
			"ja3_hash":       inproc.JA3Hash != "" && inproc.JA3Hash == sc.JA3Hash,
			"ja4":            inproc.JA4 != "" && inproc.JA4 == sc.JA4,
			"akamai_hash":    akamaiFingerprintMatches(profile, inproc.AkamaiHash, sc.AkamaiHash),
			"http2_expected": profile != tlsclient.ProfileClaude,
		}
	} else {
		result["sidecar_skipped"] = "no sidecar endpoint on egress; showing in-process fingerprint only"
	}

	writeJSON(w, http.StatusOK, result)
}

// akamaiFingerprintMatches treats the intentional absence of HTTP/2 as a match for
// Claude. The captured Bun ClientHello has no ALPN extension, so both engines should
// report no Akamai/SETTINGS fingerprint; requiring a non-empty hash would incorrectly
// flag the faithful result as a mismatch in the admin diagnostic.
func akamaiFingerprintMatches(profile, inProcess, sidecar string) bool {
	if profile == tlsclient.ProfileClaude {
		return inProcess == "" && sidecar == ""
	}
	return inProcess != "" && inProcess == sidecar
}

// inProcessProfileName maps an admin-supplied profile label onto a tlsclient profile const.
func inProcessProfileName(label string) string {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case tlsclient.ProfileClaude, "claude", "claude-cli", "bun":
		return tlsclient.ProfileClaude
	case tlsclient.ProfileNode, "node_undici", "undici", "kiro":
		return tlsclient.ProfileNode
	case tlsclient.ProfileRustls, "aws_sdk_rust", "amazon_q", "q":
		return tlsclient.ProfileRustls
	default:
		return tlsclient.ProfileChrome
	}
}
