package services

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	awss3 "github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	pg "github.com/go-pg/pg/v10"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	cs "github.com/webtor-io/common-services"
	ra "github.com/webtor-io/rest-api/services"
)

// progressReader wraps an io.Reader and invokes onRead with the number of bytes
// read on each successful Read call. If onRead returns an error, reading stops
// and that error is propagated to the caller.
type progressReader struct {
	r      io.Reader
	onRead func(n int) error
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 && p.onRead != nil {
		if cbErr := p.onRead(n); cbErr != nil {
			return n, cbErr
		}
	}
	return n, err
}

// Worker is a placeholder for background tasks (store/delete torrent data, update progress).
type Worker struct {
	ctx    context.Context
	cancel context.CancelFunc
	pg     *cs.PG
	s3     *cs.S3Client
	jobs   chan job
	nwrks  int
	api    *Api
	bucket string
}

const (
	workerCountFlag = "workers"
	awsBucketFlag   = "aws-bucket"
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
	)
}

type job struct {
	status Status
	id     string
}

func NewWorker(c *cli.Context, pgc *cs.PG, s3 *cs.S3Client, api *Api) *Worker {
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	w := &Worker{
		ctx:    ctx,
		cancel: cancel,
		pg:     pgc,
		s3:     s3,
		nwrks:  c.Int(workerCountFlag),
		jobs:   make(chan job, 1024),
		api:    api,
		bucket: c.String(awsBucketFlag),
	}
	// start worker pool
	for i := 0; i < w.nwrks; i++ {
		go w.workerLoop()
	}
	return w
}

