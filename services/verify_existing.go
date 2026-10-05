package services

import (
	"bytes"
	"context"
	"crypto/sha1"
	"net/http"

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

const (
	verifyExistingThresholdFlag  = "verify-threshold"
	verifyExistingDryRunFlag     = "verify-dry-run"
	verifyExistingLimitFlag      = "verify-limit"
	verifyExistingResourceIDFlag = "verify-resource-id"
	verifyExistingBoundariesFlag = "verify-boundaries-only"
)

func RegisterVerifyExistingFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.StringFlag{
			Name:   awsBucketFlag,
			Usage:  "aws bucket",
			EnvVar: "AWS_BUCKET",
		},
		cli.Int64Flag{
			Name:   verifyExistingThresholdFlag,
			Usage:  "verify only resources whose total_size is >= this many bytes (0 = all)",
			Value:  50 * 1024 * 1024 * 1024,
			EnvVar: "VAULT_VERIFY_THRESHOLD",
		},
		cli.IntFlag{
			Name:   verifyExistingLimitFlag,
			Usage:  "maximum number of resources to verify in this run",
			Value:  100,
			EnvVar: "VAULT_VERIFY_LIMIT",
		},
		cli.BoolFlag{
			Name:   verifyExistingDryRunFlag,
			Usage:  "report mismatches without invalidating files or re-queueing resources",
			EnvVar: "VAULT_VERIFY_DRY_RUN",
		},
		cli.BoolFlag{
			Name:   verifyExistingBoundariesFlag,
			Usage:  "check only the pieces holding each file's last byte (file boundaries, BEP 47 padding) — one piece read per file, no full-file streaming",
			EnvVar: "VAULT_VERIFY_BOUNDARIES_ONLY",
		},
		cli.StringFlag{
			Name:   verifyExistingResourceIDFlag,
			Usage:  "verify a single resource by infohash (overrides threshold/limit)",
			EnvVar: "VAULT_VERIFY_RESOURCE_ID",
		},
	)
}

type VerifyExistingOptions struct {
	Threshold  int64
	Limit      int
	DryRun     bool
	ResourceID string
	// BoundariesOnly skips streaming whole files and checks only the pieces
	// holding each file's last byte.
	BoundariesOnly bool
}

func VerifyExistingOptionsFromContext(c *cli.Context) VerifyExistingOptions {
	return VerifyExistingOptions{
		Threshold:  c.Int64(verifyExistingThresholdFlag),
		Limit:      c.Int(verifyExistingLimitFlag),
		DryRun:     c.Bool(verifyExistingDryRunFlag),
		ResourceID: c.String(verifyExistingResourceIDFlag),

		BoundariesOnly: c.Bool(verifyExistingBoundariesFlag),
	}
}

type VerifyExistingStats struct {
	Resources    int
	Clean        int
	Corrupt      int
	BadFiles     int
	Requeued     int
	S3Deleted    int
	Errors       int
	BytesScanned int64
	// BoundaryPieces checked, and mismatches the source could not attribute.
	BoundaryPieces int
	Unresolved     int
}

