package services

import (
	stderrors "errors"
	"fmt"
	"net"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
	"github.com/urfave/cli"
	cs "github.com/webtor-io/common-services"
	"github.com/webtor-io/vault/docs"
)

// Sentinel errors for HTTP status code mapping.
var (
	ErrBadRequest = stderrors.New("bad request")
	ErrForbidden  = stderrors.New("forbidden")
	ErrNotFound   = stderrors.New("not found")
	ErrTimeout    = stderrors.New("timeout")
)

// @title           Vault API
// @version         0.1
// @description     API to communicate with Vault service.

// @contact.name   Webtor Support
// @contact.url    https://webtor.io/support
// @contact.email  support@webtor.io

const (
	webHostFlag     = "host"
	webPortFlag     = "port"
	s3CacheURLFlag  = "s3-cache-url"
)

func RegisterWebFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.StringFlag{
			Name:   webHostFlag,
			Usage:  "listening host",
			Value:  "",
			EnvVar: "WEB_HOST",
		},
		cli.IntFlag{
			Name:   webPortFlag,
			Usage:  "http listening port",
			Value:  8080,
			EnvVar: "WEB_PORT",
		},
		cli.StringFlag{
			Name: s3CacheURLFlag,
			Usage: "base URL of s3-cache (e.g. http://s3-cache:80); " +
				"when set, /webseed redirects clients here instead of presigned S3. " +
				"Empty disables — falls back to direct S3 presigned URL.",
			Value:  "",
			EnvVar: "S3_CACHE_URL",
		},
	)
}

type Web struct {
	host string
	port int
	ln   net.Listener
	pg   *cs.PG
	s3   *cs.S3Client
	// bucket to read objects from (same as worker's AWS_BUCKET)
	bucket string
	// s3CacheURL — when set, webseed redirects here instead of OVH directly.
	s3CacheURL string
}

func NewWeb(c *cli.Context, pg *cs.PG, s3 *cs.S3Client) *Web {
	return &Web{
		host:       c.String(webHostFlag),
		port:       c.Int(webPortFlag),
		pg:         pg,
		s3:         s3,
		bucket:     c.String("aws-bucket"),
		s3CacheURL: c.String(s3CacheURLFlag),
	}
}

func (s *Web) Serve() error {
	addr := fmt.Sprintf("%s:%d", s.host, s.port)
	ln, err := net.Listen("tcp", addr)
	s.ln = ln
	if err != nil {
		return errors.Wrap(err, "Failed to web listen to tcp connection")
	}
	r := gin.Default()
	r.UseRawPath = true
	r.Use(s.errorHandler)
	rg := r.Group("/resource")

	rg.PUT("/:id", s.putResource)
	rg.GET("/:id", s.getResource)
	rg.DELETE("/:id", s.deleteResource)
	// files listing endpoint is not needed per requirements

	// WebSeed: /webseed/{id}/{path}
	r.Any("/webseed/:id/*path", s.webSeed)

	// Swagger UI
	r.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler, ginSwagger.InstanceName("vault")))
	docs.SwaggerInfovault.BasePath = "/"

	log.Infof("serving Web at %v", addr)
	return http.Serve(s.ln, r)
}

func (s *Web) errorHandler(c *gin.Context) {
	c.Next()
	if len(c.Errors) == 0 {
		return
	}
	ginErr := c.Errors[0]
	log.Error(ginErr)

	status := http.StatusInternalServerError
	unwrapped := ginErr.Err

	switch {
	case stderrors.Is(unwrapped, ErrBadRequest):
		status = http.StatusBadRequest
	case stderrors.Is(unwrapped, ErrForbidden):
		status = http.StatusForbidden
	case stderrors.Is(unwrapped, ErrNotFound):
		status = http.StatusNotFound
	case stderrors.Is(unwrapped, ErrTimeout):
		status = http.StatusRequestTimeout
	}
	c.PureJSON(status, &ErrorResponse{Error: ginErr.Error()})
}

func (s *Web) Close() {
	log.Info("closing Web")
	defer func() {
		log.Info("Web closed")
	}()
	if s.ln != nil {
		_ = s.ln.Close()
	}
}