// Serve runs the worker loop until ctx is done.
func (s *Worker) Serve() error {
	db := s.pg.Get()
	if db == nil {
		return errors.New("db is not configured")
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
	// 1. Get all resources queued for storing or deletion in one request
	var list []Resource
	err := db.Model(&list).
		Context(ctx).
		Where("status != ?", StatusStored).
		Where("now() - updated_at > interval '10 seconds'").
		Select()
	if err != nil && !errors.Is(err, pg.ErrNoRows) {
		return err
	}
	// 2. For each resource handle atomically with SELECT FOR UPDATE to avoid races
	for _, r := range list {
		if err := s.processResource(ctx, db, r); err != nil {
			log.WithError(err).WithField("id", r.ID).Error("process resource failed")
			continue
		}
		//log.WithField("id", r.ID).Info("processed resource")
	}
	return nil
}

func (s *Worker) processResource(ctx context.Context, db *pg.DB, r Resource) error {
	var processingStatus Status
	switch r.Status {
	case StatusQueuedForDeletion:
		processingStatus = StatusDeleting
	case StatusQueuedForStoring:
		processingStatus = StatusStoring
	}
	return db.RunInTransaction(s.ctx, func(tx *pg.Tx) error {
		// lock row only if it is still queued for deletion
		cur := &Resource{}
		// For update with status check avoids taking rows already processed
		err := tx.Model(cur).
			Context(ctx).
			Where("resource_id = ?", r.ID).
			Where("updated_at = ?", r.UpdatedAt).
			For("UPDATE").
			Select()
		if err != nil {
			if errors.Is(err, pg.ErrNoRows) {
				return nil
			}
			return err
		}
		cur.ID = r.ID
		cur.Status = processingStatus
		if _, err = tx.Model(cur).Context(ctx).Column("status").WherePK().Update(); err != nil {
			return err
		}
		select {
		case s.jobs <- job{status: processingStatus, id: r.ID}:
		case <-s.ctx.Done():
		}
		return nil
	})
}

func (s *Worker) Close() {
	log.Info("closing Worker")
	s.cancel()
}

func (s *Worker) jobCancelContext(inCtx context.Context, db *pg.DB, j job) (ctx context.Context, cancel context.CancelFunc) {
	ctx, cancel = context.WithCancel(inCtx)
	t := time.NewTimer(5 * time.Second)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				res := &Resource{ID: j.id}
				err := db.Model(res).
					Context(ctx).
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
	opLog, err := LogOperationStart(ctx, db, j.id, j.status)
	if err != nil {
		log.WithError(err).WithField("resource_id", j.id).Warn("failed to create operation log")
	}
	if opLog != nil {
		defer func() {
			lerr := LogOperationFinish(ctx, db, opLog.LogID, err)
			if lerr != nil {
				log.WithError(lerr).WithField("log_id", opLog.LogID).Warn("failed to finish operation log")
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
	case StatusDeleting:
		log.WithField("id", j.id).Info("deleting started")
		if err = s.handleDelete(ctx, db, j.id); err != nil {
			log.WithError(err).WithField("id", j.id).Error("delete failed")
			s.handleError(ctx, j.id, err, StatusDeleteError)
			return
		}
		log.WithField("id", j.id).Info("deleted successfully")
	}
	return
}

func (s *Worker) workerLoop() {
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
		Where("resource_id = ?", id).
		Update(); err != nil {
		return err
	}

	// Paginate through results to find the file at the specified index
	for {
		resp, err := s.api.ListResourceContent(ctx, cla, id, listArgs)
		if err != nil {
			return err
		}
		var totalSize, totalStored int64
		for _, item := range resp.Items {
			if item.Type == ra.ListTypeFile {
				// First, increment total size for the resource
				totalSize += item.Size
				if _, err := db.Model(&Resource{ID: id}).
					Context(ctx).
					Set("total_size = ?", totalSize).
					Where("resource_id = ?", id).
					Update(); err != nil {
					return err
				}

				f, err := s.storeFile(ctx, cla, id, item, totalStored)
				if err != nil {
					return err
				}
				totalStored += item.Size

				if _, err := db.Model(&Resource{ID: id}).
					Context(ctx).
					Set("stored_size = ?", totalStored).
					Set("error = ?", "").
					Where("resource_id = ?", id).
					Update(); err != nil {
					return err
				}
				rf := &ResourceFile{
					ResourceID: id,
					FileHash:   f.Hash,
					Path:       item.PathStr,
				}
				_, err = db.Model(rf).Insert()
				if err != nil && !strings.Contains(err.Error(), "duplicate key value violates unique constraint") {
					return err
				} else if err != nil {
					continue
				}
			}
		}

		// Check if we've reached the end
		if (resp.Count - int(listArgs.Offset)) == len(resp.Items) {
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
	return err
}

func (s *Worker) handleDelete(ctx context.Context, db *pg.DB, id string) (err error) {
	if s.bucket == "" {
		return errors.New("s3 bucket is not configured")
	}
	// 1) Collect all files linked to this resource
	var rfs []ResourceFile
	if err := db.Model(&rfs).Context(ctx).Where("resource_id = ?", id).Select(); err != nil && !errors.Is(err, pg.ErrNoRows) {
		return err
	}

	// 2) For each file check if it's referenced by any other resource, if not — delete from S3 and DB
	for _, rf := range rfs {
		// Load file to know its size for counters update
		f := &File{Hash: rf.FileHash}
		if err := db.Model(f).Context(ctx).WherePK().Select(); err != nil {
			if !errors.Is(err, pg.ErrNoRows) {
				return err
			}
		} else {
			// Decrease resource stored_size by the size currently accounted for this file
			// Guard against negatives in SQL
			if _, err := db.Model(&Resource{ID: id}).Context(ctx).
				Set("stored_size = CASE WHEN stored_size >= ? THEN stored_size - ? ELSE 0 END", f.StoredSize, f.StoredSize).
				Set("updated_at = now()").
				Where("resource_id = ?", id).
				Update(); err != nil {
				return err
			}
		}
		// Count references excluding current resource
		cnt, err := db.Model((*ResourceFile)(nil)).Context(ctx).
			Where("file_hash = ?", rf.FileHash).
			Where("resource_id <> ?", id).
			Count()
		if err != nil {
			return err
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
			return err
		}
		// No more references — delete S3 object (if configured) and file row
		s3Cl := s.s3.Get()
		_, delErr := s3Cl.DeleteObjectWithContext(ctx, &awss3.DeleteObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(rf.FileHash),
		})
		if delErr != nil {
			return delErr
		}
		log.WithFields(log.Fields{"bucket": s.bucket, "path": rf.Path, "resource_id": id, "key": rf.FileHash}).Info("deleted from s3")
		// Delete file row
		f = &File{Hash: rf.FileHash}
		if _, err := db.Model(f).Context(ctx).WherePK().Delete(); err != nil && !errors.Is(err, pg.ErrNoRows) {
			return err
		}
	}

	res := &Resource{ID: id}
	_, err = db.Model(res).Context(ctx).WherePK().Delete()

	return err
}

func (s *Worker) handleError(ctx context.Context, id string, err error, status Status) {
	db := s.pg.Get()
	// Change status from storing to stored
	errMsg := err.Error()
	res := &Resource{ID: id, Error: &errMsg, Status: status}
	_, upErr := db.Model(res).
		Context(ctx).
		Column("status").
		Column("error").
		Where("resource_id = ?", id).
		Update()
	if upErr != nil {
		log.WithError(upErr).Error("update error status failed")
	}
}

func (s *Worker) storeFile(ctx context.Context, cla *Claims, id string, item ra.ListItem, totalStored int64) (*File, error) {
	if s.bucket == "" {
		return nil, errors.New("s3 bucket is not configured")
	}
	db := s.pg.Get()
	f := &File{
		TotalSize: item.Size,
		Path:      &item.PathStr,
		Status:    StatusStoring,
	}
	err := db.Model(f).Context(ctx).Where("total_size = ? AND path = ?", item.Size, item.PathStr, StatusStored).Select()
	if err != nil && !errors.Is(err, pg.ErrNoRows) {
		return nil, err
	}
	if err == nil && (f.Status == StatusStored || f.UpdatedAt.Add(10*time.Second).After(time.Now())) {
		return f, nil
	}
	ei, err := s.api.ExportResourceContent(ctx, cla, id, item.ID)
	if err != nil {
		return nil, err
	}
	u := ei.ExportItems["download"].URL
	log.WithField("url", u).Debug("export url")
	hash, err := s.generateFileHash(ctx, item, ei)
	if err != nil {
		return nil, err
	}
	log.WithField("hash", hash).Debug("generated hash")
	f.Hash = hash
	err = db.Model(f).
		Context(ctx).
		WherePK().
		Select()
	if err != nil && !errors.Is(err, pg.ErrNoRows) {
		return nil, err
	}
	if err == nil && (f.Status == StatusStored || f.UpdatedAt.Add(10*time.Second).After(time.Now())) {
		return f, nil
	}
	_, err = db.Model(f).Context(ctx).Insert()
	if err != nil && !strings.Contains(err.Error(), "duplicate key value violates unique constraint") {
		return nil, err
	}

	// Progress reporting wrapper with throttled DB flushes (once every 5 seconds)
	var stored int64
	flush := func(stored int64) error {
		// Update file and resource stored_size counters atomically
		if _, err := db.Model(&File{Hash: hash}).
			Context(ctx).
			Set("stored_size = ?", stored).
			Set("updated_at = now()").
			WherePK().
			Update(); err != nil {
			return err
		}
		if _, err := db.Model(&Resource{ID: id}).
			Context(ctx).
			Set("stored_size = ?", totalStored+stored).
			Set("updated_at = now()").
			Where("resource_id = ?", id).
			Update(); err != nil {
			return err
		}
		return nil
	}

	flushCtx, cancel := context.WithCancel(ctx)
	flushTicker := time.NewTicker(5 * time.Second)
	defer flushTicker.Stop()
	defer cancel()
	go func() {
		for {
			select {
			case <-flushCtx.Done():
				return
			case <-flushTicker.C:
				if err := flush(stored); err != nil {
					log.WithError(err).Error("flush progress failed")
				}
			}
		}
	}()
	s3Cl := s.s3.Get()
	r, err := s.api.Download(ctx, u)
	if err != nil {
		return nil, err
	}
	defer func(r io.ReadCloser) {
		_ = r.Close()
	}(r)

	pr := &progressReader{
		r: r,
		onRead: func(n int) error {
			stored += int64(n)
			return nil
		},
	}
	// Upload stream directly to S3 under the file hash key using s3manager (supports io.Reader)
	uploader := s3manager.NewUploaderWithClient(s3Cl)
	_, err = uploader.UploadWithContext(ctx, &s3manager.UploadInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(hash),
		Body:   pr,
	})
	if err != nil {
		return nil, err
	}
	// Ensure file status and stored_size are finalized
	f.Status = StatusStored
	f.StoredSize = f.TotalSize
	if _, err := db.Model(f).Context(ctx).Column("status", "stored_size").WherePK().Update(); err != nil {
		return nil, err
	}
	log.WithFields(log.Fields{"bucket": s.bucket, "resource_id": id, "path": item.PathStr, "key": hash, "size": item.Size}).Info("stored to s3")
	return f, nil
}

func (s *Worker) generateFileHash(ctx context.Context, item ra.ListItem, ei *ra.ExportResponse) (string, error) {
	u := ei.ExportItems["download"].URL
	size := item.Size
	var limitStart int64 = 500 * 1024
	var limitEnd int64 = 500 * 1024
	h := sha256.New()
	h.Write([]byte(fmt.Sprintf("%v", size)))
	if size < limitStart+limitEnd {
		r, err := s.api.Download(ctx, u)
		if err != nil {
			return "", err
		}
		defer func(r io.ReadCloser) {
			_ = r.Close()
		}(r)
		_, err = io.Copy(h, r)
		if err != nil {
			return "", err
		}
	} else {
		r, err := s.api.DownloadWithRange(ctx, u, 0, int(limitStart))
		if err != nil {
			return "", err
		}
		defer func(r io.ReadCloser) {
			_ = r.Close()
		}(r)
		_, err = io.Copy(h, r)
		if err != nil {
			return "", err
		}
		r, err = s.api.DownloadWithRange(ctx, u, int(size-limitEnd), -1)
		if err != nil {
			return "", err
		}
		defer func(r io.ReadCloser) {
			_ = r.Close()
		}(r)
		_, err = io.Copy(h, r)
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
