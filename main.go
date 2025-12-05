package main

import (
	"os"

	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

func main() {
	app := cli.NewApp()
	app.Name = "vault"
	app.Usage = "runs Vault service"
	app.Version = "0.1.0"

	log.SetFormatter(&log.TextFormatter{FullTimestamp: true})

	configure(app)

	if err := app.Run(os.Args); err != nil {
		log.WithError(err).Fatal("failed to serve application")
	}
}
