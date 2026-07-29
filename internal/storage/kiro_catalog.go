package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

type KiroModelDescriptor struct {
	AccountID       string   `json:"account_id"`
	CapabilityKey   string   `json:"capability_key"`
	UpstreamID      string   `json:"upstream_id"`
	PublicID        string   `json:"public_id"`
	Aliases         []string `json:"aliases,omitempty"`
	Region          string   `json:"region"`
	Default         bool     `json:"default"`
	MaxInputTokens  int64    `json:"max_input_tokens"`
	MaxOutputTokens int64    `json:"max_output_tokens"`
	ThinkingJSON    string   `json:"thinking_json"`
	EffortJSON      string   `json:"effort_json"`
	Source          string   `json:"source"`
	Generation      int64    `json:"generation"`
	ObservedAt      int64    `json:"observed_at"`
	ExpiresAt       int64    `json:"expires_at"`
	Complete        bool     `json:"complete"`
	RawJSONHash     string   `json:"raw_json_hash,omitempty"`
}

type KiroProbeState struct {
	AccountID      string `json:"account_id"`
	CapabilityKey  string `json:"capability_key"`
	Region         string `json:"region"`
	EndpointHash   string `json:"endpoint_hash"`
	GovernanceKey  string `json:"governance_key"`
	Source         string `json:"source"`
	LastSuccessAt  int64  `json:"last_success_at"`
	LastErrorAt    int64  `json:"last_error_at"`
	LastErrorClass string `json:"last_error_class,omitempty"`
	ExpiresAt      int64  `json:"expires_at"`
	Generation     int64  `json:"generation"`
	PageCount      int    `json:"page_count"`
	Complete       bool   `json:"complete"`
}

// ReplaceKiroModelCatalog atomically replaces one complete, non-empty catalog.
// Callers must not invoke it for partial pagination or anomalous empty responses,
// which leaves the previous generation intact.
func (s *Store) ReplaceKiroModelCatalog(ctx context.Context, state KiroProbeState, models []KiroModelDescriptor) error {
	state.AccountID = strings.TrimSpace(state.AccountID)
	state.CapabilityKey = strings.TrimSpace(state.CapabilityKey)
	if state.AccountID == "" || state.CapabilityKey == "" {
		return errors.New("Kiro catalog account and capability key are required")
	}
	if !state.Complete || len(models) == 0 {
		return errors.New("Kiro catalog replacement requires a complete non-empty pagination result")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM kiro_model_catalog WHERE account_id=? AND capability_key=?`, state.AccountID, state.CapabilityKey); err != nil {
		return err
	}
	for _, model := range models {
		if strings.TrimSpace(model.AccountID) == "" {
			model.AccountID = state.AccountID
		}
		if strings.TrimSpace(model.CapabilityKey) == "" {
			model.CapabilityKey = state.CapabilityKey
		}
		if model.AccountID != state.AccountID || model.CapabilityKey != state.CapabilityKey ||
			strings.TrimSpace(model.UpstreamID) == "" || strings.TrimSpace(model.PublicID) == "" || !model.Complete {
			return errors.New("Kiro catalog contains an incomplete or cross-scope descriptor")
		}
		aliases, _ := json.Marshal(model.Aliases)
		if model.ObservedAt == 0 {
			model.ObservedAt = Now()
		}
		if model.Generation == 0 {
			model.Generation = state.Generation
		}
		_, err = tx.ExecContext(ctx, `
INSERT INTO kiro_model_catalog(
 account_id,capability_key,upstream_id,public_id,aliases_json,region,is_default,
 max_input_tokens,max_output_tokens,thinking_json,effort_json,source,generation,
 observed_at,expires_at,complete,raw_json_hash)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			model.AccountID, model.CapabilityKey, model.UpstreamID, model.PublicID, string(aliases), model.Region, boolInt(model.Default),
			model.MaxInputTokens, model.MaxOutputTokens, normalizedJSONObject(model.ThinkingJSON), normalizedJSONObject(model.EffortJSON),
			model.Source, model.Generation, model.ObservedAt, model.ExpiresAt, boolInt(model.Complete), model.RawJSONHash)
		if err != nil {
			return err
		}
	}
	state.LastSuccessAt = Now()
	state.LastErrorAt = 0
	state.LastErrorClass = ""
	if _, err = tx.ExecContext(ctx, `
INSERT INTO kiro_probe_state(
 account_id,capability_key,region,endpoint_hash,governance_key,source,last_success_at,
 last_error_at,last_error_class,expires_at,generation,page_count,complete)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(account_id,capability_key) DO UPDATE SET
 region=excluded.region,endpoint_hash=excluded.endpoint_hash,governance_key=excluded.governance_key,
 source=excluded.source,last_success_at=excluded.last_success_at,last_error_at=0,last_error_class='',
 expires_at=excluded.expires_at,generation=excluded.generation,page_count=excluded.page_count,complete=excluded.complete`,
		state.AccountID, state.CapabilityKey, state.Region, state.EndpointHash, state.GovernanceKey, state.Source, state.LastSuccessAt,
		0, "", state.ExpiresAt, state.Generation, state.PageCount, boolInt(state.Complete)); err != nil {
		return err
	}
	return tx.Commit()
}

