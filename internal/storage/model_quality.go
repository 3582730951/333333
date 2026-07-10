package storage

import (
	"context"
	"database/sql"
	"errors"
)

// ModelQualityStatus is the current group×model health verdict. It deliberately
// has no per-account state: each check samples the normal scheduler for the group,
// so the verdict describes the route users actually consume.
type ModelQualityStatus struct {
	GroupName            string `json:"group_name"`
	ModelSlug            string `json:"model"`
	Provider             string `json:"provider"`
	State                string `json:"state"`
	LastOutcome          string `json:"last_outcome"`
	LastProbeAt          int64  `json:"last_probe_at"`
	LastPassAt           int64  `json:"last_pass_at"`
	ConsecutiveAnomalies int    `json:"consecutive_anomalies"`
	ConsecutiveErrors    int    `json:"consecutive_errors"`
	TotalChecks          int    `json:"total_checks"`
	TotalTokens          int64  `json:"total_tokens"`
	LastProbeID          string `json:"last_probe_id"`
	LastExpected         string `json:"last_expected"`
	LastActual           string `json:"last_actual"`
	LastReturnedModel    string `json:"last_returned_model"`
	LastLatencyMS        int64  `json:"last_latency_ms"`
	UpdatedAt            int64  `json:"updated_at"`
}

