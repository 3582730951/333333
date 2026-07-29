package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

const (
	StorageResourceCreating = "creating"
	StorageResourceActive   = "active"
	StorageResourceSealed   = "sealed"
	StorageResourceEligible = "eligible"
	StorageResourceTrash    = "trash"
	StorageResourceDeleted  = "deleted"

	StorageResourceTypeDiagnosticArtifact = "diagnostic_artifact"
	StorageRetentionDiagnosticArtifact    = "diagnostic_artifact_24h"
)

var (
	ErrStorageResourceConflict = errors.New("storage resource conflicts with an existing resource")
	ErrStorageResourceFenced   = errors.New("storage resource transition was fenced")
)

// StorageResource is the durable ownership record for a filesystem object.
// GC operates only on an eligible record with a known owner, mount and fencing
// generation; discovering a path on disk is never sufficient authority to delete it.
type StorageResource struct {
	ID             string `json:"id"`
	ResourceType   string `json:"resource_type"`
	Path           string `json:"-"`
	State          string `json:"state"`
	OwnerID        string `json:"owner_id"`
	LeaseExpiresAt int64  `json:"lease_expires_at"`
	FencingToken   int64  `json:"fencing_token"`
	MountID        string `json:"mount_id"`
	SizeBytes      int64  `json:"size_bytes"`
	RetentionClass string `json:"retention_class"`
	CreatedAt      int64  `json:"created_at"`
	UpdatedAt      int64  `json:"updated_at"`
}

type storageResourceScanner interface {
	Scan(...interface{}) error
}

const storageResourceSelect = `
SELECT id,resource_type,path,state,owner_id,lease_expires_at,fencing_token,mount_id,
 size_bytes,retention_class,created_at,updated_at
FROM storage_resources`

func scanStorageResource(scanner storageResourceScanner) (StorageResource, error) {
	var resource StorageResource
	err := scanner.Scan(
		&resource.ID, &resource.ResourceType, &resource.Path, &resource.State,
		&resource.OwnerID, &resource.LeaseExpiresAt, &resource.FencingToken,
		&resource.MountID, &resource.SizeBytes, &resource.RetentionClass,
		&resource.CreatedAt, &resource.UpdatedAt,
	)
	return resource, err
}

func validateStorageResource(resource StorageResource) (StorageResource, error) {
	resource.ID = strings.TrimSpace(resource.ID)
	resource.ResourceType = strings.TrimSpace(resource.ResourceType)
	resource.Path = filepath.Clean(strings.TrimSpace(resource.Path))
	resource.OwnerID = strings.TrimSpace(resource.OwnerID)
	resource.MountID = strings.TrimSpace(resource.MountID)
	resource.RetentionClass = strings.TrimSpace(resource.RetentionClass)
	if resource.ID == "" || len(resource.ID) > 256 {
		return StorageResource{}, errors.New("storage resource id must contain 1-256 characters")
	}
	if resource.ResourceType == "" || len(resource.ResourceType) > 128 {
		return StorageResource{}, errors.New("storage resource type must contain 1-128 characters")
	}
	if !filepath.IsAbs(resource.Path) {
		return StorageResource{}, errors.New("storage resource path must be absolute")
	}
	if resource.OwnerID == "" || len(resource.OwnerID) > 256 {
		return StorageResource{}, errors.New("storage resource owner must contain 1-256 characters")
	}
	if resource.MountID == "" || len(resource.MountID) > 256 {
		return StorageResource{}, errors.New("storage resource mount id must contain 1-256 characters")
	}
	if resource.FencingToken <= 0 {
		return StorageResource{}, errors.New("storage resource fencing token must be positive")
	}
	if resource.SizeBytes < 0 || resource.LeaseExpiresAt < 0 {
		return StorageResource{}, errors.New("storage resource size and lease expiry must be non-negative")
	}
	if resource.RetentionClass == "" || len(resource.RetentionClass) > 128 {
		return StorageResource{}, errors.New("storage resource retention class must contain 1-128 characters")
	}
	return resource, nil
}

// CreateStorageResource records ownership before a file is created. IDs are
// immutable: a collision never overwrites a path or changes its owner.
func (s *Store) CreateStorageResource(ctx context.Context, resource StorageResource) (StorageResource, error) {
	resource.State = StorageResourceCreating
	resource, err := validateStorageResource(resource)
	if err != nil {
		return StorageResource{}, err
	}
	now := Now()
	resource.CreatedAt, resource.UpdatedAt = now, now
	result, err := s.db.ExecContext(ctx, `
INSERT INTO storage_resources(
 id,resource_type,path,state,owner_id,lease_expires_at,fencing_token,mount_id,
 size_bytes,retention_class,created_at,updated_at
) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO NOTHING`,
		resource.ID, resource.ResourceType, resource.Path, resource.State, resource.OwnerID,
		resource.LeaseExpiresAt, resource.FencingToken, resource.MountID, resource.SizeBytes,
		resource.RetentionClass, resource.CreatedAt, resource.UpdatedAt)
	if err != nil {
		return StorageResource{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return StorageResource{}, err
	}
	if affected != 1 {
		return StorageResource{}, fmt.Errorf("%w: %s", ErrStorageResourceConflict, resource.ID)
	}
	return resource, nil
}

func (s *Store) GetStorageResource(ctx context.Context, id string) (StorageResource, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return StorageResource{}, errors.New("storage resource id is empty")
	}
	return scanStorageResource(s.rdb.QueryRowContext(ctx, storageResourceSelect+` WHERE id=?`, id))
}

