package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	awss3 "github.com/aws/aws-sdk-go/service/s3"
	pg "github.com/go-pg/pg/v10"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	cs "github.com/webtor-io/common-services"
	ra "github.com/webtor-io/rest-api/services"
)

// Worker is a placeholder for background tasks (store/delete torrent data, update progress).
type Worker struct {
	ctx        context.Context
	cancel     context.CancelFunc
	pg         *cs.PG
	s3         *cs.S3Client
	jobs       chan job
	nwrks      int
	api        *Api
	bucket     string
	concur     int
	part       int64
	nats       *cs.NATS
	resourceID string
	wg         sync.WaitGroup
}

const (
	workerCountFlag          = "workers"
	awsBucketFlag            = "aws-bucket"
	awsUploadConcurrencyFlag = "aws-upload-concurrency"
	awsUploadPartSizeFlag    = "aws-upload-part-size"
	resourceIDFlag           = "resource-id"
)

// RegisterWorkerFlags registers CLI flags for the worker service.
func RegisterWorkerFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.IntFlag{
			Name:   workerCountFlag,
			Usage:  "number of worker goroutines",
			Value:  10,
			EnvVar: "WORKERS",
		},
		cli.StringFlag{
			Name:   awsBucketFlag,
			Usage:  "aws bucket",
			EnvVar: "AWS_BUCKET",
		},
		cli.IntFlag{
			Name:   awsUploadConcurrencyFlag,
			Usage:  "aws upload concurrency",
			Value:  1,
			EnvVar: "AWS_UPLOAD_CONCURRENCY",
		},
		cli.Int64Flag{
			Name:   awsUploadPartSizeFlag,
			Usage:  "aws upload part size",
			Value:  50 * 1000 * 1000,
			EnvVar: "AWS_UPLOAD_PART_SIZE",
		},
		cli.StringFlag{
			Name:   resourceIDFlag,
			Usage:  "specific resource ID to process (for debugging)",
			EnvVar: "RESOURCE_ID",
		},
	)
}

type job struct {
	status Status
	id     string
}

func NewWorker(c *cli.Context, pgc *cs.PG, s3 *cs.S3Client, api *Api, nt *cs.NATS) *Worker {
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	w := &Worker{
		ctx:        ctx,
		cancel:     cancel,
		pg:         pgc,
		s3:         s3,
		nwrks:      c.Int(workerCountFlag),
		jobs:       make(chan job, 1024),
		api:        api,
		bucket:     c.String(awsBucketFlag),
		concur:     c.Int(awsUploadConcurrencyFlag),
		part:       c.Int64(awsUploadPartSizeFlag),
		nats:       nt,
		resourceID: c.String(resourceIDFlag),
	}
	return w
}

// Serve runs the worker loop until ctx is done.
func (s *Worker) Serve() error {
	db := s.pg.Get()
	if db == nil {
		return errors.New("db is not configured")
	}
	// Start worker pool now that all dependencies are ready.
	for i := 0; i < s.nwrks; i++ {
		s.wg.Add(1)
		go s.workerLoop()
	}
	if s.resourceID != "" {
		log.WithField("resource_id", s.resourceID).Info("Worker started in debug mode for specific resource (no time constraints, single run)")
		// Process once and exit when specific resource ID is set
		processErr := s.process(s.ctx, db)
		if processErr != nil {
			log.WithError(processErr).Error("Worker process error")
			return processErr
		}
		log.Info("Worker finished processing specific resource")
		<-s.ctx.Done()
		return nil
	}
	log.Info("Worker started")
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			log.Info("Worker stopped")
			return nil
		case <-ticker.C:
			processErr := s.process(s.ctx, db)
			if processErr != nil {
				log.WithError(processErr).Error("Worker process error")
			}
			log.Debug("Worker tick")
		}
	}
}

