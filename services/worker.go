package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/anacrolix/torrent/metainfo"
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

// Lease parameters for worker claim. See migration 8_resource_claim.up.sql
// for the rationale. The lease duration must be larger than the heartbeat
// interval times several ticks so a transient DB hiccup does not cause a
// worker to lose its lease while it is still alive.
const (
	leaseDuration     = 2 * time.Minute
	leaseHeartbeat    = 30 * time.Second
	claimIdleSleep    = 5 * time.Second
	storeErrorBackoff = 30 * time.Minute
)

// Worker processes background store and delete jobs for vault resources.
// Each worker goroutine runs an independent claim loop that acquires a
// lease on exactly one resource at a time via an atomic UPDATE ...
// RETURNING against the resource.claim_expires_at column, then processes
// it while a heartbeat goroutine refreshes the lease every leaseHeartbeat.
// On crash, shutdown or stuck upstream, the lease expires naturally and
// another worker can take over from wherever the previous one left off
// (multipart upload resumes via ListPartsPages).
type Worker struct {
	ctx             context.Context
	cancel          context.CancelFunc
	pg              *cs.PG
	s3              *cs.S3Client
	nwrks           int
	api             *Api
	bucket          string
	concur          int
	part            int64
	nats            *cs.NATS
	resourceID      string
	workerBase      string // hostname-derived prefix, suffixed with goroutine index
	verifyIntegrity bool
	wg              sync.WaitGroup
}

const (
	workerCountFlag          = "workers"
	awsBucketFlag            = "aws-bucket"
	awsUploadConcurrencyFlag = "aws-upload-concurrency"
	awsUploadPartSizeFlag    = "aws-upload-part-size"
	resourceIDFlag           = "resource-id"
	verifyIntegrityFlag      = "verify-integrity"
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
		cli.BoolTFlag{
			Name:   verifyIntegrityFlag,
			Usage:  "verify each stored file against torrent piece SHA-1 hashes (default true)",
			EnvVar: "VAULT_VERIFY_INTEGRITY",
		},
	)
}

func NewWorker(c *cli.Context, pgc *cs.PG, s3 *cs.S3Client, api *Api, nt *cs.NATS) *Worker {
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	// Use HOSTNAME (Kubernetes injects the pod name by default) as the
	// base component of the lease owner identifier. Each worker goroutine
	// will append its index so leases are traceable to a specific
	// goroutine on a specific pod.
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "vault-unknown"
	}
	w := &Worker{
		ctx:             ctx,
		cancel:          cancel,
		pg:              pgc,
		s3:              s3,
		nwrks:           c.Int(workerCountFlag),
		api:             api,
		bucket:          c.String(awsBucketFlag),
		concur:          c.Int(awsUploadConcurrencyFlag),
		part:            c.Int64(awsUploadPartSizeFlag),
		nats:            nt,
		resourceID:      c.String(resourceIDFlag),
		workerBase:      host,
		verifyIntegrity: c.BoolT(verifyIntegrityFlag),
	}
	return w
}

// Serve starts nwrks independent claim loops. Each goroutine has its own
// full worker identifier ({hostname}#{index}) that becomes the value of
// resource.claimed_by when it acquires a lease. The Go runtime does the
// scheduling across CPUs; the claim loop itself is simple — try to claim
// one resource, process it, repeat, and sleep claimIdleSleep when the
// queue is empty.
func (s *Worker) Serve() error {
	db := s.pg.Get()
	if db == nil {
		return errors.New("db is not configured")
	}
	if s.resourceID != "" {
		log.WithField("resource_id", s.resourceID).
			Info("Worker started in debug mode for specific resource")
		workerID := s.workerBase + "#debug"
		res, err := s.tryClaim(s.ctx, db, workerID)
		if err != nil {
			return errors.Wrap(err, "debug claim failed")
		}
		if res == nil {
			log.Info("debug mode: nothing to claim for the specified resource_id")
		} else {
			s.processClaimed(s.ctx, db, res, workerID)
		}
		<-s.ctx.Done()
		return nil
	}
	log.WithField("workers", s.nwrks).Info("Worker started")
	for i := 0; i < s.nwrks; i++ {
		s.wg.Add(1)
		workerID := fmt.Sprintf("%s#%d", s.workerBase, i)
		go s.claimLoop(workerID)
	}
	<-s.ctx.Done()
	log.Info("Worker stopped")
	return nil
}

// claimLoop is the long-running worker goroutine. It keeps trying to
// claim a resource; when it gets one, it processes it to completion;
// when the queue is empty it sleeps claimIdleSleep before trying again.
// Each worker goroutine runs independently — the only coordination with
// other goroutines (inside this pod or across pods) is the atomic CAS in
// tryClaim, which is safe because it is a single UPDATE ... RETURNING in
// the database.
func (s *Worker) claimLoop(workerID string) {
	defer s.wg.Done()
	db := s.pg.Get()
	for {
		select {
		case <-s.ctx.Done():
			return
		default:
		}
		res, err := s.tryClaim(s.ctx, db, workerID)
		if err != nil {
			log.WithError(err).WithField("worker", workerID).Warn("claim failed")
			// back off a bit on transient DB errors
			select {
			case <-s.ctx.Done():
				return
			case <-time.After(claimIdleSleep):
			}
			continue
		}
		if res == nil {
			// Nothing to do. Sleep a little before polling again.
			select {
			case <-s.ctx.Done():
				return
			case <-time.After(claimIdleSleep):
			}
			continue
		}
		s.processClaimed(s.ctx, db, res, workerID)
	}
}

