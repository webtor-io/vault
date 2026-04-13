package services

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	awss3 "github.com/aws/aws-sdk-go/service/s3"
	pg "github.com/go-pg/pg/v10"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	cs "github.com/webtor-io/common-services"
)

// Flags for the `gc` command.
const (
	gcGracePeriodFlag = "gc-grace-period"
	gcBatchLimitFlag  = "gc-batch-limit"
	gcDryRunFlag      = "gc-dry-run"
)

// RegisterGCFlags adds CLI flags controlling the gc command.
func RegisterGCFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.StringFlag{
			Name:   awsBucketFlag,
			Usage:  "aws bucket",
			EnvVar: "AWS_BUCKET",
		},
		cli.DurationFlag{
			Name:   gcGracePeriodFlag,
			Usage:  "skip files whose updated_at is newer than this grace window",
			Value:  4 * time.Hour,
			EnvVar: "GC_GRACE_PERIOD",
		},
		cli.IntFlag{
			Name:   gcBatchLimitFlag,
			Usage:  "maximum number of orphan files processed per run",
			Value:  1000,
			EnvVar: "GC_BATCH_LIMIT",
		},
		cli.BoolFlag{
			Name:   gcDryRunFlag,
			Usage:  "report what would be deleted without touching S3 or DB",
			EnvVar: "GC_DRY_RUN",
		},
	)
}

// GCOptions controls a single gc run.
type GCOptions struct {
	Grace  time.Duration
	Limit  int
	DryRun bool
}

// GCStats summarises the outcome of a gc run.
type GCStats struct {
	Candidates int
	Deleted    int
	Failed     int
	BytesFreed int64
}

// GCOptionsFromContext builds GCOptions from CLI flags.
func GCOptionsFromContext(c *cli.Context) GCOptions {
	return GCOptions{
		Grace:  c.Duration(gcGracePeriodFlag),
		Limit:  c.Int(gcBatchLimitFlag),
		DryRun: c.Bool(gcDryRunFlag),
	}
}

// RunGC scans for File rows that have no referencing ResourceFile row
// and are older than opts.Grace, then removes them from S3 (aborting
// any in-flight multipart upload first) and from the database. It is
// safe to run concurrently with the worker: the grace window keeps it
// away from rows the worker is actively touching, and we re-check the
// reference count right before deletion.
//
// In dry-run mode nothing is mutated — only statistics are reported.
func RunGC(ctx context.Context, pgCl *cs.PG, s3c *cs.S3Client, bucket string, opts GCOptions) (GCStats, error) {
	var stats GCStats
	db := pgCl.Get()
	if db == nil {
		return stats, errors.New("db is nil")
	}
	if bucket == "" {
		return stats, errors.New("s3 bucket is not configured")
	}
	cutoff := time.Now().Add(-opts.Grace)

	var orphans []File
	// LEFT JOIN + IS NULL gives files with no matching resource_file row.
	// Oldest first so the backlog drains deterministically across runs.
	err := db.Model(&orphans).
		Context(ctx).
		ColumnExpr(`"file".*`).
		Join(`LEFT JOIN resource_file AS rf ON rf.file_hash = "file".hash`).
		Where(`rf.resource_id IS NULL`).
		Where(`"file".updated_at < ?`, cutoff).
		OrderExpr(`"file".updated_at ASC`).
		Limit(opts.Limit).
		Select()
	if err != nil {
		return stats, errors.Wrap(err, "failed to list orphan files")
	}
	stats.Candidates = len(orphans)
	log.WithFields(log.Fields{
		"candidates": stats.Candidates,
		"grace":      opts.Grace,
		"limit":      opts.Limit,
		"dry_run":    opts.DryRun,
	}).Info("gc: starting sweep")

	if opts.DryRun {
		var bytes int64
		for _, f := range orphans {
			bytes += f.TotalSize
			log.WithFields(log.Fields{
				"hash":       f.Hash,
				"status":     f.Status.String(),
				"total_size": f.TotalSize,
				"upload_id":  f.UploadID,
				"updated_at": f.UpdatedAt,
			}).Info("gc: would delete orphan")
		}
		stats.BytesFreed = bytes
		log.WithFields(log.Fields{
			"candidates":  stats.Candidates,
			"bytes_freed": stats.BytesFreed,
		}).Info("gc: dry-run complete")
		return stats, nil
	}

	s3Cl := s3c.Get()
	for i := range orphans {
		f := &orphans[i]
		// Re-check the reference count right before deletion — a worker
		// may have linked this file in the window between the select and
		// now. If so, skip.
		cnt, err := db.Model((*ResourceFile)(nil)).
			Context(ctx).
			Where("file_hash = ?", f.Hash).
			Count()
		if err != nil {
			log.WithError(err).WithField("hash", f.Hash).Warn("gc: re-check reference count failed; skipping")
			stats.Failed++
			continue
		}
		if cnt > 0 {
			log.WithField("hash", f.Hash).Info("gc: file re-linked during sweep; skipping")
			continue
		}

		if f.UploadID != "" {
			if _, err := s3Cl.AbortMultipartUploadWithContext(ctx, &awss3.AbortMultipartUploadInput{
				Bucket:   aws.String(bucket),
				Key:      aws.String(f.Hash),
				UploadId: aws.String(f.UploadID),
			}); err != nil {
				log.WithError(err).WithFields(log.Fields{
					"hash":      f.Hash,
					"upload_id": f.UploadID,
				}).Warn("gc: failed to abort multipart upload; continuing with DeleteObject")
			}
		}
		if _, err := s3Cl.DeleteObjectWithContext(ctx, &awss3.DeleteObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(f.Hash),
		}); err != nil {
			log.WithError(err).WithField("hash", f.Hash).Warn("gc: failed to delete S3 object; will retry on next sweep")
			stats.Failed++
			continue
		}
		if _, err := db.Model(f).Context(ctx).WherePK().Delete(); err != nil && !errors.Is(err, pg.ErrNoRows) {
			log.WithError(err).WithField("hash", f.Hash).Warn("gc: failed to delete file row; S3 object is gone, DB row may linger")
			stats.Failed++
			continue
		}
		stats.Deleted++
		stats.BytesFreed += f.TotalSize
		log.WithFields(log.Fields{
			"hash":       f.Hash,
			"total_size": f.TotalSize,
			"status":     f.Status.String(),
		}).Info("gc: removed orphan file")
	}

	log.WithFields(log.Fields{
		"candidates":  stats.Candidates,
		"deleted":     stats.Deleted,
		"failed":      stats.Failed,
		"bytes_freed": stats.BytesFreed,
	}).Info("gc: sweep complete")
	return stats, nil
}