func (s *Worker) process(ctx context.Context, db *pg.DB) error {
	// 1. Select resources and update their status inside a short transaction.
	// Dispatch to the jobs channel happens outside the transaction to avoid
	// holding FOR UPDATE locks while waiting on a potentially full channel.
	var list []Resource
	err := db.RunInTransaction(ctx, func(tx *pg.Tx) error {
		q := tx.Model(&list).
			Context(ctx).
			Where("status != ?", StatusStored)

		// If specific resource ID is set (for debugging), filter by it and skip time constraints
		if s.resourceID != "" {
			q = q.Where("resource_id = ?", s.resourceID)
		} else {
			// Apply normal time constraints only when not debugging specific resource
			q = q.WhereGroup(func(q *pg.Query) (*pg.Query, error) {
				return q.WhereOrGroup(func(q *pg.Query) (*pg.Query, error) {
					return q.Where("status IN (?, ?)", StatusStoreError, StatusDeleteError).
						Where("now() - updated_at > interval '30 minutes'"), nil
				}).WhereOrGroup(func(q *pg.Query) (*pg.Query, error) {
					return q.Where("status NOT IN (?, ?)", StatusStoreError, StatusDeleteError).
						Where("now() - updated_at > interval '1 minute'"), nil
				}), nil
			})
		}

		err := q.
			OrderExpr("CASE WHEN status IN (?, ?) THEN 0 ELSE 1 END", StatusQueuedForStoring, StatusQueuedForDeletion).
			Order("updated_at ASC").
			Limit(s.nwrks * 2).
			For("UPDATE SKIP LOCKED").
			Select()
		if err != nil && !errors.Is(err, pg.ErrNoRows) {
			return errors.Wrap(err, "failed to select resources for processing")
		}
		for _, r := range list {
			if err := s.processResource(ctx, tx, r); err != nil {
				log.WithError(err).WithField("id", r.ID).Error("process resource failed")
				continue
			}
		}
		return nil
	})
	if err != nil {
		return errors.Wrap(err, "failed to run transaction in process")
	}

	// 2. Dispatch jobs outside the transaction so we never block while holding row locks.
	for _, r := range list {
		var processingStatus Status
		switch r.Status {
		case StatusQueuedForDeletion, StatusDeleting, StatusDeleteError:
			processingStatus = StatusDeleting
		case StatusQueuedForStoring, StatusStoring, StatusStoreError:
			processingStatus = StatusStoring
		default:
			continue
		}
		select {
		case s.jobs <- job{status: processingStatus, id: r.ID}:
		case <-s.ctx.Done():
			return nil
		}
	}
	return nil
}

// processResource updates the resource status to processing inside the transaction.
// Job dispatch to the channel is done later in process(), outside the transaction.
func (s *Worker) processResource(ctx context.Context, tx *pg.Tx, r Resource) error {
	log.WithField("id", r.ID).WithField("status", r.Status).Info("processing resource")
	var processingStatus Status
	switch r.Status {
	case StatusQueuedForDeletion, StatusDeleting, StatusDeleteError:
		processingStatus = StatusDeleting
	case StatusQueuedForStoring, StatusStoring, StatusStoreError:
		processingStatus = StatusStoring
	default:
		return fmt.Errorf("unexpected resource status: %s", r.Status)
	}

	// For update with status check avoids taking rows already processed
	cur := &Resource{}
	err := tx.Model(cur).
		Context(ctx).
		Where("resource_id = ?", r.ID).
		Where("status = ?", r.Status).
		Where("updated_at = ?", r.UpdatedAt).
		Select()
	if err != nil {
		if errors.Is(err, pg.ErrNoRows) {
			return nil
		}
		return errors.Wrap(err, "failed to select resource for update")
	}
	cur.ID = r.ID
	cur.Status = processingStatus
	cur.UpdatedAt = time.Now()

	if _, err = tx.Model(cur).Context(ctx).Column("status", "updated_at").WherePK().Update(); err != nil {
		return errors.Wrap(err, "failed to update resource status to processing")
	}
	return nil
}

func (s *Worker) Close() {
	log.Info("closing Worker")
	s.cancel()
	s.wg.Wait()
	log.Info("Worker closed")
}