// tryClaim atomically picks one eligible resource and acquires a lease on
// it. The combination of SELECT ... FOR UPDATE SKIP LOCKED (for picking a
// candidate without stepping on other workers) and UPDATE ... WHERE
// (claim_expires_at IS NULL OR claim_expires_at < now()) (for the lease
// CAS guarantee) makes the operation safe across any number of workers
// and pods: at most one worker succeeds in claiming any given row per call.
//
// Eligibility rules (mirroring the old process() time constraints but
// expressed on top of lease):
//   - queued_for_storing / queued_for_deletion: always eligible.
//   - storing / deleting with expired (or null) lease: reclaim after crash.
//   - store_error / delete_error: eligible after storeErrorBackoff has
//     elapsed since the last status change (updated_at).
//
// The status is flipped to the corresponding processing status in the
// same UPDATE, so there is no intermediate "just claimed, not yet started"
// state visible to other workers.
func (s *Worker) tryClaim(ctx context.Context, db *pg.DB, workerID string) (*Resource, error) {
	var res Resource
	// The subquery picks one eligible row, and the outer UPDATE takes the
	// lease and flips status. RETURNING gives us the full updated row.
	subquery := fmt.Sprintf(`
		SELECT resource_id FROM resource
		WHERE (claim_expires_at IS NULL OR claim_expires_at < now())
		  AND (
		    status IN (%d, %d)
		    OR (status IN (%d, %d))
		    OR (status IN (%d, %d) AND now() - updated_at > interval '%d seconds')
		  )
		  %s
		ORDER BY
		  CASE WHEN status IN (%d, %d) THEN 0 ELSE 1 END,
		  updated_at ASC
		LIMIT 1
		FOR UPDATE SKIP LOCKED
	`,
		StatusQueuedForStoring, StatusQueuedForDeletion,
		StatusStoring, StatusDeleting,
		StatusStoreError, StatusDeleteError, int(storeErrorBackoff.Seconds()),
		s.debugResourceIDClause(),
		StatusQueuedForStoring, StatusQueuedForDeletion,
	)

	stmt := fmt.Sprintf(`
		UPDATE resource
		SET claim_expires_at = now() + interval '%d seconds',
		    claimed_by = ?,
		    status = CASE
		      WHEN status IN (%d, %d, %d) THEN %d
		      WHEN status IN (%d, %d, %d) THEN %d
		      ELSE status
		    END,
		    error = NULL,
		    updated_at = now()
		WHERE resource_id = (%s)
		RETURNING *
	`,
		int(leaseDuration.Seconds()),
		StatusQueuedForStoring, StatusStoring, StatusStoreError, StatusStoring,
		StatusQueuedForDeletion, StatusDeleting, StatusDeleteError, StatusDeleting,
		subquery,
	)

	_, err := db.QueryContext(ctx, &res, stmt, workerID)
	if err != nil {
		if errors.Is(err, pg.ErrNoRows) {
			return nil, nil
		}
		return nil, errors.Wrap(err, "failed to claim resource")
	}
	if res.ID == "" {
		return nil, nil
	}
	log.WithFields(log.Fields{
		"resource_id": res.ID,
		"status":      res.Status.String(),
		"worker":      workerID,
	}).Info("claimed resource")
	return &res, nil
}

// debugResourceIDClause returns an extra WHERE clause that narrows the
// claim subquery to a specific resource ID when debug mode is active.
// Empty string in normal (production) operation.
func (s *Worker) debugResourceIDClause() string {
	if s.resourceID == "" {
		return ""
	}
	// Safe: resourceID comes from a CLI flag, not user input.
	return fmt.Sprintf("AND resource_id = '%s'", s.resourceID)
}

