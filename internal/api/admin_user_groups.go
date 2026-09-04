package api

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"codex-account-pool/internal/storage"
	"github.com/google/uuid"
)

// adminUserGroups handles collection-level operations on user_groups.
//
//	GET  /admin/user-groups  — list all user groups
//	POST /admin/user-groups  — create a user group; generates a "ug_" prefixed UUID
func (s *Server) adminUserGroups(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		groups, err := s.store.ListUserGroups(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if groups == nil {
			groups = []storage.UserGroup{}
		}
		writeJSON(w, http.StatusOK, groups)
	case http.MethodPost:
		var req storage.UserGroup
		if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if strings.TrimSpace(req.Name) == "" {
			writeError(w, http.StatusBadRequest, errors.New("name required"))
			return
		}
		if len(req.Targets) == 0 {
			writeError(w, http.StatusBadRequest, errors.New("at least one target required"))
			return
		}
		if err := normalizeUserGroupInstructionConfig(&req); err != nil {
			writePoolCodeError(w, http.StatusUnprocessableEntity, "invalid_model_instruction_policy", err.Error())
			return
		}
		if err := s.validateUserGroupEgressRPMBalance(r, req); err != nil {
			writePoolCodeError(w, http.StatusUnprocessableEntity, "invalid_egress_rpm_balance", err.Error())
			return
		}
		if err := s.validateUserGroupSuperInstructConfig(r, &req); err != nil {
			writePoolCodeError(w, http.StatusUnprocessableEntity, "invalid_super_instruct_policy", err.Error())
			return
		}
		req.ID = "ug_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		if err := s.store.CreateUserGroupDefinition(r.Context(), req); err != nil {
			writeError(w, http.StatusUnprocessableEntity, err)
			return
		}
		created, ok, err := s.store.GetUserGroup(r.Context(), req.ID)
		if err != nil || !ok {
			writeJSON(w, http.StatusCreated, req)
			return
		}
		writeJSON(w, http.StatusCreated, created)
	default:
		methodNotAllowed(w)
	}
}

// adminUserGroupsAction dispatches sub-paths under /admin/user-groups/:
//
//	GET/PUT/DELETE /admin/user-groups/{id}
//	GET/POST       /admin/user-groups/{id}/targets
//	DELETE         /admin/user-groups/{id}/targets/{tid}
func (s *Server) adminUserGroupsAction(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/user-groups/"), "/")
	parts := strings.Split(rest, "/")
	switch len(parts) {
	case 1:
		s.adminUserGroupsItem(w, r, parts[0])
	case 2:
		if parts[1] == "targets" {
			s.adminUserGroupTargets(w, r, parts[0])
		} else {
			http.NotFound(w, r)
		}
	case 3:
		if parts[1] == "targets" {
			s.adminUserGroupTargetItem(w, r, parts[0], parts[2])
		} else {
			http.NotFound(w, r)
		}
	default:
		http.NotFound(w, r)
	}
}

