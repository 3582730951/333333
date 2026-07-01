package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
)

// adminNodeRegistrarConfig gets/sets the Node registration engine's operator config — the
// "node_registrar_config" setting holding the hero-sms key, mailbox + residential-proxy
// credentials and phone-country catalog. Exposing it over the admin API lets an operator
// configure the registrar ENTIRELY from the web UI, with no SSH to the VPS and no editing
// of other_new_gpt_register/config.server.json. The orchestrator
// (registration/pipeline/registrar_node.go nodeRegistrarBaseConfig) merges this object,
// at highest precedence, over any on-disk config at registration time.
//
//	GET  /admin/register/node-config  -> the stored config object ({} when unset)
//	POST /admin/register/node-config  <- a JSON object to store (full replace)
func (s *Server) adminNodeRegistrarConfig(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		out := map[string]interface{}{}
		v, ok, err := s.store.GetSetting(r.Context(), "node_registrar_config")
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if ok {
			if v = strings.TrimSpace(v); v != "" {
				// Tolerate a legacy/garbled value rather than 500 — the form then starts blank.
				if err := json.Unmarshal([]byte(v), &out); err != nil {
					log.Printf("[REGISTRAR-CONFIG-WARN] stored node_registrar_config is invalid JSON: %v", err)
				}
			}
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost, http.MethodPut:
		var cfg map[string]interface{}
		if err := decodeJSONRequestBody(r.Body, &cfg, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("body must be a JSON object: %w", err))
			return
		}
		// A blank string field clears the key (so emptying a field in the UI removes it)
		// rather than persisting an empty override that would shadow the on-disk default.
		for k, v := range cfg {
			if sv, ok := v.(string); ok && strings.TrimSpace(sv) == "" {
				delete(cfg, k)
			}
		}
		raw, err := json.Marshal(cfg)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := s.store.SetSetting(r.Context(), "node_registrar_config", string(raw)); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, cfg)
	default:
		methodNotAllowed(w)
	}
}