// ModelQualityRun is one cheap primary probe or the confirmation suite that is
// emitted only after a primary anomaly.
type ModelQualityRun struct {
	ID               int64  `json:"id"`
	GroupName        string `json:"group_name"`
	ModelSlug        string `json:"model"`
	Provider         string `json:"provider"`
	AccountID        string `json:"account_id,omitempty"`
	ProbeID          string `json:"probe_id"`
	Phase            string `json:"phase"`
	Outcome          string `json:"outcome"`
	Expected         string `json:"expected"`
	Actual           string `json:"actual"`
	ReturnedModel    string `json:"returned_model"`
	HTTPStatus       int    `json:"http_status"`
	ErrorKind        string `json:"error_kind,omitempty"`
	ErrorMessage     string `json:"error_message,omitempty"`
	LatencyMS        int64  `json:"latency_ms"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	TotalTokens      int64  `json:"total_tokens"`
	CreatedAt        int64  `json:"created_at"`
}

func (s *Store) InsertModelQualityRun(ctx context.Context, run ModelQualityRun) (int64, error) {
	if run.CreatedAt == 0 {
		run.CreatedAt = Now()
	}
	result, err := s.db.ExecContext(ctx, `
INSERT INTO model_quality_runs(group_name, model_slug, provider, account_id, probe_id, phase, outcome, expected, actual, returned_model, http_status, error_kind, error_message, latency_ms, prompt_tokens, completion_tokens, total_tokens, created_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.GroupName, run.ModelSlug, run.Provider, run.AccountID, run.ProbeID, run.Phase, run.Outcome,
		run.Expected, run.Actual, run.ReturnedModel, run.HTTPStatus, run.ErrorKind, run.ErrorMessage,
		run.LatencyMS, run.PromptTokens, run.CompletionTokens, run.TotalTokens, run.CreatedAt)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (s *Store) UpsertModelQualityStatus(ctx context.Context, status ModelQualityStatus) error {
	if status.UpdatedAt == 0 {
		status.UpdatedAt = Now()
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO model_quality_status(group_name, model_slug, provider, state, last_outcome, last_probe_at, last_pass_at, consecutive_anomalies, consecutive_errors, total_checks, total_tokens, last_probe_id, last_expected, last_actual, last_returned_model, last_latency_ms, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(group_name, model_slug, provider) DO UPDATE SET
 state=excluded.state,
 last_outcome=excluded.last_outcome,
 last_probe_at=excluded.last_probe_at,
 last_pass_at=excluded.last_pass_at,
 consecutive_anomalies=excluded.consecutive_anomalies,
 consecutive_errors=excluded.consecutive_errors,
 total_checks=excluded.total_checks,
 total_tokens=excluded.total_tokens,
 last_probe_id=excluded.last_probe_id,
 last_expected=excluded.last_expected,
 last_actual=excluded.last_actual,
 last_returned_model=excluded.last_returned_model,
 last_latency_ms=excluded.last_latency_ms,
 updated_at=excluded.updated_at`,
		status.GroupName, status.ModelSlug, status.Provider, status.State, status.LastOutcome,
		status.LastProbeAt, status.LastPassAt, status.ConsecutiveAnomalies, status.ConsecutiveErrors,
		status.TotalChecks, status.TotalTokens, status.LastProbeID, status.LastExpected, status.LastActual,
		status.LastReturnedModel, status.LastLatencyMS, status.UpdatedAt)
	return err
}

func (s *Store) GetModelQualityStatus(ctx context.Context, group, model, provider string) (ModelQualityStatus, bool, error) {
	row := s.rdb.QueryRowContext(ctx, `SELECT group_name, model_slug, provider, state, last_outcome, last_probe_at, last_pass_at, consecutive_anomalies, consecutive_errors, total_checks, total_tokens, last_probe_id, last_expected, last_actual, last_returned_model, last_latency_ms, updated_at FROM model_quality_status WHERE group_name=? AND model_slug=? AND provider=?`, group, model, provider)
	status, err := scanModelQualityStatus(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ModelQualityStatus{}, false, nil
	}
	return status, err == nil, err
}

func (s *Store) ListModelQualityStatuses(ctx context.Context, group, model string) ([]ModelQualityStatus, error) {
	query := `SELECT group_name, model_slug, provider, state, last_outcome, last_probe_at, last_pass_at, consecutive_anomalies, consecutive_errors, total_checks, total_tokens, last_probe_id, last_expected, last_actual, last_returned_model, last_latency_ms, updated_at FROM model_quality_status WHERE (?='' OR group_name=?) AND (?='' OR model_slug=?) ORDER BY group_name, model_slug, provider`
	rows, err := s.rdb.QueryContext(ctx, query, group, group, model, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ModelQualityStatus{}
	for rows.Next() {
		status, err := scanModelQualityStatus(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, status)
	}
	return out, rows.Err()
}

func scanModelQualityStatus(scan func(...interface{}) error) (ModelQualityStatus, error) {
	var status ModelQualityStatus
	err := scan(&status.GroupName, &status.ModelSlug, &status.Provider, &status.State, &status.LastOutcome,
		&status.LastProbeAt, &status.LastPassAt, &status.ConsecutiveAnomalies, &status.ConsecutiveErrors,
		&status.TotalChecks, &status.TotalTokens, &status.LastProbeID, &status.LastExpected, &status.LastActual,
		&status.LastReturnedModel, &status.LastLatencyMS, &status.UpdatedAt)
	return status, err
}

func (s *Store) ListModelQualityRuns(ctx context.Context, group, model string, limit int) ([]ModelQualityRun, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT id, group_name, model_slug, provider, account_id, probe_id, phase, outcome, expected, actual, returned_model, http_status, error_kind, error_message, latency_ms, prompt_tokens, completion_tokens, total_tokens, created_at FROM model_quality_runs WHERE (?='' OR group_name=?) AND (?='' OR model_slug=?) ORDER BY created_at DESC, id DESC LIMIT ?`, group, group, model, model, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ModelQualityRun{}
	for rows.Next() {
		var run ModelQualityRun
		if err := rows.Scan(&run.ID, &run.GroupName, &run.ModelSlug, &run.Provider, &run.AccountID, &run.ProbeID,
			&run.Phase, &run.Outcome, &run.Expected, &run.Actual, &run.ReturnedModel, &run.HTTPStatus,
			&run.ErrorKind, &run.ErrorMessage, &run.LatencyMS, &run.PromptTokens, &run.CompletionTokens,
			&run.TotalTokens, &run.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

func (s *Store) PurgeModelQualityRunsBefore(ctx context.Context, cutoff int64) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM model_quality_runs WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *Store) DeleteModelQualityStatus(ctx context.Context, group, model, provider string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM model_quality_status WHERE (?='' OR group_name=?) AND (?='' OR model_slug=?) AND (?='' OR provider=?)`, group, group, model, model, provider, provider)
	return err
}