// processClaimed is invoked once the worker has successfully acquired a
// lease on a resource. It starts the lease heartbeat goroutine, dispatches
// to the correct handler based on the resource's (already flipped)
// processing status, and finally releases the lease.
func (s *Worker) processClaimed(parentCtx context.Context, db *pg.DB, res *Resource, workerID string) {
	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	// Start lease heartbeat. On lease loss (e.g. another worker took over
	// because our DB updates stalled, or someone cleared the lease
	// externally, or our status changed out from under us) the heartbeat
	// cancels ctx so the handler returns promptly.
	stopHeartbeat := s.startLeaseHeartbeat(ctx, cancel, db, res.ID, workerID, res.Status)
	defer stopHeartbeat()

	opLog, logErr := WorkerLogStart(ctx, db, res.ID, res.Status)
	if logErr != nil {
		log.WithError(logErr).WithField("resource_id", res.ID).Warn("failed to create worker log")
	}

	var handlerErr error
	switch res.Status {
	case StatusStoring:
		log.WithField("id", res.ID).Info("storing started")
		handlerErr = s.handleStore(ctx, db, res.ID)
		if handlerErr != nil {
			log.WithError(handlerErr).WithField("id", res.ID).Error("store failed")
			s.handleError(res.ID, handlerErr, StatusStoreError, workerID)
		} else {
			log.WithField("id", res.ID).Info("stored successfully")
			if s.nats != nil {
				if nc := s.nats.Get(); nc != nil {
					b, _ := json.Marshal(map[string]string{"resource_id": res.ID})
					if pubErr := nc.Publish("resource.vaulted", b); pubErr != nil {
						log.WithError(pubErr).WithField("id", res.ID).Error("failed to publish nats message")
					}
				}
			}
			// Successful store reached StatusStored in handleStore; clear
			// the lease for tidiness (status=stored rows never get
			// re-claimed anyway, but leaving a stale claim_expires_at
			// around is confusing when debugging).
			s.releaseLease(db, res.ID, workerID)
		}
	case StatusDeleting:
		log.WithField("id", res.ID).Info("deleting started")
		handlerErr = s.handleDelete(ctx, db, res.ID)
		if handlerErr != nil {
			log.WithError(handlerErr).WithField("id", res.ID).Error("delete failed")
			s.handleError(res.ID, handlerErr, StatusDeleteError, workerID)
		} else {
			log.WithField("id", res.ID).Info("deleted successfully")
			// handleDelete removes the resource row entirely on success,
			// so there's nothing left to release.
		}
	default:
		log.WithField("status", res.Status.String()).Error("unexpected processing status on claimed resource")
		handlerErr = fmt.Errorf("unexpected status: %s", res.Status)
		s.releaseLease(db, res.ID, workerID)
	}

	if opLog != nil {
		if fErr := WorkerLogFinish(ctx, db, opLog.LogID, handlerErr); fErr != nil {
			log.WithError(fErr).WithField("log_id", opLog.LogID).Warn("failed to finish worker log")
		}
	}
}

// startLeaseHeartbeat launches a goroutine that refreshes the lease
// every leaseHeartbeat. If the UPDATE touches 0 rows — meaning we no
// longer own the lease for any reason (lease expired and someone else
// reclaimed it, status was flipped externally, the row was deleted) —
// the heartbeat cancels the job context so the handler aborts promptly.
//
// Returns a function that stops the heartbeat (call in a defer).
func (s *Worker) startLeaseHeartbeat(ctx context.Context, cancelJob context.CancelFunc, db *pg.DB, resourceID, workerID string, processingStatus Status) context.CancelFunc {
	hbCtx, hbCancel := context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(leaseHeartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
			}
			// Use a short independent timeout — we don't want the heartbeat
			// itself to hang on a slow DB and starve the cancellation path.
			uctx, uc := context.WithTimeout(context.Background(), 5*time.Second)
			result, err := db.Model((*Resource)(nil)).
				Context(uctx).
				Set("claim_expires_at = now() + interval '? seconds'", int(leaseDuration.Seconds())).
				Where("resource_id = ?", resourceID).
				Where("claimed_by = ?", workerID).
				Where("status = ?", processingStatus).
				Update()
			uc()
			if err != nil {
				log.WithError(err).
					WithField("resource_id", resourceID).
					WithField("worker", workerID).
					Warn("lease heartbeat db error; will retry next tick")
				continue
			}
			if result.RowsAffected() == 0 {
				log.WithFields(log.Fields{
					"resource_id":       resourceID,
					"worker":            workerID,
					"processing_status": processingStatus.String(),
				}).Warn("lease lost (row gone, reclaimed by another worker, or status changed); cancelling job")
				cancelJob()
				return
			}
		}
	}()
	return hbCancel
}

// releaseLease clears the lease fields for a resource, but only if we
// actually hold it. Used on successful completion and on graceful
// shutdown. Never fails the caller — a stale lease will expire naturally.
func (s *Worker) releaseLease(db *pg.DB, resourceID, workerID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := db.Model((*Resource)(nil)).
		Context(ctx).
		Set("claim_expires_at = NULL").
		Set("claimed_by = NULL").
		Where("resource_id = ?", resourceID).
		Where("claimed_by = ?", workerID).
		Update()
	if err != nil {
		log.WithError(err).
			WithField("resource_id", resourceID).
			WithField("worker", workerID).
			Warn("failed to release lease (will expire naturally)")
	}
}

// Close cancels the worker context, waits for all claim loops to finish,
// then makes a best-effort attempt to release any leases this pod still
// holds. Releasing leases on shutdown means a rolling deploy hands work
// back to the remaining pods immediately instead of making them wait for
// the 2-minute lease to expire before reclaiming.
func (s *Worker) Close() {
	log.Info("closing Worker")
	s.cancel()
	s.wg.Wait()
	// Best-effort: release anything still pinned to this pod. Use a
	// separate short-lived context since s.ctx is already cancelled.
	db := s.pg.Get()
	if db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		result, err := db.Model((*Resource)(nil)).
			Context(ctx).
			Set("claim_expires_at = NULL").
			Set("claimed_by = NULL").
			Where("claimed_by LIKE ?", s.workerBase+"#%").
			Update()
		if err != nil {
			log.WithError(err).Warn("shutdown lease release failed (leases will expire naturally)")
		} else if n := result.RowsAffected(); n > 0 {
			log.WithField("count", n).Info("released leases held by this pod on shutdown")
		}
	}
	log.Info("Worker closed")
}

