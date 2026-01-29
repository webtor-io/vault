package services

import (
	"encoding/json"
	"fmt"

	"github.com/nats-io/nats.go"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

const (
	natsServiceHostFlag = "nats-service-host"
	natsServicePortFlag = "nats-service-port"
)

func RegisterNATSFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.StringFlag{
			Name:   natsServiceHostFlag,
			Usage:  "nats service host",
			Value:  "",
			EnvVar: "NATS_SERVICE_HOST",
		},
		cli.IntFlag{
			Name:   natsServicePortFlag,
			Usage:  "nats service port",
			Value:  4222,
			EnvVar: "NATS_SERVICE_PORT",
		},
	)
}

type NATS struct {
	nc *nats.Conn
}

func NewNATS(c *cli.Context) *NATS {
	host := c.String(natsServiceHostFlag)
	if host == "" {
		return nil
	}
	port := c.Int(natsServicePortFlag)
	url := fmt.Sprintf("nats://%s:%d", host, port)
	nc, err := nats.Connect(url)
	if err != nil {
		log.WithError(err).Fatal("failed to connect to nats")
	}
	return &NATS{
		nc: nc,
	}
}

func (s *NATS) Publish(subject string, v interface{}) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.nc.Publish(subject, b)
}

func (s *NATS) Close() {
	if s.nc != nil {
		s.nc.Close()
	}
}
