package main

import (
	"github.com/urfave/cli"
)

func configure(app *cli.App) {
	serveCmd := makeServeCMD()
	gcCmd := makeGCCMD()
	verifyCmd := makeVerifyExistingCMD()
	app.Commands = []cli.Command{serveCmd, gcCmd, verifyCmd}
}