func (s *Worker) handleStore(ctx context.Context, db *pg.DB, id string) (err error) {
	// Note: the lease heartbeat that keeps this resource claimed by the
	// current worker is started and stopped by processClaimed — there is
	// no heartbeat logic inside handleStore itself. If the lease is lost
	// for any reason, the heartbeat goroutine cancels ctx and every DB
	// operation below returns promptly with context.Canceled.
	//
	// Orphan-safety invariant:
	// 1) File status is flipped to `stored` only inside the atomic
	//    transaction that also inserts the `resource_file` link. So every
	//    file with status=stored is guaranteed to have at least one
	//    resource_file row, and `handleDelete` can always find it.
	// 2) `resource_file` is never wiped up-front. Old links are pruned
	//    only AFTER the new listing has been fully processed (diff-based
	//    pruning at the end). This guarantees that at any intermediate
	//    point — including a crash, a lease loss, or an incoming
	//    queued_for_deletion — `resource_file` reflects a valid superset
	//    of the files that are currently in S3.
	// 3) `handleDelete` runs a post-deletion orphan sweep to catch any
	//    `file` rows left behind by cancelled store attempts.

	cla := &Claims{
		Role: "vault",
	}

	// Phase 1: prefetch the full listing and compute final total_size up
	// front. This way the UI's stored/total ratio rises monotonically
	// instead of bouncing 100% → 99% → 100% on every page boundary as
	// each newly discovered file inflates the denominator.
	listArgs := &ListResourceContentArgs{
		Limit:  100,
		Offset: 0,
	}
	var fileItems []ra.ListItem
	var totalSize int64
	for {
		resp, err := s.api.ListResourceContent(ctx, cla, id, listArgs)
		if err != nil {
			return errors.Wrap(err, "failed to list resource content")
		}
		for _, item := range resp.Items {
			if item.Type == ra.ListTypeFile {
				fileItems = append(fileItems, item)
				totalSize += item.Size
			}
		}
		if len(resp.Items) < int(listArgs.Limit) {
			break
		}
		listArgs.Offset += listArgs.Limit
	}

	// Reset resource counters before (re)storing. total_size is now known
	// and pinned for the whole run; stored_size is rebuilt from zero so the
	// progress ratio is consistent with what we are about to upload.
	// `resource_file` rows are preserved so that a partial re-store cannot
	// drop links to files that are still in S3.
	if _, err := db.Model(&Resource{ID: id}).
		Context(ctx).
		Set("total_size = ?", totalSize).
		Set("stored_size = 0").
		Set("updated_at = now()").
		Set("error = null").
		Where("resource_id = ?", id).
		Update(); err != nil {
		return errors.Wrap(err, "failed to reset resource counters")
	}

	// Fetch torrent metainfo once for the whole run when integrity
	// verification is enabled. The .torrent is served by the seeder at
	// /{infohash}/source.torrent — we reuse the auth tokens already
	// embedded in the first file's export URL to avoid resigning JWTs.
	var mi *metainfo.Info
	if s.verifyIntegrity && len(fileItems) > 0 {
		ei, err := s.api.ExportResourceContent(ctx, cla, id, fileItems[0].ID)
		if err != nil {
			return errors.Wrap(err, "failed to export resource content for torrent fetch")
		}
		raw, err := s.api.FetchTorrent(ctx, ei.ExportItems["download"].URL)
		if err != nil {
			return errors.Wrap(err, "failed to fetch torrent for verification")
		}
		mi, err = parseMetainfo(raw)
		if err != nil {
			return errors.Wrap(err, "failed to parse torrent metainfo")
		}
		log.WithFields(log.Fields{
			"id":           id,
			"num_pieces":   mi.NumPieces(),
			"piece_length": mi.PieceLength,
		}).Info("integrity verification enabled")
	}

	// Track (file_hash, path) pairs produced by this run so stale links
	// can be diff-pruned at the end. Using a map of maps guards against
	// the same path appearing with a different hash (content change).
	type rfKey struct{ Hash, Path string }
	expected := make(map[rfKey]struct{})

	var totalStored int64
	// prevFiles accumulates already-stored files in torrent global-offset
	// order so that the next file's pieceVerifier can pull left-boundary
	// prefix bytes from S3.
	var prevFiles []prevFileInfo
	// Phase 2: store files sequentially using the fixed listing.
	for _, item := range fileItems {
		var fileOff int64 = -1
		if mi != nil {
			fileOff = fileOffsetInTorrent(mi, item.PathStr, item.Size)
		}
		f, err := s.storeFile(ctx, cla, id, item, totalStored, mi, prevFiles)
		if err != nil {
			return errors.Wrap(err, "failed to store file")
		}
		totalStored += item.Size
		if mi != nil && fileOff >= 0 {
			prevFiles = append(prevFiles, prevFileInfo{
				torrentOff: fileOff,
				length:     item.Size,
				hash:       f.Hash,
			})
		}

		// Atomically finalize the file to `stored` and link it to
		// the resource. Wrapping both statements in one transaction
		// closes the window where a file is in S3 but has no
		// resource_file row (which would leak it on deletion).
		// Works for all storeFile return paths: for dedup/hash
		// hits the UPDATE is a no-op.
		if err := db.RunInTransaction(ctx, func(tx *pg.Tx) error {
			if _, err := tx.Model((*File)(nil)).
				Set("status = ?", StatusStored).
				Set("stored_size = ?", f.TotalSize).
				Set("upload_id = ''").
				Set("updated_at = now()").
				Where("hash = ?", f.Hash).
				Update(); err != nil {
				return errors.Wrap(err, "failed to finalize file status")
			}
			rf := &ResourceFile{
				ResourceID: id,
				FileHash:   f.Hash,
				Path:       item.PathStr,
			}
			if _, err := tx.Model(rf).OnConflict("DO NOTHING").Insert(); err != nil {
				return errors.Wrap(err, "failed to insert resource_file")
			}
			return nil
		}); err != nil {
			return err
		}
		expected[rfKey{Hash: f.Hash, Path: item.PathStr}] = struct{}{}

		if _, err := db.Model(&Resource{ID: id}).
			Context(ctx).
			Set("stored_size = ?", totalStored).
			Set("error = null").
			Where("resource_id = ?", id).
			Update(); err != nil {
			return errors.Wrap(err, "failed to update resource stored_size")
		}
	}

	// All new files are stored and linked. Now diff-prune: drop any
	// resource_file rows not in `expected`, and GC orphan file rows/S3
	// objects whose last reference we just removed. Done only on the
	// success path — partial failures preserve the old link set so that
	// a subsequent retry or deletion can clean up safely.
	var existing []ResourceFile
	if err := db.Model(&existing).Context(ctx).Where("resource_id = ?", id).Select(); err != nil {
		return errors.Wrap(err, "failed to list existing resource_file for pruning")
	}
	for _, rf := range existing {
		if _, ok := expected[rfKey{Hash: rf.FileHash, Path: rf.Path}]; ok {
			continue
		}
		log.WithFields(log.Fields{
			"resource_id": id,
			"hash":        rf.FileHash,
			"path":        rf.Path,
		}).Info("pruning stale resource_file link after re-store")
		if _, err := db.Model(&rf).Context(ctx).WherePK().Delete(); err != nil {
			return errors.Wrap(err, "failed to delete stale resource_file link")
		}
		if err := s.gcFileIfUnreferenced(ctx, db, rf.FileHash); err != nil {
			return errors.Wrap(err, "failed to gc unreferenced file after prune")
		}
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

// gcFileIfUnreferenced drops a file row and its S3 artefact (completed
// object or in-flight multipart upload) if no resource_file row still
// references its hash. Called after a resource_file row is deleted to
// keep storage usage in sync with logical references. Safe to call when
// the hash is still referenced — it becomes a no-op.
func (s *Worker) gcFileIfUnreferenced(ctx context.Context, db *pg.DB, hash string) error {
	if s.bucket == "" {
		return errors.New("s3 bucket is not configured")
	}
	count, err := db.Model((*ResourceFile)(nil)).
		Context(ctx).
		Where("file_hash = ?", hash).
		Count()
	if err != nil {
		return errors.Wrap(err, "failed to count file references during gc")
	}
	if count > 0 {
		return nil
	}
	f := &File{Hash: hash}
	if err := db.Model(f).Context(ctx).WherePK().Select(); err != nil {
		if errors.Is(err, pg.ErrNoRows) {
			return nil
		}
		return errors.Wrap(err, "failed to load file row during gc")
	}
	s3Cl := s.s3.Get()
	// Abort any in-flight multipart upload so we don't leave parts
	// accruing quota. Best-effort: the 7-day S3 lifecycle rule is the
	// ultimate backstop.
	if f.UploadID != "" {
		if _, err := s3Cl.AbortMultipartUploadWithContext(ctx, &awss3.AbortMultipartUploadInput{
			Bucket:   aws.String(s.bucket),
			Key:      aws.String(hash),
			UploadId: aws.String(f.UploadID),
		}); err != nil {
			log.WithError(err).WithFields(log.Fields{
				"hash":      hash,
				"upload_id": f.UploadID,
			}).Warn("failed to abort multipart upload during gc; continuing")
		}
	}
	// Delete the completed object if any. Harmless when the key does
	// not exist (e.g. we only had an in-flight multipart).
	if _, err := s3Cl.DeleteObjectWithContext(ctx, &awss3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(hash),
	}); err != nil {
		return errors.Wrapf(err, "failed to delete s3 object during gc, bucket=%s, key=%s", s.bucket, hash)
	}
	if _, err := db.Model(f).Context(ctx).WherePK().Delete(); err != nil && !errors.Is(err, pg.ErrNoRows) {
		return errors.Wrap(err, "failed to delete file row during gc")
	}
	log.WithFields(log.Fields{"bucket": s.bucket, "hash": hash}).Info("gc removed unreferenced file")
	return nil
}

// sweepOrphanFiles finds `file` rows with no `resource_file` reference
// whose updated_at is older than leaseDuration (meaning the creating
// worker has certainly abandoned them) and removes them from S3 and the
// database. Called from handleDelete as a post-deletion safety net.
// Returns the number of files removed. Only a query-level failure is
// returned; per-file errors are logged and skipped.
func (s *Worker) sweepOrphanFiles(ctx context.Context, db *pg.DB) (int, error) {
	cutoff := time.Now().Add(-leaseDuration)
	var orphans []File
	err := db.Model(&orphans).
		Context(ctx).
		ColumnExpr(`"file".*`).
		Join(`LEFT JOIN resource_file AS rf ON rf.file_hash = "file".hash`).
		Where(`rf.resource_id IS NULL`).
		Where(`"file".updated_at < ?`, cutoff).
		OrderExpr(`"file".updated_at ASC`).
		Limit(100).
		Select()
	if err != nil {
		return 0, errors.Wrap(err, "failed to query orphan files for sweep")
	}
	if len(orphans) == 0 {
		return 0, nil
	}
	s3Cl := s.s3.Get()
	swept := 0
	for i := range orphans {
		f := &orphans[i]
		// Re-check reference count — a worker may have linked this file
		// between our query and now.
		cnt, cntErr := db.Model((*ResourceFile)(nil)).
			Context(ctx).
			Where("file_hash = ?", f.Hash).
			Count()
		if cntErr != nil {
			log.WithError(cntErr).WithField("hash", f.Hash).Warn("sweep: re-check failed; skipping")
			continue
		}
		if cnt > 0 {
			continue
		}
		if f.UploadID != "" {
			if _, err := s3Cl.AbortMultipartUploadWithContext(ctx, &awss3.AbortMultipartUploadInput{
				Bucket:   aws.String(s.bucket),
				Key:      aws.String(f.Hash),
				UploadId: aws.String(f.UploadID),
			}); err != nil {
				log.WithError(err).WithField("hash", f.Hash).Warn("sweep: abort multipart failed; continuing")
			}
		}
		if _, err := s3Cl.DeleteObjectWithContext(ctx, &awss3.DeleteObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(f.Hash),
		}); err != nil {
			log.WithError(err).WithField("hash", f.Hash).Warn("sweep: S3 delete failed; skipping")
			continue
		}
		if _, err := db.Model(f).Context(ctx).WherePK().Delete(); err != nil && !errors.Is(err, pg.ErrNoRows) {
			log.WithError(err).WithField("hash", f.Hash).Warn("sweep: DB delete failed")
			continue
		}
		swept++
		log.WithFields(log.Fields{
			"hash":       f.Hash,
			"total_size": f.TotalSize,
			"status":     f.Status.String(),
		}).Info("sweep: removed orphan file")
	}
	return swept, nil
}

