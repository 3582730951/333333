package api

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"

	turboreg "codex-account-pool/internal/turbo_gpt_register"
)

func (s *Server) adminTurboGPTRegisterJobs(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if s.turboGPTRegister == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("turbo gpt register is not initialized"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		jobs, err := s.turboGPTRegister.ListJobs(r.Context(), r.URL.Query().Get("status"), limit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if jobs == nil {
			jobs = []turboreg.Job{}
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"jobs": jobs})
	case http.MethodPost:
		var req struct {
			turboreg.CreateJobRequest
			Start bool `json:"start"`
		}
		if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		job, err := s.turboGPTRegister.CreateJob(r.Context(), req.CreateJobRequest)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if req.Start {
			if err := s.turboGPTRegister.Start(job.ID); err != nil {
				writeError(w, http.StatusConflict, err)
				return
			}
		}
		writeJSON(w, http.StatusCreated, job)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) adminTurboGPTRegisterJobAction(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if s.turboGPTRegister == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("turbo gpt register is not initialized"))
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/turbo-gpt-register/jobs/"), "/")
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
		http.NotFound(w, r)
		return
	}
	id := parts[0]
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			job, err := s.turboGPTRegister.GetJob(r.Context(), id)
			if errors.Is(err, sql.ErrNoRows) {
				http.NotFound(w, r)
				return
			}
			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			writeJSON(w, http.StatusOK, job)
		case http.MethodDelete:
			if err := s.turboGPTRegister.DeleteJob(r.Context(), id); err != nil {
				writeError(w, http.StatusConflict, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
		default:
			methodNotAllowed(w)
		}
		return
	}
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	switch parts[1] {
	case "advance":
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		if err := s.turboGPTRegister.Start(id); err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"job_id": id, "status": "scheduled"})
	case "retry":
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		job, err := s.turboGPTRegister.Retry(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusAccepted, job)
	case "token":
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		token, err := s.turboGPTRegister.GetToken(r.Context(), id)
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, token)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) adminTurboGPTRegisterConfig(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if s.turboGPTRegister == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("turbo gpt register is not initialized"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		values, err := s.turboGPTRegister.GetConfig(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, values)
	case http.MethodPut, http.MethodPatch:
		var values map[string]string
		if err := decodeJSONRequestBody(r.Body, &values, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := s.turboGPTRegister.SetConfig(r.Context(), values); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, values)
	default:
		methodNotAllowed(w)
	}
}
