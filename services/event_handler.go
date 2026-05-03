package services

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	cs "github.com/webtor-io/common-services"
)

const (
	eventStreamName        = "common"
	resourceBannedSubject  = "resource.banned"
	resourceBannedConsumer = "vault-resource-banned"
)

// EventHandler subscribes to platform-wide NATS events that vault should react
// to. Currently handles a single subject — `resource.banned`, published by
// abuse-store when a torrent is added to the illegal-content stoplist — by
// queuing the resource for deletion. The same `ResourceQueueForDeletion` flow
// the public DELETE endpoint uses is reused, so the worker picks the deletion
// up like any other.
//
// `ResourceQueueForDeletion` is idempotent (no-op if the resource is already
// queued/deleting, returns nil if it never existed), which matches the
// at-least-once delivery the publisher guarantees.
type EventHandler struct {
	pg   *cs.PG
	nats *cs.NATS
	subs []*nats.Subscription
	done chan struct{}
}

func NewEventHandler(pg *cs.PG, nt *cs.NATS) *EventHandler {
	if pg == nil || nt == nil {
		return nil
	}
	return &EventHandler{
		pg:   pg,
		nats: nt,
		done: make(chan struct{}),
	}
}

func (h *EventHandler) Serve() error {
	nc := h.nats.Get()
	if nc == nil {
		log.Warn("nats connection is nil, skipping event subscriptions")
		return nil
	}
	js, err := nc.JetStream()
	if err != nil {
		return errors.Wrap(err, "failed to get jetstream context")
	}
	if err := h.subscribe(js, resourceBannedSubject, resourceBannedConsumer, h.resourceBanned); err != nil {
		return err
	}

	<-h.done
	return nil
}

func (h *EventHandler) subscribe(js nats.JetStreamContext, subject, consumer string, handler func([]byte) error) error {
	sub, err := js.PullSubscribe(subject, consumer, nats.Bind(eventStreamName, consumer))
	if err != nil {
		return errors.Wrapf(err, "failed to subscribe to %s", subject)
	}
	h.subs = append(h.subs, sub)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.WithField("consumer", consumer).Errorf("panic in event handler: %v", r)
			}
		}()
		for {
			select {
			case <-h.done:
				return
			default:
				msgs, err := sub.Fetch(1, nats.MaxWait(5*time.Second))
				if err != nil {
					if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, nats.ErrTimeout) {
						continue
					}
					log.WithError(err).WithField("consumer", consumer).Error("failed to fetch message")
					continue
				}
				msg := msgs[0]
				if err := handler(msg.Data); err != nil {
					log.WithError(err).WithField("consumer", consumer).Error("failed to handle message")
					_ = msg.Nak()
				} else {
					_ = msg.Ack()
				}
			}
		}
	}()
	return nil
}

type resourceBannedMsg struct {
	Infohash string `json:"infohash"`
}

func (h *EventHandler) resourceBanned(data []byte) error {
	var m resourceBannedMsg
	if err := json.Unmarshal(data, &m); err != nil {
		return errors.Wrap(err, "failed to unmarshal resource.banned payload")
	}
	if m.Infohash == "" {
		log.Warn("resource.banned with empty infohash, skipping")
		return nil
	}
	db := h.pg.Get()
	if db == nil {
		return errors.New("database connection is not available")
	}
	ctx := context.Background()
	res, err := ResourceQueueForDeletion(ctx, db, m.Infohash)
	if err != nil {
		// Best-effort log of the deletion attempt, mirroring the HTTP handler.
		if aerr := APILogWrite(ctx, db, m.Infohash, OperationDelete, http.StatusInternalServerError); aerr != nil {
			log.WithError(aerr).WithField("resource_id", m.Infohash).Warn("failed to write api log")
		}
		return errors.Wrapf(err, "failed to queue resource for deletion infohash=%s", m.Infohash)
	}
	status := http.StatusAccepted
	if res == nil {
		// Resource was either never stored or sat in QueuedForStoring and was
		// dropped — nothing for the worker to do. Still log the attempt so the
		// audit trail matches the HTTP-driven path.
		status = http.StatusNotFound
	}
	if aerr := APILogWrite(ctx, db, m.Infohash, OperationDelete, int16(status)); aerr != nil {
		log.WithError(aerr).WithField("resource_id", m.Infohash).Warn("failed to write api log")
	}
	log.WithFields(log.Fields{"resource_id": m.Infohash, "status": status}).Info("processed resource.banned")
	return nil
}

func (h *EventHandler) Close() {
	for _, sub := range h.subs {
		_ = sub.Unsubscribe()
	}
	select {
	case <-h.done:
	default:
		close(h.done)
	}
}