func (s *Worker) handleDelete(ctx context.Context, db *pg.DB, id string) (err error) {
	if s.bucket == "" {
		return errors.New("s3 bucket is not configured")
	}
	// Lease heartbeat is managed by processClaimed, same as handleStore.
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
		// If the file was still in-flight (multipart upload not yet
		// completed), abort the upload so its parts don't accumulate
		// S3 quota. Best-effort: a 7-day bucket lifecycle rule is the
		// ultimate backstop, but aborting here keeps cleanup prompt.
		s3Cl := s.s3.Get()
		if f.UploadID != "" {
			if _, err := s3Cl.AbortMultipartUploadWithContext(ctx, &awss3.AbortMultipartUploadInput{
				Bucket:   aws.String(s.bucket),
				Key:      aws.String(rf.FileHash),
				UploadId: aws.String(f.UploadID),
			}); err != nil {
				log.WithError(err).WithFields(log.Fields{
					"bucket":    s.bucket,
					"key":       rf.FileHash,
					"upload_id": f.UploadID,
				}).Warn("failed to abort multipart upload during delete; continuing")
			}
		}
		// No more references — delete S3 object (if configured) and file row
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

	// Best-effort orphan sweep: clean up `file` rows that have no
	// resource_file references. These can appear when a store attempt
	// was cancelled (lease loss, incoming deletion) after uploading to
	// S3 but before the atomic transaction that links file → resource.
	if swept, swErr := s.sweepOrphanFiles(ctx, db); swErr != nil {
		log.WithError(swErr).Warn("post-delete orphan sweep failed")
	} else if swept > 0 {
		log.WithField("swept", swept).Info("post-delete orphan sweep removed files")
	}

	return nil
}

