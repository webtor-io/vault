package services

import (
	"context"
	"errors"
	"fmt"
	"time"

	pg "github.com/go-pg/pg/v10"
	"github.com/google/uuid"
)

// IsPGDuplicateKey checks whether the error is a PostgreSQL unique constraint violation (code 23505).
func IsPGDuplicateKey(err error) bool {
	var pgErr pg.Error
	if errors.As(err, &pgErr) {
		return pgErr.IntegrityViolation()
	}
	return false
}

type Status int16

const (
	// Status codes for resources/files
	StatusQueuedForStoring Status = iota
	StatusStoring
	StatusStored
	StatusStoreError
	StatusQueuedForDeletion
	StatusDeleting
	StatusDeleteError
)

var statusNames = []string{"queued_for_storing", "storing", "stored", "store_error", "queued_for_deletion", "deleting", "delete_error"}

func (s Status) String() string {
	if int(s) < 0 || int(s) >= len(statusNames) {
		return fmt.Sprintf("unknown(%d)", s)
	}
	return statusNames[s]
}

// OperationType represents the type of operation performed on a resource.
type OperationType int16

const (
	OperationStore  OperationType = iota // 0 - store
	OperationDelete                      // 1 - delete
)

// OperationStatus represents the result of an operation.
// 0 - success, 1 - fail
type OperationStatus int16

const (
	OperationSuccess OperationStatus = iota // 0 - success
	OperationFail                           // 1 - fail
)

type ErrorResponse struct {
	Error string `json:"error"`
}

// Resource represents a stored object composed from one or more files.
// DB mapping is aligned with migrations/1_init.* and 8_resource_claim.*
type Resource struct {
	// go-pg table name
	tableName struct{} `pg:"resource"`

	ID         string    `json:"resource_id" pg:"resource_id,pk"`
	Status     Status    `json:"status" pg:"status,use_zero"`
	TotalSize  int64     `json:"total_size" pg:"total_size,notnull,use_zero,default:0"`
	StoredSize int64     `json:"stored_size" pg:"stored_size,notnull,use_zero,default:0"`
	Error      *string   `json:"error,omitempty" pg:"error"`
	CreatedAt  time.Time `json:"created_at" pg:"created_at,notnull,default:now()"`
	UpdatedAt  time.Time `json:"updated_at" pg:"updated_at,notnull,default:now()"`

	// Lease columns (migration 8). A non-null claim_expires_at in the future
	// means the row is owned by the worker in claimed_by for that duration.
	// Heartbeat refreshes the deadline; on crash or shutdown the lease
	// naturally expires and another worker can reclaim the row. This replaces
	// the old "row updated_at is stale" heuristic for cross-pod coordination.
	ClaimExpiresAt *time.Time `json:"claim_expires_at,omitempty" pg:"claim_expires_at"`
	ClaimedBy      *string    `json:"claimed_by,omitempty" pg:"claimed_by"`

	// Relations
	// All resource<->file links for this resource. Use with Relation("ResourceFiles") or
	// Relation("ResourceFiles.File") to also load referenced files.
	ResourceFiles []ResourceFile `json:"-" pg:"rel:has-many,fk:resource_id"`
}

// File represents a file (by content hash) that may belong to many resources.
type File struct {
	// go-pg table name
	tableName struct{} `pg:"file"`

	Hash       string    `json:"hash" pg:"hash,pk"`
	Status     Status    `json:"status" pg:"status,use_zero"`
	UploadID   string    `json:"upload_id" pg:"upload_id"`
	PartSize   int64     `json:"part_size" pg:"part_size,notnull,use_zero,default:0"`
	TotalSize  int64     `json:"total_size" pg:"total_size,notnull,use_zero,default:0"`
	StoredSize int64     `json:"stored_size" pg:"stored_size,notnull,use_zero,default:0"`
	CreatedAt  time.Time `json:"created_at" pg:"created_at,notnull,default:now()"`
	UpdatedAt  time.Time `json:"updated_at" pg:"updated_at,notnull,default:now()"`

	// Relations
	// All resource links that reference this file. Use with Relation("ResourceFiles") or
	// Relation("ResourceFiles.Resource") to also load referenced resources.
	ResourceFiles []ResourceFile `json:"-" pg:"rel:has-many,fk:file_hash"`
}

// ResourceFile links files to resources and stores a path within the resource.
type ResourceFile struct {
	// go-pg table name
	tableName struct{} `pg:"resource_file"`

	ResourceID string `json:"resource_id" pg:"resource_id,pk"`
	FileHash   string `json:"file_hash" pg:"file_hash,pk"`
	Path       string `json:"path" pg:"path,pk"`

	// Relations
	Resource *Resource `json:"-" pg:"rel:has-one,fk:resource_id"`
	File     *File     `json:"-" pg:"rel:has-one,fk:file_hash"`
}

// Helper methods for working with the DB using go-pg. These are simple helpers instead of a separate repo layer.

