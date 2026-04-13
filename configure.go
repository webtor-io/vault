package main

import (
	"github.com/urfave/cli"
)

func configure(app *cli.App) {
	serveCmd := makeServeCMD()
	gcCmd := makeGCCMD()
	app.Commands = []cli.Command{serveCmd, gcCmd}
}
