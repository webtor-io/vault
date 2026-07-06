package services

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	awss3 "github.com/aws/aws-sdk-go/service/s3"
	"github.com/gin-gonic/gin"
	"github.com/go-pg/pg/v10"
	"github.com/pkg/errors"
)

const presignTTL = 1 * time.Hour

// WebSeed handler — GET/HEAD /webseed/{id}/{path}
// @Summary      Webseed proxy
// @Description  Redirects to a presigned S3 URL for the stored file. Root path requires the whole resource to be stored; per-file paths are served as soon as the individual file is stored, even if the resource as a whole is still uploading.
// @Tags         webseed
// @Param        id    path      string  true  "Resource ID"
// @Param        path  path      string  true  "Path inside resource"
// @Success      302
// @Failure      404  {object}  ErrorResponse
// @Failure      500  {object}  ErrorResponse
// @Router       /webseed/{id}/{path} [get]
// @Router       /webseed/{id}/{path} [head]
func (s *Web) webSeed(c *gin.Context) {
	if !s.validateWebSeedDependencies(c) {
		return
	}
	id := c.Param("id")
	p := c.Param("path")

	db := s.pg.Get()
	res, err := ResourceGetByID(c.Request.Context(), db, id)

	if err != nil {
		_ = c.Error(err)
		return
	}
	if res == nil {
		c.Status(http.StatusNotFound)
		return
	}

	// Root: signals "whole torrent is in vault" — used by clients to
	// short-circuit availability checks. Stays gated on resource status.
	if p == "" || p == "/" {
		if res.Status != StatusStored {
			c.Status(http.StatusNotFound)
			return
		}
		c.Status(http.StatusOK)
		return
	}

	// Per-file: serve as soon as this specific file finished uploading,
	// independent of the resource's aggregate status. Lets partially
	// uploaded torrents (e.g. a TV series mid-store) be streamed for
	// already-stored episodes.
	hash, ok, err := s.lookupStoredFileHash(c.Request.Context(), db, id, p)
	if err != nil {
		_ = c.Error(err)
		return
	}
	if !ok {
		c.Status(http.StatusNotFound)
		return
	}

	if c.Request.Method == http.MethodHead {
		s.handleHeadRequest(c, hash)
		return
	}

	// When s3-cache is wired in, route through it: stable URL (no presigned signature),
	// multi-range + chunk caching handled there. Falls back to direct presigned S3
	// when the env-flag is empty — keeps rollback a single env unset.
	//
	// s3-cache is single-tenant — the bucket is fixed on its side via AWS_BUCKET,
	// so the URL carries only the object key. Both deploys must point at the same
	// bucket or this silently serves wrong data.
	if s.s3CacheURL != "" {
		c.Redirect(http.StatusFound, s.s3CacheURL+"/"+hash)
		return
	}

	presignedURL, err := s.presignGetObject(hash)
	if err != nil {
		_ = c.Error(err)
		return
	}
	c.Redirect(http.StatusFound, presignedURL)
}

func (s *Web) handleHeadRequest(c *gin.Context, hash string) {
	s3cl := s.s3.Get()
	out, err := s3cl.HeadObjectWithContext(c.Request.Context(), &awss3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(hash),
	})
	if err != nil {
		_ = c.Error(err)
		return
	}
	if out.ContentLength != nil {
		c.Header("Content-Length", fmt.Sprintf("%d", *out.ContentLength))
	}
	// Content-Type is normalized and ETag/Last-Modified are exposed to match
	// what s3-cache emits on GET: players key stream-resume on these
	// validators, and vault uploads carry S3's default "binary/octet-stream"
	// which is not a registered MIME type.
	ct := "application/octet-stream"
	if out.ContentType != nil && *out.ContentType != "" && *out.ContentType != "binary/octet-stream" {
		ct = *out.ContentType
	}
	c.Header("Content-Type", ct)
	if out.ETag != nil {
		c.Header("ETag", *out.ETag)
	}
	if out.LastModified != nil {
		c.Header("Last-Modified", out.LastModified.UTC().Format(http.TimeFormat))
	}
	c.Header("Accept-Ranges", "bytes")
	c.Status(http.StatusOK)
}

func (s *Web) validateWebSeedDependencies(c *gin.Context) bool {
	if s.pg.Get() == nil {
		_ = c.Error(errors.New("DB not configured"))
		return false
	}
	if s.s3 == nil {
		_ = c.Error(errors.New("S3 not configured"))
		return false
	}
	if s.bucket == "" {
		_ = c.Error(errors.New("aws-bucket is not configured"))
		return false
	}
	return true
}

func (s *Web) lookupStoredFileHash(ctx context.Context, db *pg.DB, id, path string) (string, bool, error) {
	rf := &ResourceFile{}
	err := db.Model(rf).
		Context(ctx).
		Relation("File").
		Where("resource_file.resource_id = ?", id).
		Where("resource_file.path = ?", path).
		Select()
	if err != nil {
		if errors.Is(err, pg.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
	}
	if rf.File == nil || rf.File.Status != StatusStored {
		return "", false, nil
	}
	return rf.FileHash, true, nil
}

func (s *Web) presignGetObject(hash string) (string, error) {
	s3cl := s.s3.Get()
	req, _ := s3cl.GetObjectRequest(&awss3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(hash),
	})
	return req.Presign(presignTTL)
}
