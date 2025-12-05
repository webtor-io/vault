package services

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	awss3 "github.com/aws/aws-sdk-go/service/s3"
	"github.com/gin-gonic/gin"
	"github.com/go-pg/pg/v10"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

// WebSeed handler — GET/HEAD /webseed/{id}/{path}
// @Summary      Webseed proxy
// @Description  Proxies stored files from S3 with Range support. Returns 404 if resource is not fully stored or file not found.
// @Tags         webseed
// @Param        id    path      string  true  "Resource ID"
// @Param        path  path      string  true  "Path inside resource"
// @Produce      application/octet-stream
// @Success      200
// @Success      206
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

	rangeHeader := c.GetHeader("Range")
	if c.Request.Method == http.MethodHead {
		s.handleHeadRequest(c, hash, rangeHeader)
	} else {
		s.handleGetRequest(c, hash, rangeHeader, id, p)
	}
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

func (s *Web) handleHeadRequest(c *gin.Context, hash, rangeHeader string) {
	s3cl := s.s3.Get()
	input := &awss3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(hash),
		Range:  s.buildRangePointer(rangeHeader),
	}
	req, out := s3cl.HeadObjectRequest(input)
	if err := req.Send(); err != nil {
		if s.isS3NotFoundError(err) {
			c.Status(http.StatusNotFound)
			return
		}
		_ = c.Error(err)
		return
	}

	s.setHeadResponseHeaders(c, out)
	status := http.StatusOK
	if req.HTTPResponse != nil && req.HTTPResponse.StatusCode == http.StatusPartialContent {
		status = http.StatusPartialContent
	}
	c.Status(status)
}

func (s *Web) handleGetRequest(c *gin.Context, hash, rangeHeader, id, path string) {
	s3cl := s.s3.Get()
	out, err := s3cl.GetObjectWithContext(c.Request.Context(), &awss3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(hash),
		Range:  s.buildRangePointer(rangeHeader),
	})
	if err != nil {
		if s.isS3NotFoundError(err) {
			c.Status(http.StatusNotFound)
			return
		}
		_ = c.Error(err)
		return
	}
	defer func() { _ = out.Body.Close() }()

	s.setGetResponseHeaders(c, out)
	status := http.StatusOK
	if rangeHeader != "" && out.ContentRange != nil {
		status = http.StatusPartialContent
	}
	c.Status(status)

	if _, err := io.Copy(c.Writer, out.Body); err != nil {
		log.WithError(err).WithField("id", id).WithField("path", path).Warn("webseed stream error")
	}
}

func (s *Web) buildRangePointer(rangeHeader string) *string {
	if rangeHeader != "" {
		return aws.String(rangeHeader)
	}
	return nil
}

func (s *Web) isS3NotFoundError(err error) bool {
	return strings.Contains(err.Error(), awss3.ErrCodeNoSuchKey) ||
		strings.Contains(strings.ToLower(err.Error()), "not found")
}

func (s *Web) setHeadResponseHeaders(c *gin.Context, out *awss3.HeadObjectOutput) {
	if out.AcceptRanges != nil {
		c.Header("Accept-Ranges", *out.AcceptRanges)
	} else {
		c.Header("Accept-Ranges", "bytes")
	}
	if out.ContentType != nil {
		c.Header("Content-Type", *out.ContentType)
	}
	if out.ETag != nil {
		c.Header("ETag", *out.ETag)
	}
	if out.LastModified != nil {
		c.Header("Last-Modified", out.LastModified.UTC().Format(http.TimeFormat))
	}
	if out.ContentLength != nil {
		c.Header("Content-Length", fmt.Sprintf("%d", *out.ContentLength))
	}
}

func (s *Web) setGetResponseHeaders(c *gin.Context, out *awss3.GetObjectOutput) {
	if out.AcceptRanges != nil {
		c.Header("Accept-Ranges", *out.AcceptRanges)
	} else {
		c.Header("Accept-Ranges", "bytes")
	}
	if out.ContentType != nil {
		c.Header("Content-Type", *out.ContentType)
	} else {
		c.Header("Content-Type", "application/octet-stream")
	}
	if out.ETag != nil {
		c.Header("ETag", *out.ETag)
	}
	if out.LastModified != nil {
		c.Header("Last-Modified", out.LastModified.UTC().Format(http.TimeFormat))
	}
	if out.ContentLength != nil {
		c.Header("Content-Length", fmt.Sprintf("%d", *out.ContentLength))
	}
	if out.ContentRange != nil {
		c.Header("Content-Range", *out.ContentRange)
	}
}