func normalizedJSONObject(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || !json.Valid([]byte(raw)) {
		return "{}"
	}
	return raw
}

func (s *Store) RecordKiroProbeError(ctx context.Context, state KiroProbeState) error {
	if strings.TrimSpace(state.AccountID) == "" || strings.TrimSpace(state.CapabilityKey) == "" {
		return errors.New("Kiro probe scope is required")
	}
	state.LastErrorAt = Now()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO kiro_probe_state(
 account_id,capability_key,region,endpoint_hash,governance_key,source,last_success_at,
 last_error_at,last_error_class,expires_at,generation,page_count,complete)
VALUES(?,?,?,?,?,?,0,?,?,?,?,?,0)
ON CONFLICT(account_id,capability_key) DO UPDATE SET
 last_error_at=excluded.last_error_at,last_error_class=excluded.last_error_class,
 region=excluded.region,endpoint_hash=excluded.endpoint_hash,governance_key=excluded.governance_key,
 source=excluded.source`,
		state.AccountID, state.CapabilityKey, state.Region, state.EndpointHash, state.GovernanceKey, state.Source,
		state.LastErrorAt, safeProbeErrorClass(state.LastErrorClass), state.ExpiresAt, state.Generation, state.PageCount)
	return err
}

func safeProbeErrorClass(class string) string {
	switch strings.ToLower(strings.TrimSpace(class)) {
	case "auth", "governance", "rate_limit", "network", "upstream", "invalid_response", "empty_catalog", "pagination":
		return strings.ToLower(strings.TrimSpace(class))
	default:
		return "internal"
	}
}

func (s *Store) ListKiroModelCatalog(ctx context.Context, accountID, capabilityKey string) ([]KiroModelDescriptor, error) {
	query := `
SELECT account_id,capability_key,upstream_id,public_id,aliases_json,region,is_default,
 max_input_tokens,max_output_tokens,thinking_json,effort_json,source,generation,
 observed_at,expires_at,complete,raw_json_hash
FROM kiro_model_catalog WHERE account_id=?`
	args := []interface{}{accountID}
	if strings.TrimSpace(capabilityKey) != "" {
		query += ` AND capability_key=?`
		args = append(args, capabilityKey)
	}
	query += ` ORDER BY is_default DESC,public_id,upstream_id`
	rows, err := s.rdb.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KiroModelDescriptor
	for rows.Next() {
		var model KiroModelDescriptor
		var aliases string
		var isDefault, complete int
		if err := rows.Scan(&model.AccountID, &model.CapabilityKey, &model.UpstreamID, &model.PublicID, &aliases, &model.Region, &isDefault,
			&model.MaxInputTokens, &model.MaxOutputTokens, &model.ThinkingJSON, &model.EffortJSON, &model.Source, &model.Generation,
			&model.ObservedAt, &model.ExpiresAt, &complete, &model.RawJSONHash); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(aliases), &model.Aliases)
		model.Default = isDefault != 0
		model.Complete = complete != 0
		out = append(out, model)
	}
	return out, rows.Err()
}

func (s *Store) GetKiroProbeState(ctx context.Context, accountID, capabilityKey string) (KiroProbeState, error) {
	var state KiroProbeState
	var complete int
	err := s.rdb.QueryRowContext(ctx, `
SELECT account_id,capability_key,region,endpoint_hash,governance_key,source,last_success_at,
 last_error_at,last_error_class,expires_at,generation,page_count,complete
FROM kiro_probe_state WHERE account_id=? AND capability_key=?`, accountID, capabilityKey).Scan(
		&state.AccountID, &state.CapabilityKey, &state.Region, &state.EndpointHash, &state.GovernanceKey, &state.Source,
		&state.LastSuccessAt, &state.LastErrorAt, &state.LastErrorClass, &state.ExpiresAt, &state.Generation, &state.PageCount, &complete)
	state.Complete = complete != 0
	if errors.Is(err, sql.ErrNoRows) {
		return state, sql.ErrNoRows
	}
	return state, err
}