// handleError records a failure on the resource and releases our lease.
// Releasing the lease is important: the next retry attempt (after the
// store_error backoff) should see claim_expires_at IS NULL so tryClaim
// can pick the row up cleanly without relying on the deadline having
// passed. The WHERE on claimed_by + processing status makes the update
// safe against concurrent lease transfer.
func (s *Worker) handleError(id string, err error, errorStatus Status, workerID string) {
	db := s.pg.Get()
	errMsg := err.Error()
	// Determine which processing status we expect the resource to still be in.
	// Only overwrite if the resource is still in that processing state —
	// otherwise an external status change (e.g. QueuedForDeletion) would be lost.
	var expectedStatus Status
	switch errorStatus {
	case StatusStoreError:
		expectedStatus = StatusStoring
	case StatusDeleteError:
		expectedStatus = StatusDeleting
	default:
		expectedStatus = errorStatus
	}
	// Use a fresh context with timeout — the job context may already be cancelled.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, upErr := db.Model((*Resource)(nil)).
		Context(ctx).
		Set("status = ?", errorStatus).
		Set("error = ?", errMsg).
		Set("claim_expires_at = NULL").
		Set("claimed_by = NULL").
		Where("resource_id = ?", id).
		Where("status = ?", expectedStatus).
		Where("claimed_by = ?", workerID).
		Update()
	if upErr != nil {
		log.WithError(upErr).Error("update error status failed")
	} else if result.RowsAffected() == 0 {
		log.WithFields(log.Fields{
			"id":              id,
			"expected_status": expectedStatus.String(),
			"worker":          workerID,
		}).Warn("skipped error status update: resource status/lease changed externally")
	}
}