func (s *Worker) jobCancelContext(inCtx context.Context, db *pg.DB, j job) (ctx context.Context, cancel context.CancelFunc) {
	ctx, cancel = context.WithCancel(inCtx)
	ticker := time.NewTicker(5 * time.Second)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				res := &Resource{ID: j.id}
				err := db.Model(res).
					Context(inCtx).
					WherePK().
					Where("status = ?", j.status).
					Select()
				if err != nil && !errors.Is(err, pg.ErrNoRows) {
					log.WithError(err).WithField("id", j.id).Error("check status failed")
				} else if err != nil && errors.Is(err, pg.ErrNoRows) {
					log.WithField("id", j.id).WithField("status", j.status.String()).Info("status changed, job cancelled")
					cancel()
					return
				}
			}
		}
	}()
	return
}

func (s *Worker) processJob(ctx context.Context, db *pg.DB, j job) (err error) {
	ctx, cancel := s.jobCancelContext(ctx, db, j)
	defer cancel()
	opLog, err := WorkerLogStart(ctx, db, j.id, j.status)
	if err != nil {
		log.WithError(err).WithField("resource_id", j.id).Warn("failed to create worker log")
	}
	if opLog != nil {
		defer func() {
			lerr := WorkerLogFinish(ctx, db, opLog.LogID, err)
			if lerr != nil {
				log.WithError(lerr).WithField("log_id", opLog.LogID).Warn("failed to finish worker log")
			}
		}()
	}
	switch j.status {
	case StatusStoring:
		log.WithField("id", j.id).Info("storing started")
		if err = s.handleStore(ctx, db, j.id); err != nil {
			log.WithError(err).WithField("id", j.id).Error("store failed")
			s.handleError(ctx, j.id, err, StatusStoreError)
			return
		}
		log.WithField("id", j.id).Info("stored successfully")
		if s.nats != nil {
			nc := s.nats.Get()
			if nc != nil {
				b, _ := json.Marshal(map[string]string{"resource_id": j.id})
				if pubErr := nc.Publish("resource.vaulted", b); pubErr != nil {
					log.WithError(pubErr).WithField("id", j.id).Error("failed to publish nats message")
				}
			}
		}
	case StatusDeleting:
		log.WithField("id", j.id).Info("deleting started")
		if err = s.handleDelete(ctx, db, j.id); err != nil {
			log.WithError(err).WithField("id", j.id).Error("delete failed")
			s.handleError(ctx, j.id, err, StatusDeleteError)
			return
		}
		log.WithField("id", j.id).Info("deleted successfully")
	default:
		return fmt.Errorf("unexpected job status: %s", j.status)
	}
	return
}

func (s *Worker) workerLoop() {
	defer s.wg.Done()
	db := s.pg.Get()
	for {
		select {
		case <-s.ctx.Done():
			return
		case j := <-s.jobs:
			err := s.processJob(s.ctx, db, j)
			if err != nil {
				log.WithError(err).Error("process job failed")
			}
		}
	}
}

