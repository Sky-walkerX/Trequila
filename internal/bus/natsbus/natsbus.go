// Package natsbus implements internal/bus on NATS JetStream.
//
// Events travel as JSON envelopes on the subject equal to Event.Type, inside the
// TRIPS stream. Publishing sets Nats-Msg-Id to the event ID so a re-published event
// is dropped by the stream's de-duplication window. Each subscription is a durable
// pull consumer with explicit acknowledgements.
package natsbus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sky-walkerx/trequila/internal/bus"
)

const (
	// StreamName is the JetStream stream every trip event lives in.
	StreamName = "TRIPS"

	defaultAckWait    = 30 * time.Second
	defaultMaxDeliver = 3
	defaultRetryDelay = 2 * time.Second
)

// Config describes how to reach NATS.
type Config struct {
	URL          string
	CredsFile    string // path to a .creds file (local development)
	CredsContent string // raw .creds content (hosted platforms that only offer env vars)
	RetryDelay   time.Duration
	Logger       *slog.Logger
}

// Bus is a bus.Bus backed by JetStream.
type Bus struct {
	nc    *nats.Conn
	js    jetstream.JetStream
	log   *slog.Logger
	retry time.Duration
}

var (
	_ bus.Bus    = (*Bus)(nil)
	_ bus.Pinger = (*Bus)(nil)
)

// Connect dials NATS and makes sure the TRIPS stream exists.
func Connect(ctx context.Context, cfg Config) (*Bus, error) {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	opts := []nats.Option{
		nats.Name("Trequila"),
		nats.ReconnectWait(2 * time.Second),
		nats.MaxReconnects(-1),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				log.Warn("nats disconnected", "error", err)
			}
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			log.Info("nats reconnected", "url", nc.ConnectedUrlRedacted())
		}),
	}
	switch {
	case cfg.CredsContent != "":
		opts = append(opts, nats.UserCredentialBytes([]byte(cfg.CredsContent)))
	case cfg.CredsFile != "":
		opts = append(opts, nats.UserCredentials(cfg.CredsFile))
	}

	nc, err := nats.Connect(cfg.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("natsbus: connect: %w", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("natsbus: jetstream: %w", err)
	}

	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := js.CreateOrUpdateStream(sctx, jetstream.StreamConfig{
		Name:       StreamName,
		Subjects:   []string{"trips.*"},
		Storage:    jetstream.FileStorage,
		Retention:  jetstream.LimitsPolicy,
		MaxMsgs:    1_000_000,
		MaxBytes:   1 << 30,
		MaxAge:     7 * 24 * time.Hour,
		Duplicates: 2 * time.Minute,
	}); err != nil {
		nc.Close()
		return nil, fmt.Errorf("natsbus: ensure stream %s: %w", StreamName, err)
	}

	retry := cfg.RetryDelay
	if retry <= 0 {
		retry = defaultRetryDelay
	}
	return &Bus{nc: nc, js: js, log: log, retry: retry}, nil
}

// Close drops the connection.
func (b *Bus) Close() { b.nc.Close() }

// Ping reports whether the connection is usable.
func (b *Bus) Ping(ctx context.Context) error {
	if !b.nc.IsConnected() {
		return errors.New("nats: not connected")
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
	}
	return b.nc.FlushWithContext(ctx)
}

// Publish sends e on the subject e.Type.
func (b *Bus) Publish(ctx context.Context, e bus.Event) error {
	if e.ID == "" || e.Type == "" {
		return errors.New("natsbus: event needs an ID and a type")
	}
	payload, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("natsbus: marshal event: %w", err)
	}
	if _, err := b.js.Publish(ctx, e.Type, payload, jetstream.WithMsgID(e.ID)); err != nil {
		return fmt.Errorf("natsbus: publish %s: %w", e.Type, err)
	}
	return nil
}

// Subscribe runs a durable pull consumer until ctx is cancelled.
func (b *Bus) Subscribe(ctx context.Context, s bus.Subscription) error {
	if s.Durable == "" || s.Type == "" || s.Handler == nil {
		return errors.New("natsbus: subscription needs a durable name, a type and a handler")
	}
	ackWait, maxDeliver := s.AckWait, s.MaxDeliver
	if ackWait <= 0 {
		ackWait = defaultAckWait
	}
	if maxDeliver <= 0 {
		maxDeliver = defaultMaxDeliver
	}

	stream, err := b.js.Stream(ctx, StreamName)
	if err != nil {
		return fmt.Errorf("natsbus: stream %s: %w", StreamName, err)
	}
	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       s.Durable,
		FilterSubject: s.Type,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       ackWait,
		MaxDeliver:    maxDeliver,
	})
	if err != nil {
		return fmt.Errorf("natsbus: consumer %s: %w", s.Durable, err)
	}
	b.log.Info("subscribed", "durable", s.Durable, "type", s.Type)

	it, err := cons.Messages(jetstream.PullMaxMessages(1))
	if err != nil {
		return fmt.Errorf("natsbus: consume %s: %w", s.Durable, err)
	}
	go func() {
		<-ctx.Done()
		it.Stop()
	}()

	for {
		msg, err := it.Next()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, jetstream.ErrMsgIteratorClosed) {
				return nil
			}
			b.log.Warn("receive failed", "durable", s.Durable, "error", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(time.Second):
			}
			continue
		}
		b.deliver(ctx, s, ackWait, msg)
	}
}

func (b *Bus) deliver(ctx context.Context, s bus.Subscription, ackWait time.Duration, msg jetstream.Msg) {
	var e bus.Event
	if err := json.Unmarshal(msg.Data(), &e); err != nil || e.ID == "" {
		// Not one of ours (for instance the pre-bus envelope): redelivery cannot fix it.
		b.log.Warn("skipping message without an event envelope", "durable", s.Durable, "subject", msg.Subject())
		msg.Ack()
		return
	}

	// Stop working just before the broker would redeliver, so a slow handler does not
	// overlap with its own retry.
	hctx, cancel := context.WithTimeout(ctx, ackWait-ackWait/10)
	err := s.Handler(hctx, e)
	cancel()

	switch {
	case err == nil:
		msg.Ack()
	case bus.IsPermanent(err):
		msg.Ack() // redelivery cannot fix it; the logging middleware has already recorded why
	default:
		msg.NakWithDelay(b.retry)
	}
}
