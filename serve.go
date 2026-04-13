package main

import (
	"net/http"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"

	cs "github.com/webtor-io/common-services"
	"github.com/webtor-io/vault/services"
)

func configureServe(c *cli.Command) {
	c.Flags = cs.RegisterProbeFlags(c.Flags)
	c.Flags = cs.RegisterPprofFlags(c.Flags)
	c.Flags = cs.RegisterPromFlags(c.Flags)
	c.Flags = cs.RegisterPGFlags(c.Flags)
	c.Flags = cs.RegisterS3ClientFlags(c.Flags)
	c.Flags = services.RegisterWebFlags(c.Flags)
	c.Flags = services.RegisterWorkerFlags(c.Flags)
	c.Flags = services.RegisterApiFlags(c.Flags)
	c.Flags = services.RegisterMetricsFlags(c.Flags)
	c.Flags = cs.RegisterNATSFlags(c.Flags)
}

func makeServeCMD() cli.Command {
	serveCmd := cli.Command{
		Name:    "serve",
		Aliases: []string{"s"},
		Usage:   "Serves web server",
		Action:  serve,
	}
	configureServe(&serveCmd)
	return serveCmd
}

func serve(c *cli.Context) (err error) {
	// Setting DB
	pg := cs.NewPG(c)
	if pg != nil {
		defer pg.Close()
	}

	// Setting Migrations
	m := cs.NewPGMigration(pg)
	err = m.Run()
	if err != nil {
		return err
	}

	var svcs []cs.Servable

	// Setting Probe
	probe := cs.NewProbe(c)
	if probe != nil {
		svcs = append(svcs, probe)
		defer probe.Close()
	}

	// Setting PPROF
	pprof := cs.NewPprof(c)
	if pprof != nil {
		svcs = append(svcs, pprof)
		defer pprof.Close()
	}

	// Setting Prometheus exporter (/metrics on :8083 by default)
	prom := cs.NewProm(c)
	if prom != nil {
		svcs = append(svcs, prom)
		defer prom.Close()
	}

	// Setting vault metrics refresher — populates gauges consumed by prom
	metrics := services.NewMetrics(c, pg)
	svcs = append(svcs, metrics)
	defer metrics.Close()

	cl := &http.Client{
		Timeout: 30 * time.Minute,
	}

	// Setting S3Client
	s3c := cs.NewS3Client(c, cl)

	// Setting Web
	web := services.NewWeb(c, pg, s3c)
	svcs = append(svcs, web)
	defer web.Close()

	// Setting Webtor Rest API
	api := services.NewApi(c, cl)

	// Setting NATS
	nt := cs.NewNATS(c)
	if nt != nil {
		defer nt.Close()
	}

	// Setting Worker
	worker := services.NewWorker(c, pg, s3c, api, nt)
	svcs = append(svcs, worker)
	defer worker.Close()

	// Setting Serve
	s := cs.NewServe(svcs...)

	// And SERVE!
	err = s.Serve()
	if err != nil {
		log.WithError(err).Error("got server error")
	}
	return
}