func (s *Worker) handleStore(ctx context.Context, db *pg.DB, id string) (err error) {
	listArgs := &ListResourceContentArgs{
		Limit:  100,
		Offset: 0,
	}
	cla := &Claims{
		Role: "vault",
	}

	// Reset resource counters before (re)storing
	if _, err := db.Model(&Resource{ID: id}).
		Context(ctx).
		Set("total_size = 0").
		Set("stored_size = 0").
		Set("updated_at = now()").
		Set("error = null").
		Where("resource_id = ?", id).
		Update(); err != nil {
		return errors.Wrap(err, "failed to reset resource counters")
	}

	// Clean up old resource_file links to avoid orphans on re-store
	if _, err := db.Model((*ResourceFile)(nil)).
		Context(ctx).
		Where("resource_id = ?", id).
		Delete(); err != nil {
		return errors.Wrap(err, "failed to clean up old resource_file links")
	}

	var totalSize, totalStored int64
	// Paginate through results to find the file at the specified index
	for {
		resp, err := s.api.ListResourceContent(ctx, cla, id, listArgs)
		if err != nil {
			return errors.Wrap(err, "failed to list resource content")
		}
		for _, item := range resp.Items {
			if item.Type == ra.ListTypeFile {
				// First, increment total size for the resource
				totalSize += item.Size
				if _, err := db.Model(&Resource{ID: id}).
					Context(ctx).
					Set("total_size = ?", totalSize).
					Where("resource_id = ?", id).
					Update(); err != nil {
					return errors.Wrap(err, "failed to update resource total_size")
				}

				f, err := s.storeFile(ctx, cla, id, item, totalStored)
				if err != nil {
					return errors.Wrap(err, "failed to store file")
				}
				totalStored += item.Size

				if _, err := db.Model(&Resource{ID: id}).
					Context(ctx).
					Set("stored_size = ?", totalStored).
					Set("error = null").
					Where("resource_id = ?", id).
					Update(); err != nil {
					return errors.Wrap(err, "failed to update resource stored_size")
				}
				rf := &ResourceFile{
					ResourceID: id,
					FileHash:   f.Hash,
					Path:       item.PathStr,
				}
				_, err = db.Model(rf).Insert()
				if err != nil && !IsPGDuplicateKey(err) {
					return errors.Wrap(err, "failed to insert resource_file")
				}
			}
		}

		// Check if we've reached the end
		if len(resp.Items) < int(listArgs.Limit) {
			break
		}

		listArgs.Offset += listArgs.Limit
	}

	res := &Resource{ID: id, Status: StatusStored}
	_, err = db.Model(res).
		Context(ctx).
		Column("status").
		Where("resource_id = ?", id).
		Update()
	if err != nil {
		return errors.Wrap(err, "failed to update resource status to stored")
	}
	return nil
}

func (s *Worker) handleDelete(ctx context.Context, db *pg.DB, id string) (err error) {
	if s.bucket == "" {
		return errors.New("s3 bucket is not configured")
	}
	stopFlush := runPeriodicFlush(ctx, func() {
		if _, err := db.Model(&Resource{ID: id}).
			Context(ctx).
			Set("updated_at = now()").
			Where("resource_id = ?", id).
			Update(); err != nil {
			log.WithError(err).WithField("resource_id", id).Warn("delete heartbeat failed")
		}
	})
	defer stopFlush()
	// 1) Collect all files linked to this resource
	var rfs []ResourceFile
	if err := db.Model(&rfs).Context(ctx).Where("resource_id = ?", id).Select(); err != nil && !errors.Is(err, pg.ErrNoRows) {
		return errors.Wrap(err, "failed to select resource files for deletion")
	}

	// 2) For each file check if it's referenced by any other resource, if not — delete from S3 and DB
	for _, rf := range rfs {
		// Load file to know its size for counters update
		f := &File{Hash: rf.FileHash}
		if err := db.Model(f).Context(ctx).WherePK().Select(); err != nil {
			if !errors.Is(err, pg.ErrNoRows) {
				return errors.Wrap(err, "failed to select file for deletion")
			}
		} else {
			// Decrease resource stored_size by the size currently accounted for this file
			// Guard against negatives in SQL
			if _, err := db.Model(&Resource{ID: id}).Context(ctx).
				Set("stored_size = CASE WHEN stored_size >= ? THEN stored_size - ? ELSE 0 END", f.StoredSize, f.StoredSize).
				Set("updated_at = now()").
				Where("resource_id = ?", id).
				Update(); err != nil {
				return errors.Wrap(err, "failed to update resource stored_size during deletion")
			}
		}
		// Count references excluding current resource
		cnt, err := db.Model((*ResourceFile)(nil)).Context(ctx).
			Where("file_hash = ?", rf.FileHash).
			Where("resource_id <> ?", id).
			Count()
		if err != nil {
			return errors.Wrap(err, "failed to count file references")
		}
		if cnt > 0 {
			continue
		}
		// Mark file status as Deleting before removing the object from S3
		up := &File{Hash: rf.FileHash, Status: StatusDeleting}
		if _, err := db.Model(up).Context(ctx).
			Set("status = ?", StatusDeleting).
			Set("stored_size = 0").
			Set("updated_at = now()").
			WherePK().
			Update(); err != nil && !errors.Is(err, pg.ErrNoRows) {
			return errors.Wrap(err, "failed to mark file as deleting")
		}
		// No more references — delete S3 object (if configured) and file row
		s3Cl := s.s3.Get()
		_, delErr := s3Cl.DeleteObjectWithContext(ctx, &awss3.DeleteObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(rf.FileHash),
		})
		if delErr != nil {
			return errors.Wrapf(delErr, "failed to delete object from S3, bucket=%s, key=%s", s.bucket, rf.FileHash)
		}
		log.WithFields(log.Fields{"bucket": s.bucket, "path": rf.Path, "resource_id": id, "key": rf.FileHash}).Info("deleted from s3")
		// Delete file row
		f = &File{Hash: rf.FileHash}
		if _, err := db.Model(f).Context(ctx).WherePK().Delete(); err != nil && !errors.Is(err, pg.ErrNoRows) {
			return errors.Wrap(err, "failed to delete file row from database")
		}
	}

	res := &Resource{ID: id}
	_, err = db.Model(res).Context(ctx).WherePK().Delete()
	if err != nil {
		return errors.Wrap(err, "failed to delete resource from database")
	}
	return nil
}

