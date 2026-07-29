package storage

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

type RegistrationCanary struct {
	Method               string `json:"method"`
	Status               string `json:"status"`
	ReadinessFingerprint string `json:"readiness_fingerprint,omitempty"`
	JobID                string `json:"job_id,omitempty"`
	AccountID            string `json:"account_id,omitempty"`
	ErrorClass           string `json:"error_class,omitempty"`
	LastSuccessAt        int64  `json:"last_success_at"`
	LastFailureAt        int64  `json:"last_failure_at"`
	UpdatedAt            int64  `json:"updated_at"`
}

func (s *Store) RecordRegistrationCanary(
	ctx context.Context,
	method, status, fingerprint, jobID, accountID, errorClass string,
) error {
	method = strings.ToLower(strings.TrimSpace(method))
	if method == "" || (status != "passed" && status != "failed") {
		return errors.New("invalid registration canary result")
	}
	now := Now()
	successAt := int64(0)
	failureAt := int64(0)
	if status == "passed" {
		successAt = now
		errorClass = ""
	} else {
		failureAt = now
		accountID = ""
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO registration_method_canaries(
  method,status,readiness_fingerprint,job_id,account_id,error_class,
  last_success_at,last_failure_at,updated_at
) VALUES(?,?,?,?,?,?,?,?,?)
ON CONFLICT(method) DO UPDATE SET
  status=excluded.status,
  readiness_fingerprint=excluded.readiness_fingerprint,
  job_id=excluded.job_id,
  account_id=excluded.account_id,
  error_class=excluded.error_class,
  last_success_at=CASE WHEN excluded.last_success_at>0 THEN excluded.last_success_at ELSE registration_method_canaries.last_success_at END,
  last_failure_at=CASE WHEN excluded.last_failure_at>0 THEN excluded.last_failure_at ELSE registration_method_canaries.last_failure_at END,
  updated_at=excluded.updated_at`,
		method, status, strings.TrimSpace(fingerprint), strings.TrimSpace(jobID),
		strings.TrimSpace(accountID), strings.TrimSpace(errorClass), successAt, failureAt, now)
	return err
}

func (s *Store) GetRegistrationCanary(ctx context.Context, method string) (RegistrationCanary, error) {
	var canary RegistrationCanary
	err := s.rdb.QueryRowContext(ctx, `
SELECT method,status,readiness_fingerprint,job_id,account_id,error_class,
       last_success_at,last_failure_at,updated_at
FROM registration_method_canaries WHERE method=?`,
		strings.ToLower(strings.TrimSpace(method))).
		Scan(&canary.Method, &canary.Status, &canary.ReadinessFingerprint, &canary.JobID,
			&canary.AccountID, &canary.ErrorClass, &canary.LastSuccessAt,
			&canary.LastFailureAt, &canary.UpdatedAt)
	return canary, err
}

func (s *Store) ListRegistrationCanaries(ctx context.Context) ([]RegistrationCanary, error) {
	rows, err := s.rdb.QueryContext(ctx, `
SELECT method,status,readiness_fingerprint,job_id,account_id,error_class,
       last_success_at,last_failure_at,updated_at
FROM registration_method_canaries ORDER BY method`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]RegistrationCanary, 0, 5)
	for rows.Next() {
		var canary RegistrationCanary
		if err := rows.Scan(&canary.Method, &canary.Status, &canary.ReadinessFingerprint,
			&canary.JobID, &canary.AccountID, &canary.ErrorClass, &canary.LastSuccessAt,
			&canary.LastFailureAt, &canary.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, canary)
	}
	return out, rows.Err()
}

func (s *Store) RegistrationCanaryPassed(ctx context.Context, method, fingerprint string) (bool, error) {
	canary, err := s.GetRegistrationCanary(ctx, method)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return canary.Status == "passed" &&
		canary.ReadinessFingerprint != "" &&
		canary.ReadinessFingerprint == strings.TrimSpace(fingerprint), nil
}
