package main

import (
	"context"
	"net/http"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"

	cs "github.com/webtor-io/common-services"
	"github.com/webtor-io/vault/services"
)

func configureGC(c *cli.Command) {
	c.Flags = cs.RegisterPGFlags(c.Flags)
	c.Flags = cs.RegisterS3ClientFlags(c.Flags)
	c.Flags = services.RegisterGCFlags(c.Flags)
}

func makeGCCMD() cli.Command {
	gcCmd := cli.Command{
		Name:    "gc",
		Usage:   "Sweep orphan files (no resource_file reference) from S3 and DB",
		Action:  gc,
	}
	configureGC(&gcCmd)
	return gcCmd
}

func gc(c *cli.Context) error {
	ctx := context.Background()

	pg := cs.NewPG(c)
	if pg == nil {
		return errors.New("pg is not configured")
	}
	defer pg.Close()

	if err := cs.NewPGMigration(pg).Run(); err != nil {
		return errors.Wrap(err, "failed to run migrations")
	}

	cl := &http.Client{Timeout: 5 * time.Minute}
	s3c := cs.NewS3Client(c, cl)
	if s3c == nil {
		return errors.New("s3 client is not configured")
	}

	bucket := c.String("aws-bucket")
	opts := services.GCOptionsFromContext(c)

	stats, err := services.RunGC(ctx, pg, s3c, bucket, opts)
	if err != nil {
		return err
	}
	log.WithFields(log.Fields{
		"candidates":  stats.Candidates,
		"deleted":     stats.Deleted,
		"failed":      stats.Failed,
		"bytes_freed": stats.BytesFreed,
		"dry_run":     opts.DryRun,
	}).Info("gc finished")
	if stats.Failed > 0 {
		return errors.Errorf("%d gc operations failed; see logs", stats.Failed)
	}
	return nil
}