func (s *Worker) handleError(_ context.Context, id string, err error, status Status) {
	db := s.pg.Get()
	errMsg := err.Error()
	// Determine which processing status we expect the resource to still be in.
	// Only overwrite if the resource is still in that processing state —
	// otherwise an external status change (e.g. QueuedForDeletion) would be lost.
	var expectedStatus Status
	switch status {
	case StatusStoreError:
		expectedStatus = StatusStoring
	case StatusDeleteError:
		expectedStatus = StatusDeleting
	default:
		expectedStatus = status
	}
	// Use a fresh context with timeout — the job context may already be cancelled.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res := &Resource{ID: id, Error: &errMsg, Status: status}
	result, upErr := db.Model(res).
		Context(ctx).
		Column("status").
		Column("error").
		Where("resource_id = ?", id).
		Where("status = ?", expectedStatus).
		Update()
	if upErr != nil {
		log.WithError(upErr).Error("update error status failed")
	} else if result.RowsAffected() == 0 {
		log.WithFields(log.Fields{
			"id":              id,
			"expected_status": expectedStatus.String(),
		}).Warn("skipped error status update: resource status changed externally")
	}
}

// runPeriodicFlush calls fn every 10 seconds until the returned cancel
// function is called. It is used to keep resource.updated_at fresh so that
// other workers do not re-claim the same job.
func runPeriodicFlush(ctx context.Context, fn func()) context.CancelFunc {
	fctx, cancel := context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-fctx.Done():
				return
			case <-ticker.C:
				fn()
			}
		}
	}()
	return cancel
}

