package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrMaintenanceLeaseLost = errors.New("maintenance lease lost")

// MaintenanceLease is the durable fencing record used by workers that perform
// singleton side effects. FencingToken increases on every takeover, including a
// reacquisition by the same owner after its previous lease expired.
type MaintenanceLease struct {
	Name         string `json:"lease_name"`
	OwnerID      string `json:"owner_id"`
	FencingToken int64  `json:"fencing_token"`
	ExpiresAt    int64  `json:"expires_at"`
	UpdatedAt    int64  `json:"updated_at"`
}

func validateMaintenanceLeaseInput(name, ownerID string, ttl time.Duration) (string, string, int64, error) {
	name = strings.TrimSpace(name)
	ownerID = strings.TrimSpace(ownerID)
	if name == "" || len(name) > 128 {
		return "", "", 0, errors.New("maintenance lease name must contain 1-128 characters")
	}
	if ownerID == "" || len(ownerID) > 256 {
		return "", "", 0, errors.New("maintenance lease owner must contain 1-256 characters")
	}
	if ttl <= 0 {
		return "", "", 0, errors.New("maintenance lease TTL must be positive")
	}
	ttlSeconds := int64((ttl + time.Second - 1) / time.Second)
	if ttlSeconds > int64((24*time.Hour)/time.Second) {
		return "", "", 0, errors.New("maintenance lease TTL must not exceed 24 hours")
	}
	return name, ownerID, ttlSeconds, nil
}

// AcquireMaintenanceLease atomically acquires an absent/expired lease or
// refreshes an unexpired lease already held by ownerID. The returned bool is
// false when another live owner remains fenced in.
func (s *Store) AcquireMaintenanceLease(ctx context.Context, name, ownerID string, ttl time.Duration) (MaintenanceLease, bool, error) {
	name, ownerID, ttlSeconds, err := validateMaintenanceLeaseInput(name, ownerID, ttl)
	if err != nil {
		return MaintenanceLease{}, false, err
	}
	now := Now()
	expiresAt := now + ttlSeconds
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MaintenanceLease{}, false, err
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
INSERT INTO maintenance_leases(lease_name,owner_id,fencing_token,expires_at,updated_at)
VALUES(?,?,1,?,?)
ON CONFLICT(lease_name) DO UPDATE SET
  owner_id=excluded.owner_id,
  fencing_token=CASE
    WHEN maintenance_leases.owner_id=excluded.owner_id AND maintenance_leases.expires_at>? THEN maintenance_leases.fencing_token
    ELSE maintenance_leases.fencing_token+1
  END,
  expires_at=excluded.expires_at,
  updated_at=excluded.updated_at
WHERE maintenance_leases.owner_id=excluded.owner_id OR maintenance_leases.expires_at<=?`,
		name, ownerID, expiresAt, now, now, now)
	if err != nil {
		return MaintenanceLease{}, false, err
	}
	lease, err := scanMaintenanceLease(tx.QueryRowContext(ctx, `
SELECT lease_name,owner_id,fencing_token,expires_at,updated_at
FROM maintenance_leases WHERE lease_name=?`, name))
	if err != nil {
		return MaintenanceLease{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return MaintenanceLease{}, false, err
	}
	return lease, lease.OwnerID == ownerID && lease.ExpiresAt == expiresAt, nil
}

// RenewMaintenanceLease extends only the exact live fencing generation. Once a
// lease expires or is taken over, a stale worker can never make itself current
// through Renew; it must acquire a new, higher generation.
func (s *Store) RenewMaintenanceLease(ctx context.Context, lease MaintenanceLease, ttl time.Duration) (MaintenanceLease, error) {
	name, ownerID, ttlSeconds, err := validateMaintenanceLeaseInput(lease.Name, lease.OwnerID, ttl)
	if err != nil {
		return MaintenanceLease{}, err
	}
	if lease.FencingToken <= 0 {
		return MaintenanceLease{}, errors.New("maintenance lease fencing token must be positive")
	}
	now := Now()
	expiresAt := now + ttlSeconds
	result, err := s.db.ExecContext(ctx, `
UPDATE maintenance_leases SET expires_at=?,updated_at=?
WHERE lease_name=? AND owner_id=? AND fencing_token=? AND expires_at>?`,
		expiresAt, now, name, ownerID, lease.FencingToken, now)
	if err != nil {
		return MaintenanceLease{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return MaintenanceLease{}, err
	}
	if affected != 1 {
		return MaintenanceLease{}, fmt.Errorf("%w: %s generation %d", ErrMaintenanceLeaseLost, name, lease.FencingToken)
	}
	lease.Name = name
	lease.OwnerID = ownerID
	lease.ExpiresAt = expiresAt
	lease.UpdatedAt = now
	return lease, nil
}

// ReleaseMaintenanceLease expires (rather than deletes) the exact generation so
// the next owner always receives a monotonically increasing fencing token.
func (s *Store) ReleaseMaintenanceLease(ctx context.Context, lease MaintenanceLease) error {
	if strings.TrimSpace(lease.Name) == "" || strings.TrimSpace(lease.OwnerID) == "" || lease.FencingToken <= 0 {
		return errors.New("invalid maintenance lease")
	}
	now := Now()
	_, err := s.db.ExecContext(ctx, `
UPDATE maintenance_leases SET expires_at=?,updated_at=?
WHERE lease_name=? AND owner_id=? AND fencing_token=?`,
		now, now, strings.TrimSpace(lease.Name), strings.TrimSpace(lease.OwnerID), lease.FencingToken)
	return err
}

func (s *Store) GetMaintenanceLease(ctx context.Context, name string) (MaintenanceLease, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return MaintenanceLease{}, errors.New("maintenance lease name is empty")
	}
	return scanMaintenanceLease(s.rdb.QueryRowContext(ctx, `
SELECT lease_name,owner_id,fencing_token,expires_at,updated_at
FROM maintenance_leases WHERE lease_name=?`, name))
}

type maintenanceLeaseScanner interface {
	Scan(...interface{}) error
}

func scanMaintenanceLease(row maintenanceLeaseScanner) (MaintenanceLease, error) {
	var lease MaintenanceLease
	err := row.Scan(&lease.Name, &lease.OwnerID, &lease.FencingToken, &lease.ExpiresAt, &lease.UpdatedAt)
	return lease, err
}