func transitionStorageResource(
	ctx context.Context,
	exec interface {
		ExecContext(context.Context, string, ...interface{}) (sql.Result, error)
	},
	id, ownerID string,
	fencingToken int64,
	fromStates []string,
	toState, path string,
	sizeBytes, leaseExpiresAt int64,
) error {
	id, ownerID = strings.TrimSpace(id), strings.TrimSpace(ownerID)
	if id == "" || ownerID == "" || fencingToken <= 0 || len(fromStates) == 0 {
		return errors.New("invalid storage resource transition identity")
	}
	switch toState {
	case StorageResourceActive, StorageResourceSealed, StorageResourceEligible,
		StorageResourceTrash, StorageResourceDeleted:
	default:
		return errors.New("invalid storage resource target state")
	}
	if sizeBytes < 0 || leaseExpiresAt < 0 {
		return errors.New("invalid storage resource transition metadata")
	}
	query := `UPDATE storage_resources SET state=?,updated_at=?,size_bytes=?,lease_expires_at=?`
	args := []interface{}{toState, Now(), sizeBytes, leaseExpiresAt}
	if path != "" {
		path = filepath.Clean(strings.TrimSpace(path))
		if !filepath.IsAbs(path) {
			return errors.New("storage resource transition path must be absolute")
		}
		query += `,path=?`
		args = append(args, path)
	}
	query += ` WHERE id=? AND owner_id=? AND fencing_token=? AND state IN (`
	args = append(args, id, ownerID, fencingToken)
	for index, state := range fromStates {
		if index > 0 {
			query += ","
		}
		query += "?"
		args = append(args, state)
	}
	query += `)`
	result, err := exec.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return fmt.Errorf("%w: %s generation %d", ErrStorageResourceFenced, id, fencingToken)
	}
	return nil
}

func (s *Store) ActivateStorageResource(ctx context.Context, resource StorageResource) error {
	return transitionStorageResource(ctx, s.db, resource.ID, resource.OwnerID, resource.FencingToken,
		[]string{StorageResourceCreating}, StorageResourceActive, "", resource.SizeBytes, resource.LeaseExpiresAt)
}

func (s *Store) MarkStorageResourceEligible(ctx context.Context, resource StorageResource) error {
	return transitionStorageResource(ctx, s.db, resource.ID, resource.OwnerID, resource.FencingToken,
		[]string{StorageResourceCreating, StorageResourceActive, StorageResourceSealed},
		StorageResourceEligible, "", resource.SizeBytes, Now())
}

// ListStorageResourcesForGC returns only resources for which the durable record
// contains deletion authority. Unknown owners, mounts and fencing generations are
// deliberately invisible to GC.
func (s *Store) ListStorageResourcesForGC(
	ctx context.Context,
	resourceType, retentionClass string,
	now int64,
	limit int,
) ([]StorageResource, error) {
	resourceType, retentionClass = strings.TrimSpace(resourceType), strings.TrimSpace(retentionClass)
	if resourceType == "" || retentionClass == "" {
		return nil, errors.New("storage GC resource type and retention class are required")
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.rdb.QueryContext(ctx, storageResourceSelect+`
 WHERE resource_type=? AND retention_class=? AND owner_id<>'' AND mount_id<>''
 AND fencing_token>0
 AND (state='trash' OR (state='eligible' AND lease_expires_at<=?))
 ORDER BY CASE state WHEN 'trash' THEN 0 ELSE 1 END,updated_at,id
 LIMIT ?`, resourceType, retentionClass, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var resources []StorageResource
	for rows.Next() {
		resource, scanErr := scanStorageResource(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		resources = append(resources, resource)
	}
	return resources, rows.Err()
}

func (s *Store) ClaimStorageResourceTrash(ctx context.Context, resource StorageResource) error {
	return transitionStorageResource(ctx, s.db, resource.ID, resource.OwnerID, resource.FencingToken,
		[]string{StorageResourceEligible}, StorageResourceTrash, "", resource.SizeBytes, resource.LeaseExpiresAt)
}

func (s *Store) UpdateStorageResourceTrashPath(ctx context.Context, resource StorageResource, trashPath string) error {
	return transitionStorageResource(ctx, s.db, resource.ID, resource.OwnerID, resource.FencingToken,
		[]string{StorageResourceTrash}, StorageResourceTrash, trashPath, resource.SizeBytes, resource.LeaseExpiresAt)
}

func (s *Store) MarkStorageResourceDeleted(ctx context.Context, resource StorageResource) error {
	return transitionStorageResource(ctx, s.db, resource.ID, resource.OwnerID, resource.FencingToken,
		[]string{StorageResourceTrash}, StorageResourceDeleted, "", 0, resource.LeaseExpiresAt)
}