func (s *Worker) storeFile(ctx context.Context, cla *Claims, id string, item ra.ListItem, totalStored int64) (*File, error) {

	if s.bucket == "" {
		return nil, errors.New("s3 bucket is not configured")
	}
	db := s.pg.Get()
	// Try to find an already stored file by matching resource_file.path and file.total_size
	// This allows deduplication by common path and size across resources
	var existing File
	err := db.Model(&existing).
		Context(ctx).
		Column("file.*").
		Join("JOIN resource_file AS rf ON rf.file_hash = file.hash").
		Where("rf.path = ?", item.PathStr).
		Where("file.total_size = ?", item.Size).
		Where("file.status = ?", StatusStored).
		Limit(1).
		Select()
	if err != nil && !errors.Is(err, pg.ErrNoRows) {
		return nil, errors.Wrap(err, "failed to check for existing file")
	}
	if err == nil {
		// Found a suitable file that's already stored
		return &existing, nil
	}
	// No existing stored file found by path+size, proceed with exporting and storing by content hash
	ei, err := s.api.ExportResourceContent(ctx, cla, id, item.ID)
	if err != nil {
		return nil, errors.Wrap(err, "failed to export resource content")
	}
	u := ei.ExportItems["download"].URL

	hash := ""
	flush := func(stored int64) error {
		if hash != "" {
			if _, err := db.Model(&File{Hash: hash}).
				Context(ctx).
				Set("stored_size = ?", stored).
				Set("updated_at = now()").
				WherePK().
				Update(); err != nil {
				return errors.Wrap(err, "failed to update file stored_size")
			}
		}
		if _, err := db.Model(&Resource{ID: id}).
			Context(ctx).
			Set("stored_size = ?", totalStored+stored).
			Set("updated_at = now()").
			Where("resource_id = ?", id).
			Update(); err != nil {
			return errors.Wrap(err, "failed to update resource stored_size during flush")
		}
		return nil
	}
	var mu sync.Mutex
	mu.Lock()
	var completedParts []*awss3.CompletedPart
	completedPartsMap := make(map[int64]*awss3.CompletedPart)
	mu.Unlock()
	partSize := s.part
	if partSize < 5*1024*1024 {
		partSize = 5 * 1024 * 1024
	}

	stopFlush := runPeriodicFlush(ctx, func() {
		mu.Lock()
		currentStored := int64(len(completedPartsMap)) * partSize
		mu.Unlock()
		if currentStored > item.Size {
			currentStored = item.Size
		}
		if err := flush(currentStored); err != nil {
			log.WithError(err).Error("periodic flush progress failed")
		}
	})
	defer stopFlush()
	if err := flush(0); err != nil {
		log.WithError(err).Error("initial flush progress failed")
	}
	log.WithField("url", u).Debug("export url")
	hash, err = s.generateFileHash(ctx, item, ei)
	if err != nil {
		return nil, errors.Wrap(err, "failed to generate file hash")
	}
	log.WithField("hash", hash).Debug("generated hash")
	// Prepare file model
	f := &File{
		Hash:      hash,
		TotalSize: item.Size,
		Status:    StatusStoring,
	}
	err = db.Model(f).
		Context(ctx).
		WherePK().
		Select()
	if err != nil && !errors.Is(err, pg.ErrNoRows) {
		return nil, errors.Wrap(err, "failed to select file by hash")
	}
	if err == nil && (f.Status == StatusStored || (f.UpdatedAt.Add(10*time.Second).After(time.Now()) && f.UploadID == "")) {
		return f, nil
	}
	_, err = db.Model(f).Context(ctx).Insert()
	if err != nil && !IsPGDuplicateKey(err) {
		return nil, errors.Wrap(err, "failed to insert file")
	}

	s3Cl := s.s3.Get()

	if f.UploadID != "" && f.PartSize != partSize {
		log.WithFields(log.Fields{
			"hash":          hash,
			"old_part_size": f.PartSize,
			"new_part_size": partSize,
		}).Info("part size changed, restarting upload")
		_, _ = s3Cl.AbortMultipartUploadWithContext(ctx, &awss3.AbortMultipartUploadInput{
			Bucket:   aws.String(s.bucket),
			Key:      aws.String(hash),
			UploadId: aws.String(f.UploadID),
		})
		f.UploadID = ""
		f.StoredSize = 0
		f.PartSize = partSize
		if _, err := db.Model(f).Context(ctx).Column("upload_id", "stored_size", "part_size").WherePK().Update(); err != nil {
			return nil, errors.Wrap(err, "failed to reset upload after part size change")
		}
	}

	if f.UploadID == "" {
		out, err := s3Cl.CreateMultipartUploadWithContext(ctx, &awss3.CreateMultipartUploadInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(hash),
		})
		if err != nil {
			return nil, errors.Wrapf(err, "failed to create S3 multipart upload, bucket=%s, key=%s", s.bucket, hash)
		}
		f.UploadID = *out.UploadId
		f.StoredSize = 0
		f.PartSize = partSize
		if _, err := db.Model(f).Context(ctx).Column("upload_id", "stored_size", "part_size").WherePK().Update(); err != nil {
			return nil, errors.Wrap(err, "failed to update file with new upload_id")
		}
	}

	var partNumber int64 = 1

	err = s3Cl.ListPartsPagesWithContext(ctx, &awss3.ListPartsInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(hash),
		UploadId: aws.String(f.UploadID),
	}, func(out *awss3.ListPartsOutput, lastPage bool) bool {
		mu.Lock()
		defer mu.Unlock()
		for _, p := range out.Parts {
			completedPartsMap[*p.PartNumber] = &awss3.CompletedPart{
				ETag:       p.ETag,
				PartNumber: p.PartNumber,
			}
			if *p.PartNumber >= partNumber {
				partNumber = *p.PartNumber + 1
			}
		}
		return !lastPage
	})
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok && aerr.Code() == "NoSuchUpload" {
			// Upload expired or deleted, restart
			out, err := s3Cl.CreateMultipartUploadWithContext(ctx, &awss3.CreateMultipartUploadInput{
				Bucket: aws.String(s.bucket),
				Key:    aws.String(hash),
			})
			if err != nil {
				return nil, errors.Wrapf(err, "failed to recreate S3 multipart upload after NoSuchUpload, bucket=%s, key=%s", s.bucket, hash)
			}
			f.UploadID = *out.UploadId
			f.StoredSize = 0
			f.PartSize = partSize
			if _, err := db.Model(f).Context(ctx).Column("upload_id", "stored_size", "part_size").WherePK().Update(); err != nil {
				return nil, errors.Wrap(err, "failed to update file after recreating upload")
			}
			partNumber = 1
			completedParts = nil
		} else {
			return nil, errors.Wrapf(err, "failed to list S3 multipart upload parts, bucket=%s, key=%s, upload_id=%s", s.bucket, hash, f.UploadID)
		}
	}

	mu.Lock()
	stored := int64(len(completedPartsMap)) * partSize
	mu.Unlock()
	if stored > f.TotalSize {
		stored = f.TotalSize
	}

	if err := flush(stored); err != nil {
		log.WithError(err).Error("initial flush progress failed")
	}

	dctx, dcancel := context.WithTimeout(ctx, 20*time.Minute)
	defer dcancel()
	r, err := s.api.DownloadWithRange(dctx, u, int(stored), -1)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to download file content with range, url=%s, start=%d", u, stored)
	}
	defer func(r io.ReadCloser) {
		_ = r.Close()
	}(r)

	type partJob struct {
		partNumber int64
		data       []byte
	}

	jobs := make(chan partJob, s.concur)
	var uploadErr error
	var uploadErrOnce sync.Once
	setUploadErr := func(err error) {
		uploadErrOnce.Do(func() {
			uploadErr = err
		})
	}
	var wg sync.WaitGroup

	for i := 0; i < s.concur; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for pj := range jobs {
				var upOut *awss3.UploadPartOutput
				var err error
				for i := 0; i < 3; i++ {
					upOut, err = s3Cl.UploadPartWithContext(ctx, &awss3.UploadPartInput{
						Bucket:     aws.String(s.bucket),
						Key:        aws.String(hash),
						UploadId:   aws.String(f.UploadID),
						PartNumber: aws.Int64(pj.partNumber),
						Body:       bytes.NewReader(pj.data),
					})
					if err == nil {
						break
					}
					log.WithFields(log.Fields{
						"bucket":      s.bucket,
						"resource_id": id,
						"key":         hash,
						"upload_id":   f.UploadID,
						"part_number": pj.partNumber,
						"attempt":     i + 1,
					}).WithError(err).Warn("failed to upload part, retrying")
					time.Sleep(time.Second * time.Duration(i+1))
				}
				if err != nil {
					setUploadErr(errors.Wrapf(err, "failed to upload part, bucket=%s, key=%s, upload_id=%s, part_number=%d", s.bucket, hash, f.UploadID, pj.partNumber))
					continue
				}
				mu.Lock()
				completedPartsMap[pj.partNumber] = &awss3.CompletedPart{
					ETag:       upOut.ETag,
					PartNumber: aws.Int64(pj.partNumber),
				}
				mu.Unlock()
			}
		}()
	}

	for stored < f.TotalSize {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		if uploadErr != nil {
			break
		}

		currentPartSize := partSize
		if stored+2*currentPartSize > f.TotalSize {
			currentPartSize = f.TotalSize - stored
		}

		log.WithFields(log.Fields{
			"bucket":      s.bucket,
			"resource_id": id,
			"path":        item.PathStr,
			"key":         hash,
			"size":        item.Size,
			"upload_id":   f.UploadID,
			"part_number": partNumber,
			"part_size":   currentPartSize,
			"start_byte":  stored,
		}).Info("reading part")

		buf := make([]byte, currentPartSize)
		_, err := io.ReadFull(r, buf)
		if err != nil {
			setUploadErr(errors.Wrap(err, "failed to read part data from download stream"))
			break
		}

		jobs <- partJob{
			partNumber: partNumber,
			data:       buf,
		}

		stored += currentPartSize
		partNumber++
	}
	close(jobs)
	wg.Wait()

	if uploadErr != nil {
		return nil, uploadErr
	}

	mu.Lock()
	for _, p := range completedPartsMap {
		completedParts = append(completedParts, p)
	}
	mu.Unlock()

	sort.Slice(completedParts, func(i, j int) bool {
		return *completedParts[i].PartNumber < *completedParts[j].PartNumber
	})

	_, err = s3Cl.CompleteMultipartUploadWithContext(ctx, &awss3.CompleteMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(hash),
		UploadId: aws.String(f.UploadID),
		MultipartUpload: &awss3.CompletedMultipartUpload{
			Parts: completedParts,
		},
	})
	if err != nil {
		return nil, errors.Wrapf(err, "failed to complete S3 multipart upload, bucket=%s, key=%s, upload_id=%s", s.bucket, hash, f.UploadID)
	}

	// Ensure file status and stored_size are finalized
	f.Status = StatusStored
	f.StoredSize = f.TotalSize
	f.UploadID = ""
	if _, err := db.Model(f).Context(ctx).Column("status", "stored_size", "upload_id").WherePK().Update(); err != nil {
		return nil, errors.Wrap(err, "failed to finalize file status after upload")
	}
	log.WithFields(log.Fields{"bucket": s.bucket, "resource_id": id, "path": item.PathStr, "key": hash, "size": item.Size}).Info("stored to s3")
	return f, nil
}