// adminUserGroupsItem handles GET/PUT/DELETE /admin/user-groups/{id}.
func (s *Server) adminUserGroupsItem(w http.ResponseWriter, r *http.Request, id string) {
	if id == "" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		g, ok, err := s.store.GetUserGroup(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		if g.Targets == nil {
			g.Targets = []storage.TargetRef{}
		}
		writeJSON(w, http.StatusOK, g)
	case http.MethodPut:
		var req storage.UserGroup
		if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if strings.TrimSpace(req.Name) == "" {
			writeError(w, http.StatusBadRequest, errors.New("name required"))
			return
		}
		req.ID = id
		if len(req.Targets) == 0 {
			writeError(w, http.StatusBadRequest, errors.New("at least one target required"))
			return
		}
		if err := normalizeUserGroupInstructionConfig(&req); err != nil {
			writePoolCodeError(w, http.StatusUnprocessableEntity, "invalid_model_instruction_policy", err.Error())
			return
		}
		if err := s.validateUserGroupEgressRPMBalance(r, req); err != nil {
			writePoolCodeError(w, http.StatusUnprocessableEntity, "invalid_egress_rpm_balance", err.Error())
			return
		}
		if err := s.validateUserGroupSuperInstructConfig(r, &req); err != nil {
			writePoolCodeError(w, http.StatusUnprocessableEntity, "invalid_super_instruct_policy", err.Error())
			return
		}
		if err := s.store.ReplaceUserGroupDefinition(r.Context(), req); err != nil {
			if errors.Is(err, storage.ErrUserGroupNotFound) {
				http.NotFound(w, r)
				return
			}
			writeError(w, http.StatusUnprocessableEntity, err)
			return
		}
		updated, ok, err := s.store.GetUserGroup(r.Context(), id)
		if err != nil || !ok {
			writeJSON(w, http.StatusOK, req)
			return
		}
		writeJSON(w, http.StatusOK, updated)
	case http.MethodDelete:
		if err := s.store.DeleteUserGroup(r.Context(), id); err != nil {
			if errors.Is(err, storage.ErrTargetInUse) {
				writePoolCodeError(w, http.StatusConflict, "user_group_in_use", err.Error())
				return
			}
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"deleted": id})
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) validateUserGroupEgressRPMBalance(r *http.Request, group storage.UserGroup) error {
	if !group.EgressRPMBalanceEnabled {
		return nil
	}
	if group.EgressRPMBalanceThreshold <= 0 {
		return errors.New("egress rpm balance threshold must be positive when enabled")
	}
	if len(normalizeNonEmptyStrings(group.EgressRPMBalanceEgressIDs)) == 0 {
		return errors.New("egress rpm balance requires at least one egress")
	}
	return s.validateOrderedEgressIDs(r, group.EgressRPMBalanceEgressIDs)
}

// adminUserGroupTargets handles GET/POST /admin/user-groups/{id}/targets.
func (s *Server) adminUserGroupTargets(w http.ResponseWriter, r *http.Request, groupID string) {
	if groupID == "" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		targets, err := s.store.GetUserGroupTargetRefsWithLegacyIDs(r.Context(), groupID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if targets == nil {
			targets = []storage.TargetRefWithLegacyID{}
		}
		writeJSON(w, http.StatusOK, targets)
	case http.MethodPost:
		var req storage.TargetRef
		if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if strings.TrimSpace(req.Kind) == "" {
			writeError(w, http.StatusBadRequest, errors.New("target kind required"))
			return
		}
		group, ok, err := s.store.GetUserGroup(r.Context(), groupID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		group.Targets = append(group.Targets, req)
		if err := s.store.ReplaceUserGroupDefinition(r.Context(), group); err != nil {
			writeError(w, http.StatusUnprocessableEntity, err)
			return
		}
		targets, err := s.store.GetUserGroupTargetRefsWithLegacyIDs(r.Context(), groupID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if targets == nil {
			targets = []storage.TargetRefWithLegacyID{}
		}
		writeJSON(w, http.StatusCreated, targets)
	default:
		methodNotAllowed(w)
	}
}

// adminUserGroupTargetItem handles DELETE /admin/user-groups/{id}/targets/{tid}.
func (s *Server) adminUserGroupTargetItem(w http.ResponseWriter, r *http.Request, groupID, tidStr string) {
	if groupID == "" || tidStr == "" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodDelete {
		methodNotAllowed(w)
		return
	}
	tid, err := strconv.ParseInt(tidStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid target id: %w", err))
		return
	}
	if err := s.store.RemoveUserGroupTargetForGroup(r.Context(), groupID, tid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"deleted": tid})
}

// adminAPIKeySetUserGroup handles POST /admin/api-keys/{hash}/user-group.
// It links (or clears) the user_group_id on the given api key.
func (s *Server) adminAPIKeySetUserGroup(w http.ResponseWriter, r *http.Request, keyHash string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		UserGroupID string `json:"user_group_id"`
	}
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	userGroupID := strings.TrimSpace(req.UserGroupID)
	if userGroupID != "" {
		if _, ok, err := s.store.GetUserGroup(r.Context(), userGroupID); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		} else if !ok {
			writeError(w, http.StatusBadRequest, fmt.Errorf("user_group %q not found", userGroupID))
			return
		}
	}
	if err := s.store.SetAPIKeyUserGroup(r.Context(), keyHash, userGroupID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"key_hash":      keyHash,
		"user_group_id": userGroupID,
	})
}
