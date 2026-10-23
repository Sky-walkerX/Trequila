package bus

import (
	"context"
	"log/slog"
	"time"
)

// maxSuperviseDelay caps the wait between attempts to (re)start a subscription.
const maxSuperviseDelay = 30 * time.Second

// Supervise keeps s subscribed until ctx is cancelled. A subscription that cannot start,
// or that ends while ctx is still live (a broker restart, a closed iterator), is retried
// with a doubling delay starting at minDelay; a subscription that stays dead would
// silently stop one stage of the pipeline.
func Supervise(ctx context.Context, b Bus, s Subscription, log *slog.Logger, minDelay time.Duration) {
	delay := minDelay
	for {
		start := time.Now()
		err := b.Subscribe(ctx, s)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > maxSuperviseDelay {
			delay = minDelay // it ran for a while, so this is a fresh failure
		}
		log.Warn("subscription ended; restarting", "durable", s.Durable, "error", err, "retry_in", delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if delay *= 2; delay > maxSuperviseDelay {
			delay = maxSuperviseDelay
		}
	}
}