func (s *Worker) generateFileHash(ctx context.Context, item ra.ListItem, ei *ra.ExportResponse) (string, error) {
	dctx, dcancel := context.WithTimeout(ctx, 20*time.Minute)
	defer dcancel()
	u := ei.ExportItems["download"].URL
	size := item.Size
	var limitStart int64 = 500 * 1024
	var limitEnd int64 = 500 * 1024
	h := sha256.New()
	h.Write([]byte(fmt.Sprintf("%v", size)))
	if size < limitStart+limitEnd {
		r, err := s.api.Download(dctx, u)
		if err != nil {
			return "", errors.Wrapf(err, "failed to download file for hash generation, url=%s", u)
		}
		defer func(r io.ReadCloser) {
			_ = r.Close()
		}(r)
		_, err = io.Copy(h, r)
		if err != nil {
			return "", errors.Wrap(err, "failed to copy downloaded data to hash")
		}
	} else {
		r, err := s.api.DownloadWithRange(dctx, u, 0, int(limitStart))
		if err != nil {
			return "", errors.Wrapf(err, "failed to download file start for hash generation, url=%s, range=0-%d", u, limitStart)
		}
		defer func(r io.ReadCloser) {
			_ = r.Close()
		}(r)
		_, err = io.Copy(h, r)
		if err != nil {
			return "", errors.Wrap(err, "failed to copy file start data to hash")
		}
		r, err = s.api.DownloadWithRange(dctx, u, int(size-limitEnd), -1)
		if err != nil {
			return "", errors.Wrapf(err, "failed to download file end for hash generation, url=%s, range=%d-end", u, size-limitEnd)
		}
		defer func(r io.ReadCloser) {
			_ = r.Close()
		}(r)
		_, err = io.Copy(h, r)
		if err != nil {
			return "", errors.Wrap(err, "failed to copy file end data to hash")
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
