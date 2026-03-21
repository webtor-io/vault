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
// @Description  Redirects to a presigned S3 URL for the stored file. Returns 404 if resource is not fully stored or file not found.
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
	if res == nil || res.Status != StatusStored {
		c.Status(http.StatusNotFound)
		return
	}

	if p == "" || p == "/" {
		c.Status(http.StatusOK)
		return
	}

	hash, ok, err := s.lookupFileHash(c.Request.Context(), db, id, p)
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
	} else {
		presignedURL, err := s.presignGetObject(hash)
		if err != nil {
			_ = c.Error(err)
			return
		}
		c.Redirect(http.StatusFound, presignedURL)
	}
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
	if out.ContentType != nil {
		c.Header("Content-Type", *out.ContentType)
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

func (s *Web) lookupFileHash(ctx context.Context, db *pg.DB, id, path string) (string, bool, error) {
	rf := &ResourceFile{ResourceID: id, Path: path}
	if err := db.Model(rf).Context(ctx).Where("resource_id = ? and path = ?", id, path).Select(); err != nil {
		if errors.Is(err, pg.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
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
