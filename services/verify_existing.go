package services

import (
	"context"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/aws/aws-sdk-go/aws"
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
}

func VerifyExistingOptionsFromContext(c *cli.Context) VerifyExistingOptions {
	return VerifyExistingOptions{
		Threshold:  c.Int64(verifyExistingThresholdFlag),
		Limit:      c.Int(verifyExistingLimitFlag),
		DryRun:     c.Bool(verifyExistingDryRunFlag),
		ResourceID: c.String(verifyExistingResourceIDFlag),
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

		mi, err := fetchMetainfoForResource(ctx, api, cla, r.ID)
		if err != nil {
			log.WithError(err).WithField("id", r.ID).Error("verify-existing: failed to fetch metainfo, skipping")
			stats.Errors++
			continue
		}

		var rfs []ResourceFile
		if err := db.Model(&rfs).Context(ctx).Where("resource_id = ?", r.ID).Select(); err != nil {
			log.WithError(err).WithField("id", r.ID).Error("verify-existing: failed to list resource files")
			stats.Errors++
			continue
		}

		corrupt := make(map[string]struct{})
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
			if err := verifyFileAgainstMetainfo(ctx, s3Cl, bucket, f.Hash, mi, fileOff, f.TotalSize); err != nil {
				log.WithError(err).WithFields(log.Fields{
					"id":   r.ID,
					"hash": f.Hash,
					"path": rf.Path,
				}).Warn("verify-existing: file failed integrity check")
				corrupt[f.Hash] = struct{}{}
			}
		}

		if len(corrupt) == 0 {
			stats.Clean++
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

		for hash := range corrupt {
			affected, err := invalidateCorruptFile(ctx, db, s3Cl, bucket, hash)
			if err != nil {
				log.WithError(err).WithField("hash", hash).Error("verify-existing: failed to invalidate corrupt file")
				stats.Errors++
				continue
			}
			stats.S3Deleted++
			stats.Requeued += affected
		}
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

// fetchMetainfoForResource pulls the .torrent for a resource via the export
// URL of its first file and parses out the Info dict.
func fetchMetainfoForResource(ctx context.Context, api *Api, cla *Claims, resourceID string) (*metainfo.Info, error) {
	listArgs := &ListResourceContentArgs{Limit: 100}
	resp, err := api.ListResourceContent(ctx, cla, resourceID, listArgs)
	if err != nil {
		return nil, errors.Wrap(err, "list resource content")
	}
	var firstFileID string
	for _, it := range resp.Items {
		if it.Type == ra.ListTypeFile {
			firstFileID = it.ID
			break
		}
	}
	if firstFileID == "" {
		return nil, errors.New("no file items in resource listing")
	}
	ei, err := api.ExportResourceContent(ctx, cla, resourceID, firstFileID)
	if err != nil {
		return nil, errors.Wrap(err, "export resource content")
	}
	raw, err := api.FetchTorrent(ctx, ei.ExportItems["download"].URL)
	if err != nil {
		return nil, errors.Wrap(err, "fetch torrent")
	}
	mi, err := parseMetainfo(raw)
	if err != nil {
		return nil, errors.Wrap(err, "parse metainfo")
	}
	return mi, nil
}

// invalidateCorruptFile removes a corrupt File row, every resource_file
// link to it, and the S3 object — then flips every owning Resource to
// queued_for_storing so workers re-store from a fixed seeder.
//
// Returns the number of resources re-queued.
func invalidateCorruptFile(ctx context.Context, db *pg.DB, s3Cl *awss3.S3, bucket, hash string) (int, error) {
	var owners []ResourceFile
	if err := db.Model(&owners).Context(ctx).Where("file_hash = ?", hash).Select(); err != nil {
		return 0, errors.Wrap(err, "list owning resource_files")
	}

	if err := db.RunInTransaction(ctx, func(tx *pg.Tx) error {
		for _, rf := range owners {
			if _, err := tx.Model((*Resource)(nil)).
				Set("status = ?", StatusQueuedForStoring).
				Set("error = ?", "queued for re-store: integrity check failed for hash "+hash).
				Set("claim_expires_at = NULL").
				Set("claimed_by = NULL").
				Set("updated_at = now()").
				Where("resource_id = ?", rf.ResourceID).
				Where("status = ?", StatusStored).
				Update(); err != nil {
				return errors.Wrap(err, "requeue resource")
			}
		}
		if _, err := tx.Model((*ResourceFile)(nil)).
			Where("file_hash = ?", hash).
			Delete(); err != nil {
			return errors.Wrap(err, "delete resource_file")
		}
		if _, err := tx.Model(&File{Hash: hash}).WherePK().Delete(); err != nil && !errors.Is(err, pg.ErrNoRows) {
			return errors.Wrap(err, "delete file row")
		}
		return nil
	}); err != nil {
		return 0, err
	}

	if _, err := s3Cl.DeleteObjectWithContext(ctx, &awss3.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(hash),
	}); err != nil {
		// DB is already invalidated; the S3 object is now an orphan that
		// the bucket lifecycle rule cleans up. Surface as warning, not
		// fatal — the resource has been re-queued either way.
		log.WithError(err).WithField("hash", hash).Warn("verify-existing: S3 delete failed; will rely on lifecycle rule")
	}

	return len(owners), nil
}
