package gputelemetry

import (
	"context"
	"errors"
	"time"
)

// SourceUnavailableError means initialization/discovery failed and an explicit
// unavailable record reached every local exporter. Retrying cannot hide disk errors.
type SourceUnavailableError struct{ Err error }

func (e *SourceUnavailableError) Error() string { return e.Err.Error() }
func (e *SourceUnavailableError) Unwrap() error { return e.Err }

// RunContinuous collects consecutive bounded summary windows until cancellation.
// It reopens the source at each boundary to rediscover devices and recover driver
// handles. Initialization failures retry with exponential backoff (1s to 30s).
// Exporters persist across windows and remain owned by the caller. Local output
// failures stop collection. As with Run, native driver calls must return.
func (c *Collector) RunContinuous(ctx context.Context, window time.Duration) error {
	return c.runContinuous(ctx, window, time.Second, 30*time.Second)
}

func (c *Collector) runContinuous(ctx context.Context, window, retry, maxRetry time.Duration) error {
	if window <= 0 {
		return errors.New("continuous telemetry window must be positive")
	}
	if c.FinalSampled != nil {
		// Each window is a Run with a final sample of its own, so the one
		// channel would be closed once per window.
		return errors.New("FinalSampled marks the end of a finite capture; continuous telemetry has no single final sample")
	}
	backoff := retry
	for ctx.Err() == nil {
		windowCtx, cancel := context.WithTimeout(ctx, window)
		err := c.Run(windowCtx)
		cancel()
		if err == nil {
			backoff = retry
			continue
		}
		if _, ok := errors.AsType[*SourceUnavailableError](err); !ok {
			return err
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		backoff = min(backoff*2, maxRetry)
	}
	return nil
}