// RunVerifyExisting is the one-shot maintenance pass that recomputes
// BitTorrent piece SHA-1 hashes for every File row of every Resource larger
// than opts.Threshold, comparing them against the torrent's piece hashes.
//
// On any mismatch (which implies the seeder served zero-filled bytes during
// the original ingest), the corrupt File row, its S3 object, and every
// resource_file link are removed, and every owning Resource is flipped to
// queued_for_storing so the worker will re-fetch from a now-fixed seeder
// and re-upload with end-to-end verification.
//
// In dry-run mode nothing is mutated — only stats and per-file warnings.
func RunVerifyExisting(ctx context.Context, pgCl *cs.PG, s3c *cs.S3Client, api *Api, bucket string, opts VerifyExistingOptions) (VerifyExistingStats, error) {
	var stats VerifyExistingStats
	db := pgCl.Get()
	if db == nil {
		return stats, errors.New("db is nil")
	}
	if bucket == "" {
		return stats, errors.New("s3 bucket is not configured")
	}
	s3Cl := s3c.Get()

	var resources []Resource
	q := db.Model(&resources).
		Context(ctx).
		Where("status = ?", StatusStored).
		OrderExpr("total_size DESC")
	if opts.ResourceID != "" {
		q = q.Where("resource_id = ?", opts.ResourceID)
	} else {
		q = q.Limit(opts.Limit)
		if opts.Threshold > 0 {
			q = q.Where("total_size >= ?", opts.Threshold)
		}
	}
	if err := q.Select(); err != nil {
		return stats, errors.Wrap(err, "failed to list stored resources")
	}
	stats.Resources = len(resources)
	log.WithFields(log.Fields{
		"count":     stats.Resources,
		"threshold": opts.Threshold,
		"limit":     opts.Limit,
		"dry_run":   opts.DryRun,
	}).Info("verify-existing: starting sweep")

	cla := &Claims{Role: "vault"}

	for ri := range resources {
		r := &resources[ri]
		stats.BytesScanned += r.TotalSize
		log.WithFields(log.Fields{
			"id":         r.ID,
			"total_size": r.TotalSize,
		}).Info("verify-existing: scanning resource")

		mi, items, err := fetchMetainfoForResource(ctx, api, cla, r.ID)
		if err != nil {
			log.WithError(err).WithField("id", r.ID).Error("verify-existing: failed to fetch metainfo, skipping")
			stats.Errors++
			continue
		}

		if !mi.HasV1() {
			log.WithField("id", r.ID).Info("verify-existing: torrent has no v1 piece hashes, nothing to verify against")
			continue
		}

		var rfs []ResourceFile
		if err := db.Model(&rfs).Context(ctx).Where("resource_id = ?", r.ID).Select(); err != nil {
			log.WithError(err).WithField("id", r.ID).Error("verify-existing: failed to list resource files")
			stats.Errors++
			continue
		}

		errs := stats.Errors
		corrupt := make(map[string]struct{})
		var stored []prevFileInfo
		for _, rf := range rfs {
			f := &File{Hash: rf.FileHash}
			if err := db.Model(f).Context(ctx).WherePK().Select(); err != nil {
				if errors.Is(err, pg.ErrNoRows) {
					continue
				}
				log.WithError(err).WithField("hash", rf.FileHash).Warn("verify-existing: failed to load file row")
				stats.Errors++
				continue
			}
			fileOff := fileOffsetInTorrent(mi, rf.Path, f.TotalSize)
			if fileOff < 0 {
				log.WithFields(log.Fields{
					"id":   r.ID,
					"path": rf.Path,
					"size": f.TotalSize,
				}).Warn("verify-existing: file not found in metainfo, skipping")
				stats.Errors++
				continue
			}
			head, err := s3Cl.HeadObjectWithContext(ctx, &awss3.HeadObjectInput{
				Bucket: aws.String(bucket),
				Key:    aws.String(f.Hash),
			})
			if reqErr, ok := err.(awserr.RequestFailure); ok && reqErr.StatusCode() == http.StatusNotFound {
				// Linked and stored but gone: users get a 404. Also what an
				// invalidation leaves if its commit fails after the deletes.
				log.WithFields(log.Fields{"id": r.ID, "hash": f.Hash, "path": rf.Path}).Warn("verify-existing: stored file has no object")
				corrupt[f.Hash] = struct{}{}
				continue
			}
			if err != nil {
				log.WithError(err).WithFields(log.Fields{"id": r.ID, "hash": f.Hash}).Error("verify-existing: could not stat object")
				stats.Errors++
				continue
			}
			if head.ContentLength == nil {
				log.WithFields(log.Fields{"id": r.ID, "hash": f.Hash}).Error("verify-existing: object size unknown")
				stats.Errors++
				continue
			}
			// A size mismatch is as definite as a hash mismatch: multipart
			// uploads of 04-05.02.2026 completed without their last part.
			if got := *head.ContentLength; got != f.TotalSize {
				log.WithFields(log.Fields{
					"id":          r.ID,
					"hash":        f.Hash,
					"path":        rf.Path,
					"size":        f.TotalSize,
					"object_size": got,
				}).Warn("verify-existing: stored object size differs from the file")
				corrupt[f.Hash] = struct{}{}
				continue
			}
			stored = append(stored, prevFileInfo{torrentOff: fileOff, length: f.TotalSize, hash: f.Hash})
			if opts.BoundariesOnly {
				continue
			}
			if err := verifyFileAgainstMetainfo(ctx, s3Cl, bucket, f.Hash, mi, fileOff, f.TotalSize); err != nil {
				fields := log.Fields{"id": r.ID, "hash": f.Hash, "path": rf.Path}
				// Only a piece hash mismatch is corruption. A read error is
				// not: invalidating on it deletes a good object.
				var m *pieceMismatchError
				if !errors.As(err, &m) {
					log.WithError(err).WithFields(fields).Error("verify-existing: could not check file")
					stats.Errors++
					continue
				}
				log.WithError(err).WithFields(fields).Warn("verify-existing: file failed integrity check")
				corrupt[f.Hash] = struct{}{}
			}
		}

		checked, bad, unresolved, err := checkBoundaryPieces(ctx, s3Cl, bucket, mi, stored, newSourceFetcher(api, cla, r.ID, mi, items))
		stats.BoundaryPieces += checked
		stats.Unresolved += unresolved
		if err != nil {
			log.WithError(err).WithField("id", r.ID).Error("verify-existing: boundary check failed")
			stats.Errors++
		}
		for _, h := range bad {
			log.WithFields(log.Fields{"id": r.ID, "hash": h}).Warn("verify-existing: file failed boundary piece check")
			corrupt[h] = struct{}{}
		}

		if len(corrupt) == 0 {
			// A resource with a file left unchecked is not clean.
			if stats.Errors == errs {
				stats.Clean++
			}
			continue
		}
		stats.Corrupt++
		stats.BadFiles += len(corrupt)

		if opts.DryRun {
			log.WithFields(log.Fields{
				"id":         r.ID,
				"bad_files":  len(corrupt),
				"total_size": r.TotalSize,
			}).Warn("verify-existing: would invalidate (dry-run)")
			continue
		}

		hashes := make([]string, 0, len(corrupt))
		for h := range corrupt {
			hashes = append(hashes, h)
		}
		requeued, err := invalidateCorruptFiles(ctx, db, s3Cl, bucket, hashes, "")
		if err != nil {
			log.WithError(err).WithField("id", r.ID).Error("verify-existing: failed to invalidate corrupt files")
			stats.Errors++
			continue
		}
		stats.S3Deleted += len(hashes)
		stats.Requeued += requeued
	}

	log.WithFields(log.Fields{
		"resources":     stats.Resources,
		"clean":         stats.Clean,
		"corrupt":       stats.Corrupt,
		"bad_files":     stats.BadFiles,
		"requeued":      stats.Requeued,
		"s3_deleted":    stats.S3Deleted,
		"errors":        stats.Errors,
		"bytes_scanned": stats.BytesScanned,
	}).Info("verify-existing: sweep complete")
	return stats, nil
}

