package storage

import (
	"context"
	"fmt"
	"strings"
)

// ModelAuditRow aggregates the requested→resolved→actual chain without exposing
// prompts, downstream identifiers, or account credentials.
type ModelAuditRow struct {
	RequestedModel      string `json:"requested_model"`
	ResolvedModel       string `json:"resolved_model"`
	ActualModel         string `json:"actual_model"`
	ModelOverrideSource string `json:"model_override_source"`
	Mismatch            bool   `json:"mismatch"`
	MismatchReason      string `json:"mismatch_reason"`
	Requests            int64  `json:"requests"`
	LastSeenAt          int64  `json:"last_seen_at"`
}

type ModelAuditSummary struct {
	Rows                   []ModelAuditRow `json:"rows"`
	Requests               int64           `json:"requests"`
	Mismatches             int64           `json:"mismatches"`
	ActualModelUnavailable int64           `json:"actual_model_unavailable"`
}

func (s *Store) ModelAuditWindow(ctx context.Context, since, until int64, mismatchOnly bool, limit int) (ModelAuditSummary, error) {
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	if since < 0 || (until > 0 && until < since) {
		return ModelAuditSummary{}, fmt.Errorf("invalid model audit window")
	}
	where := []string{"created_at >= ?"}
	args := []interface{}{since}
	if until > 0 {
		where = append(where, "created_at <= ?")
		args = append(args, until)
	}
	if mismatchOnly {
		where = append(where, "model_mismatch = 1")
	}
	var out ModelAuditSummary
	if err := s.rdb.QueryRowContext(ctx, `
SELECT COUNT(*),
       COALESCE(SUM(CASE WHEN model_mismatch = 1 THEN 1 ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN model_mismatch_reason = 'actual_model_unavailable' THEN 1 ELSE 0 END), 0)
FROM usage_records
WHERE `+strings.Join(where, " AND "), args...).Scan(&out.Requests, &out.Mismatches, &out.ActualModelUnavailable); err != nil {
		return ModelAuditSummary{}, err
	}
	args = append(args, limit)
	rows, err := s.rdb.QueryContext(ctx, `
SELECT requested_model, resolved_model, actual_model, model_override_source,
       model_mismatch, model_mismatch_reason, COUNT(*), MAX(created_at)
FROM usage_records
WHERE `+strings.Join(where, " AND ")+`
GROUP BY requested_model, resolved_model, actual_model, model_override_source, model_mismatch, model_mismatch_reason
ORDER BY model_mismatch DESC, COUNT(*) DESC, MAX(created_at) DESC
LIMIT ?`, args...)
	if err != nil {
		return ModelAuditSummary{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var row ModelAuditRow
		var mismatch int
		if err = rows.Scan(&row.RequestedModel, &row.ResolvedModel, &row.ActualModel, &row.ModelOverrideSource,
			&mismatch, &row.MismatchReason, &row.Requests, &row.LastSeenAt); err != nil {
			return ModelAuditSummary{}, err
		}
		row.Mismatch = mismatch != 0
		out.Rows = append(out.Rows, row)
	}
	if err = rows.Err(); err != nil {
		return ModelAuditSummary{}, err
	}
	if out.Rows == nil {
		out.Rows = []ModelAuditRow{}
	}
	return out, nil
}
