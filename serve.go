package main

import (
	"net/http"

	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"

	cs "github.com/webtor-io/common-services"
	"github.com/webtor-io/vault/services"
)

func configureServe(c *cli.Command) {
	c.Flags = cs.RegisterProbeFlags(c.Flags)
	c.Flags = cs.RegisterPprofFlags(c.Flags)
	c.Flags = cs.RegisterPGFlags(c.Flags)
	c.Flags = cs.RegisterS3ClientFlags(c.Flags)
	c.Flags = services.RegisterWebFlags(c.Flags)
	c.Flags = services.RegisterWorkerFlags(c.Flags)
	c.Flags = services.RegisterApiFlags(c.Flags)
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

	cl := http.DefaultClient

	// Setting S3Client
	s3c := cs.NewS3Client(c, cl)

	// Setting Web
	web := services.NewWeb(c, pg, s3c)
	svcs = append(svcs, web)
	defer web.Close()

	// Setting Webtor Rest API
	api := services.NewApi(c, cl)

	// Setting Worker
	worker := services.NewWorker(c, pg, s3c, api)
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