// fetchMetainfoForResource lists the resource's files and pulls the .torrent
// via the export URL of its first file.
func fetchMetainfoForResource(ctx context.Context, api *Api, cla *Claims, resourceID string) (*metainfo.Info, []ra.ListItem, error) {
	listArgs := &ListResourceContentArgs{Limit: 100}
	var items []ra.ListItem
	for {
		resp, err := api.ListResourceContent(ctx, cla, resourceID, listArgs)
		if err != nil {
			return nil, nil, errors.Wrap(err, "list resource content")
		}
		for _, it := range resp.Items {
			if it.Type == ra.ListTypeFile {
				items = append(items, it)
			}
		}
		if len(resp.Items) < int(listArgs.Limit) {
			break
		}
		listArgs.Offset += listArgs.Limit
	}
	if len(items) == 0 {
		return nil, nil, errors.New("no file items in resource listing")
	}
	ei, err := api.ExportResourceContent(ctx, cla, resourceID, items[0].ID)
	if err != nil {
		return nil, nil, errors.Wrap(err, "export resource content")
	}
	raw, err := api.FetchTorrent(ctx, ei.ExportItems["download"].URL, resourceID)
	if err != nil {
		return nil, nil, errors.Wrap(err, "fetch torrent")
	}
	mi, err := parseMetainfo(raw)
	if err != nil {
		return nil, nil, errors.Wrap(err, "parse metainfo")
	}
	return mi, items, nil
}

// checkBoundaryPieces verifies, for every stored file that does not end on a
// piece boundary, the piece holding its last byte: the bytes upload-time
// verification skipped before it checked right-boundary pieces (file tails,
// BEP 47 padding, files smaller than a piece, the torrent's short last piece).
// The piece is read from the stored objects, padding as zeros; a piece touching
// a file this resource does not store is skipped. A mismatch is blamed through
// the source (blameStoredFiles); one it cannot attribute is counted unresolved.
func checkBoundaryPieces(ctx context.Context, s3Cl *awss3.S3, bucket string, mi *metainfo.Info, files []prevFileInfo, src sourceFetcher) (checked int, bad []string, unresolved int, err error) {
	pl := mi.PieceLength
	if pl <= 0 {
		return 0, nil, 0, nil
	}
	byOff := make(map[int64]prevFileInfo, len(files))
	for _, f := range files {
		byOff[f.torrentOff] = f
	}
	spans := torrentSpans(mi, nil)
	fetch := newS3ByteFetcher(s3Cl, bucket)
	seen := map[int]bool{}
	for _, f := range files {
		end := f.torrentOff + f.length
		if f.length == 0 || end%pl == 0 {
			continue
		}
		idx := int((end - 1) / pl)
		if seen[idx] {
			continue
		}
		seen[idx] = true
		start := int64(idx) * pl
		pieceEnd := start + mi.Piece(idx).V1Length()
		buf := make([]byte, 0, pieceEnd-start)
		var involved []prevFileInfo
		complete := true
		for _, sp := range spans {
			from, to := max(start, sp.off), min(pieceEnd, sp.off+sp.length)
			if from >= to {
				continue
			}
			if sp.pad {
				buf = append(buf, make([]byte, to-from)...)
				continue
			}
			pf, ok := byOff[sp.off]
			if !ok || pf.length != sp.length {
				complete = false
				break
			}
			data, err := fetch(ctx, pf.hash, from-sp.off, to-sp.off)
			if err != nil {
				return checked, bad, unresolved, err
			}
			buf = append(buf, data...)
			involved = append(involved, pf)
		}
		want := mi.Piece(idx).V1Hash()
		if !complete || !want.Ok {
			continue
		}
		checked++
		sum := sha1.Sum(buf)
		if bytes.Equal(sum[:], want.Value[:]) {
			continue
		}
		m := &pieceMismatchError{Piece: idx, Start: start, End: pieceEnd, Got: sum[:], Want: want.Value[:]}
		blamed, err := blameStoredFiles(ctx, s3Cl, bucket, involved, src, m)
		if err != nil {
			return checked, bad, unresolved, err
		}
		if len(blamed) == 0 {
			unresolved++
			log.WithError(m).Warn("verify-existing: boundary piece mismatch the source cannot attribute")
			continue
		}
		for _, b := range blamed {
			bad = append(bad, b.hash)
		}
	}
	return checked, bad, unresolved, nil
}

