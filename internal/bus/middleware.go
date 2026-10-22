package bus

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/sky-walkerx/trequila/internal/metrics"
)

// Middleware wraps a Handler.
type Middleware func(Handler) Handler

// Chain applies middleware so that the first one is outermost.
func Chain(h Handler, mw ...Middleware) Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

// Recover turns a panic into a retryable error and logs the stack.
func Recover(log *slog.Logger) Middleware {
	return func(next Handler) Handler {
		return func(ctx context.Context, e Event) (err error) {
			defer func() {
				if r := recover(); r != nil {
					log.Error("handler panicked", "event_id", e.ID, "type", e.Type,
						"panic", fmt.Sprint(r), "stack", string(debug.Stack()))
					err = fmt.Errorf("handler panic: %v", r)
				}
			}()
			return next(ctx, e)
		}
	}
}

// Logging records each delivery: failures at warn (permanent ones say so), the
// rest at debug.
func Logging(log *slog.Logger, name string) Middleware {
	return func(next Handler) Handler {
		return func(ctx context.Context, e Event) error {
			start := time.Now()
			err := next(ctx, e)
			attrs := []any{"subscription", name, "event_id", e.ID, "type", e.Type, "duration", time.Since(start)}
			switch {
			case err == nil:
				log.Debug("event handled", attrs...)
			case IsPermanent(err):
				log.Warn("event dropped", append(attrs, "permanent", true, "error", err)...)
			default:
				log.Warn("event failed", append(attrs, "error", err)...)
			}
			return err
		}
	}
}

// Metrics counts and times deliveries per subscription.
func Metrics(name string) Middleware {
	return func(next Handler) Handler {
		return func(ctx context.Context, e Event) error {
			start := time.Now()
			err := next(ctx, e)
			result := "ok"
			switch {
			case err == nil:
			case IsPermanent(err):
				result = "permanent"
			default:
				result = "retry"
			}
			metrics.EventsHandledTotal.WithLabelValues(name, result).Inc()
			metrics.EventHandleDuration.WithLabelValues(name).Observe(time.Since(start).Seconds())
			return err
		}
	}
}