// runPeriodicFlush calls fn every 10 seconds until the returned cancel
// function is called. It is used by storeFile to flush the current
// stored_size counters on file and resource rows to the DB while a
// multipart upload is in progress, so the UI can show progress.
// Lease heartbeat is handled separately by startLeaseHeartbeat.
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

func (s *Worker) storeFile(ctx context.Context, cla *Claims, id string, item ra.ListItem, totalStored int64, mi *metainfo.Info, prevFiles []prevFileInfo) (*File, error) {

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
		// Verify the S3 object actually exists before trusting DB status.
		// Guards against orphaned "stored" rows left behind by prior race
		// conditions, external S3 cleanup, or multipart uploads that were
		// marked complete in DB but never finalized in S3.
		s3Cl := s.s3.Get()
		_, headErr := s3Cl.HeadObjectWithContext(ctx, &awss3.HeadObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(existing.Hash),
		})
		if headErr == nil {
			return &existing, nil
		}
		log.WithError(headErr).WithFields(log.Fields{
			"hash": existing.Hash,
			"path": item.PathStr,
		}).Warn("dedup candidate marked stored but missing in S3, forcing re-upload")
		// Reset the stale row so the regular upload path below re-uploads it
		// when the same content hash is recomputed via generateFileHash.
		if _, err := db.Model(&existing).Context(ctx).
			Set("status = ?", StatusStoring).
			Set("stored_size = 0").
			Set("upload_id = ''").
			Set("updated_at = now()").
			WherePK().Update(); err != nil {
			return nil, errors.Wrap(err, "failed to reset stale stored file row from dedup")
		}
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
	s3Cl := s.s3.Get()
	// Only short-circuit if the file is *fully* stored AND the S3 object
	// actually exists. Returning "fresh but in-progress" rows here (the old
	// `updated_at < 10s && upload_id == ""` branch) was unsafe: a concurrent
	// worker may have just inserted the row but not yet started the upload,
	// in which case we would declare the file stored while nothing was in S3.
	if err == nil && f.Status == StatusStored {
		_, headErr := s3Cl.HeadObjectWithContext(ctx, &awss3.HeadObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(hash),
		})
		if headErr == nil {
			return f, nil
		}
		log.WithError(headErr).WithField("hash", hash).
			Warn("file marked stored but missing in S3, re-uploading")
		// Fall through into the re-upload path below. Reset the row so the
		// multipart upload logic starts from scratch.
		f.Status = StatusStoring
		f.StoredSize = 0
		f.UploadID = ""
		if _, err := db.Model(f).Context(ctx).
			Column("status", "stored_size", "upload_id").
			WherePK().Update(); err != nil {
			return nil, errors.Wrap(err, "failed to reset stale stored file row")
		}
	}
	_, err = db.Model(f).Context(ctx).Insert()
	if err != nil && !IsPGDuplicateKey(err) {
		return nil, errors.Wrap(err, "failed to insert file")
	}

	// Zero-byte files can't use multipart upload — S3 rejects
	// CompleteMultipartUpload with an empty parts list (MalformedXML).
	// All empty files share the same hash (sha256 of the size string "0"),
	// so a single empty S3 object is enough for dedup.
	if f.TotalSize == 0 {
		if f.UploadID != "" {
			_, _ = s3Cl.AbortMultipartUploadWithContext(ctx, &awss3.AbortMultipartUploadInput{
				Bucket:   aws.String(s.bucket),
				Key:      aws.String(hash),
				UploadId: aws.String(f.UploadID),
			})
			f.UploadID = ""
			f.StoredSize = 0
			f.PartSize = partSize
			if _, err := db.Model(f).Context(ctx).Column("upload_id", "stored_size", "part_size").WherePK().Update(); err != nil {
				return nil, errors.Wrap(err, "failed to reset upload for zero-byte file")
			}
		}
		_, err := s3Cl.PutObjectWithContext(ctx, &awss3.PutObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(hash),
			Body:   bytes.NewReader(nil),
		})
		if err != nil {
			return nil, errors.Wrapf(err, "failed to put zero-byte object, bucket=%s, key=%s", s.bucket, hash)
		}
		log.WithFields(log.Fields{"bucket": s.bucket, "resource_id": id, "path": item.PathStr, "key": hash, "size": 0}).Info("stored zero-byte file to s3")
		return f, nil
	}

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

	// Build the inline integrity verifier when metainfo is available. The
	// verifier hashes piece-aligned chunks as they flow through this loop —
	// catching a mismatch here lets us abort the multipart upload before
	// committing bad bytes, instead of having to re-download from S3 after
	// completion. Resume case: if `stored > 0`, Bootstrap pulls the already-
	// uploaded prefix from this file's S3 object so the in-progress piece
	// state matches the actual stored bytes.
	var verifier *pieceVerifier
	if mi != nil {
		fileOff := fileOffsetInTorrent(mi, item.PathStr, item.Size)
		if fileOff < 0 {
			return nil, errors.Errorf("verify: file %q (size %d) not found in torrent metainfo", item.PathStr, item.Size)
		}
		verifier = newPieceVerifier(mi, fileOff, item.Size, prevFiles, newS3ByteFetcher(s3Cl, s.bucket))
		if err := verifier.Bootstrap(ctx, hash, stored); err != nil {
			// Bootstrap pulls already-uploaded bytes through the hasher;
			// failure means the resume prefix is corrupt (or prev files
			// can't be read). Abort the multipart upload and reset the
			// row so the next worker pass starts the upload fresh.
			log.WithError(err).WithField("hash", hash).Warn("verifier bootstrap failed, aborting and resetting upload state")
			_, _ = s3Cl.AbortMultipartUploadWithContext(ctx, &awss3.AbortMultipartUploadInput{
				Bucket:   aws.String(s.bucket),
				Key:      aws.String(hash),
				UploadId: aws.String(f.UploadID),
			})
			f.UploadID = ""
			f.StoredSize = 0
			f.PartSize = partSize
			if _, dbErr := db.Model(f).Context(ctx).Column("upload_id", "stored_size", "part_size").WherePK().Update(); dbErr != nil {
				return nil, errors.Wrap(dbErr, "failed to reset upload after verifier bootstrap failure")
			}
			return nil, errors.Wrap(err, "verifier bootstrap")
		}
	}

	const maxDownloadRetries = 3

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

	// openDownload creates a new download stream from the current stored offset
	// with a dynamic timeout based on remaining bytes.
	openDownload := func() (io.ReadCloser, context.CancelFunc, error) {
		remaining := f.TotalSize - stored
		dlTimeout := time.Duration(remaining/(2*1024*1024)) * time.Second
		if dlTimeout < 20*time.Minute {
			dlTimeout = 20 * time.Minute
		}
		if dlTimeout > 6*time.Hour {
			dlTimeout = 6 * time.Hour
		}
		dctx, dcancel := context.WithTimeout(ctx, dlTimeout)
		r, err := s.api.DownloadWithRange(dctx, u, int(stored), -1)
		if err != nil {
			dcancel()
			return nil, nil, errors.Wrapf(err, "failed to download file content with range, url=%s, start=%d", u, stored)
		}
		return r, dcancel, nil
	}

	r, dcancel, err := openDownload()
	if err != nil {
		return nil, err
	}
	defer func() {
		if dcancel != nil {
			dcancel()
		}
	}()
	defer func() {
		if r != nil {
			_ = r.Close()
		}
	}()

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
		_, readErr := io.ReadFull(r, buf)
		if readErr != nil {
			// Connection dropped or timeout — retry download from current offset.
			retried := false
			for attempt := 1; attempt <= maxDownloadRetries; attempt++ {
				log.WithFields(log.Fields{
					"resource_id": id,
					"stored":      stored,
					"attempt":     attempt,
				}).WithError(readErr).Warn("download stream interrupted, reconnecting")
				// r/dcancel may be nil if a previous openDownload() in this
				// retry loop failed — only clean up a live reader.
				if r != nil {
					_ = r.Close()
				}
				if dcancel != nil {
					dcancel()
				}
				time.Sleep(time.Duration(attempt) * 5 * time.Second)
				r, dcancel, err = openDownload()
				if err != nil {
					readErr = err
					continue
				}
				_, readErr = io.ReadFull(r, buf)
				if readErr == nil {
					retried = true
					break
				}
			}
			if !retried {
				setUploadErr(errors.Wrap(readErr, "failed to read part data from download stream"))
				break
			}
		}

		// Inline integrity check before the part is uploaded — catches a
		// piece SHA-1 mismatch in time to abort the multipart upload
		// without committing bad bytes.
		if verifier != nil {
			if vErr := verifier.Feed(ctx, buf); vErr != nil {
				setUploadErr(errors.Wrap(vErr, "integrity verification failed during upload"))
				break
			}
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
		_, _ = s3Cl.AbortMultipartUploadWithContext(ctx, &awss3.AbortMultipartUploadInput{
			Bucket:   aws.String(s.bucket),
			Key:      aws.String(hash),
			UploadId: aws.String(f.UploadID),
		})
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

	// NOTE: file status intentionally stays `storing` here. The sole
	// point that flips a file to `stored` is the atomic transaction in
	// handleStore, which sets status=stored AND inserts the resource_file
	// link in one commit. This guarantees every `stored` file always has
	// at least one resource_file reference, eliminating the orphan window
	// that previously existed between this point and the transaction.
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