// invalidateCorruptFiles drops corrupt files -- their File rows, every
// resource_file link to them (ON DELETE CASCADE) and their S3 objects -- and
// flips their stored owners to queued_for_storing, all in one transaction.
//
// The file rows are locked first: a worker finalizing a link to one of them
// waits, then fails on the foreign key and retries. The owners are locked
// next, so no worker claims one halfway (tryClaim skips locked rows) and
// reuses a file not yet dropped. An owner other than self that a worker is
// storing or deleting may already be using a file, and a file row that is not
// stored is being re-uploaded; then nothing is dropped and the caller retries
// later. self, the resource a worker is storing, is neither checked nor
// re-queued: its store fails and is retried.
//
// Returns the number of resources re-queued.
func invalidateCorruptFiles(ctx context.Context, db *pg.DB, s3Cl *awss3.S3, bucket string, hashes []string, self string) (int, error) {
	requeued := 0
	err := db.RunInTransaction(ctx, func(tx *pg.Tx) error {
		var reuploading int
		if _, err := tx.QueryOneContext(ctx, pg.Scan(&reuploading), `
			SELECT count(*) FILTER (WHERE status <> ?)
			FROM (SELECT status FROM file WHERE hash IN (?) ORDER BY hash FOR UPDATE) f`,
			StatusStored, pg.In(hashes)); err != nil {
			return errors.Wrap(err, "lock file rows")
		}
		if reuploading > 0 {
			// A worker found the object bad and is uploading it again; that
			// fixes it for every owner.
			return errors.New("a file is being re-uploaded")
		}
		var owners []Resource
		if _, err := tx.QueryContext(ctx, &owners, `
			SELECT * FROM resource
			WHERE resource_id IN (SELECT resource_id FROM resource_file WHERE file_hash IN (?))
			  AND resource_id <> ?
			ORDER BY resource_id
			FOR UPDATE`, pg.In(hashes), self); err != nil {
			return errors.Wrap(err, "lock owning resources")
		}
		ids := make([]string, 0, len(owners))
		for _, o := range owners {
			switch o.Status {
			case StatusStoring, StatusDeleting:
				return errors.Errorf("owner %s is %v: it may be using the file", o.ID, o.Status)
			case StatusStored:
				ids = append(ids, o.ID)
			}
			// An idle owner (queued, failed) loses the link and stores the
			// file again from scratch when it runs.
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM file WHERE hash IN (?)`, pg.In(hashes)); err != nil {
			return errors.Wrap(err, "delete file rows")
		}
		if len(ids) > 0 {
			res, err := tx.ExecContext(ctx, `
				UPDATE resource
				SET status = ?, error = ?, claim_expires_at = NULL, claimed_by = NULL, updated_at = now()
				WHERE resource_id IN (?) AND status = ?`,
				StatusQueuedForStoring, "queued for re-store: integrity check failed", pg.In(ids), StatusStored)
			if err != nil {
				return errors.Wrap(err, "requeue owning resources")
			}
			requeued = res.RowsAffected()
		}
		// Deleted while the owners are locked, so no owner's re-upload of the
		// same key can complete first. A failed delete leaves an object no
		// row links; the re-store overwrites it.
		for _, h := range hashes {
			if _, err := s3Cl.DeleteObjectWithContext(ctx, &awss3.DeleteObjectInput{
				Bucket: aws.String(bucket),
				Key:    aws.String(h),
			}); err != nil {
				log.WithError(err).WithField("hash", h).Warn("S3 delete of a corrupt file failed; its rows are dropped anyway")
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return requeued, nil
}
