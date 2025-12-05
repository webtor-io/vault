package services

import (
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
	"github.com/urfave/cli"
	cs "github.com/webtor-io/common-services"
	"github.com/webtor-io/vault/docs"
)

// @title           Vault API
// @version         0.1
// @description     API to communicate with Vault service.

// @contact.name   Webtor Support
// @contact.url    https://webtor.io/support
// @contact.email  support@webtor.io

const (
	webHostFlag = "host"
	webPortFlag = "port"
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
}

func NewWeb(c *cli.Context, pg *cs.PG, s3 *cs.S3Client) *Web {
	return &Web{
		host:   c.String(webHostFlag),
		port:   c.Int(webPortFlag),
		pg:     pg,
		s3:     s3,
		bucket: c.String("aws-bucket"),
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
	err := c.Errors[0]
	log.Error(err)

	status := http.StatusInternalServerError

	if strings.Contains(err.Error(), "failed to parse") {
		status = http.StatusBadRequest
	} else if strings.Contains(err.Error(), "forbidden") {
		status = http.StatusForbidden
	} else if strings.Contains(err.Error(), "not found") {
		status = http.StatusNotFound
	} else if strings.Contains(err.Error(), "timeout") {
		status = http.StatusRequestTimeout
	}
	c.PureJSON(status, &ErrorResponse{Error: err.Error()})
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