// WorkerLog stores audit information about store/delete operations on resources.
type WorkerLog struct {
	tableName     struct{}      `pg:"worker_log"`
	LogID         uuid.UUID     `json:"log_id" pg:"log_id,pk,type:uuid"`
	OperationType OperationType `json:"operation_type" pg:"operation_type,use_zero"`
	StartedAt     time.Time     `json:"started_at" pg:"started_at,notnull,default:now()"`
	FinishedAt    *time.Time    `json:"finished_at,omitempty" pg:"finished_at"`
	// ResourceID becomes nullable to preserve logs when resource is deleted
	ResourceID string `json:"resource_id,omitempty" pg:"resource_id"`
	// Status is nullable until the operation completes
	Status *OperationStatus `json:"status,omitempty" pg:"status"`
	// ErrorText stores error message when operation fails
	ErrorText *string `json:"error_text,omitempty" pg:"error_text"`
}

// WorkerLogStart creates a new worker log entry and returns it.
func WorkerLogStart(ctx context.Context, db *pg.DB, resourceID string, s Status) (*WorkerLog, error) {
	op := OperationStore
	if s == StatusDeleting {
		op = OperationDelete
	}
	l := &WorkerLog{ResourceID: resourceID, OperationType: op}
	if _, err := db.Model(l).Context(ctx).Insert(); err != nil {
		return nil, err
	}
	return l, nil
}

// WorkerLogFinish sets finished_at to NOW() and stores status and error_text for the specified worker log entry.
func WorkerLogFinish(ctx context.Context, db *pg.DB, logID uuid.UUID, oerr error) (err error) {
	l := &WorkerLog{LogID: logID}
	if oerr != nil {
		_, err = db.Model(l).Context(ctx).
			Set("finished_at = now()").
			Set("status = ?", OperationFail).
			Set("error_text = ?", oerr.Error()).
			WherePK().
			Update()
	} else {
		_, err = db.Model(l).Context(ctx).
			Set("finished_at = now()").
			Set("status = ?", OperationSuccess).
			WherePK().
			Update()
	}
	return
}

// APILog stores audit information about incoming HTTP requests for store/delete operations.
type APILog struct {
	tableName     struct{}      `pg:"api_log"`
	APILogID      uuid.UUID     `json:"api_log_id" pg:"api_log_id,pk,type:uuid"`
	OperationType OperationType `json:"operation_type" pg:"operation_type,use_zero"`
	ResourceID    string        `json:"resource_id" pg:"resource_id"`
	HTTPStatus    int16         `json:"http_status" pg:"http_status,use_zero"`
	CreatedAt     time.Time     `json:"created_at" pg:"created_at,notnull,default:now()"`
}

// APILogWrite writes an API log entry. Errors are non-fatal and returned for the caller to log.
func APILogWrite(ctx context.Context, db *pg.DB, resourceID string, operationType OperationType, httpStatus int16) error {
	l := &APILog{
		ResourceID:    resourceID,
		OperationType: operationType,
		HTTPStatus:    httpStatus,
	}
	_, err := db.Model(l).Context(ctx).Insert()
	return err
}

// ResourceQueueForStoring inserts a new resource with queued status or updates existing to queued.
func ResourceQueueForStoring(ctx context.Context, db *pg.DB, id string) (*Resource, error) {
	res := &Resource{ID: id, Status: StatusQueuedForStoring}
	err := db.Model(res).
		Context(ctx).
		WherePK().
		Select()
	if err != nil && !errors.Is(err, pg.ErrNoRows) {
		return nil, err
	}
	if errors.Is(err, pg.ErrNoRows) {
		if _, err = db.Model(res).Context(ctx).Insert(); err != nil {
			return nil, err
		}
		return res, nil
	}
	if res.Status == StatusQueuedForStoring || res.Status == StatusStoring || res.Status == StatusStored {
		return res, nil
	}
	res.Status = StatusQueuedForStoring
	// update
	if _, err = db.Model(res).Context(ctx).Column("status").WherePK().Update(); err != nil {
		return nil, err
	}
	// reload
	if err = db.Model(res).Context(ctx).WherePK().Select(); err != nil {
		return nil, err
	}

	return res, nil
}

// ResourceGetByID loads a resource by id.
func ResourceGetByID(ctx context.Context, db *pg.DB, id string) (*Resource, error) {
	res := &Resource{ID: id}
	err := db.Model(res).Context(ctx).WherePK().Select()
	if err != nil {
		if errors.Is(err, pg.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return res, nil
}

// ResourceQueueForDeletion marks the resource as queued (placeholder for deletion workflow).
func ResourceQueueForDeletion(ctx context.Context, db *pg.DB, id string) (*Resource, error) {
	res := &Resource{ID: id}
	err := db.Model(res).Context(ctx).WherePK().Select()
	if err != nil && !errors.Is(err, pg.ErrNoRows) {
		return nil, err
	}
	if errors.Is(err, pg.ErrNoRows) {
		return nil, nil
	}
	if res.Status == StatusDeleting || res.Status == StatusQueuedForDeletion {
		return res, nil
	}
	if res.Status == StatusQueuedForStoring {
		if _, err = db.Model(res).Context(ctx).WherePK().Delete(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	res.Status = StatusQueuedForDeletion
	if _, err = db.Model(res).Context(ctx).Column("status").WherePK().Update(); err != nil {
		return nil, err
	}
	if err = db.Model(res).Context(ctx).WherePK().Select(); err != nil {
		return nil, err
	}
	return res, nil
}
