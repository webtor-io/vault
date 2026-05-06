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

func configureVerifyExisting(c *cli.Command) {
	c.Flags = cs.RegisterPGFlags(c.Flags)
	c.Flags = cs.RegisterS3ClientFlags(c.Flags)
	c.Flags = services.RegisterApiFlags(c.Flags)
	c.Flags = services.RegisterVerifyExistingFlags(c.Flags)
}

func makeVerifyExistingCMD() cli.Command {
	cmd := cli.Command{
		Name:   "verify-existing",
		Usage:  "Verify already-stored resources against torrent piece hashes; invalidate corrupt files and re-queue owning resources for re-store",
		Action: verifyExisting,
	}
	configureVerifyExisting(&cmd)
	return cmd
}

func verifyExisting(c *cli.Context) error {
	ctx := context.Background()

	pg := cs.NewPG(c)
	if pg == nil {
		return errors.New("pg is not configured")
	}
	defer pg.Close()
	if err := cs.NewPGMigration(pg).Run(); err != nil {
		return errors.Wrap(err, "failed to run migrations")
	}

	cl := &http.Client{Timeout: 30 * time.Minute}
	s3c := cs.NewS3Client(c, cl)
	if s3c == nil {
		return errors.New("s3 client is not configured")
	}
	api := services.NewApi(c, cl)

	bucket := c.String("aws-bucket")
	opts := services.VerifyExistingOptionsFromContext(c)
	stats, err := services.RunVerifyExisting(ctx, pg, s3c, api, bucket, opts)
	if err != nil {
		return err
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
		"dry_run":       opts.DryRun,
	}).Info("verify-existing finished")
	if stats.Errors > 0 {
		return errors.Errorf("%d verification errors; see logs", stats.Errors)
	}
	return nil
}
