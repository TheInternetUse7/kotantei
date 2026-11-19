// Package delivery drains the persistent outbox through a context-aware sender.
package delivery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"strconv"
	"sync"
	"time"

	"kotantei/internal/store"
)

// Send must respect context cancellation. Success returns a Discord message ID;
// retryable/permanent upstream failures may be classified with Failure.
type Sender interface {
	Send(context.Context, store.Delivery) (string, error)
}
type Failure struct {
	Reason     store.Reason
	RetryAfter time.Duration
}

func (f *Failure) Error() string { return "delivery transport failed" }

type Options struct {
	Workers       int
	LeaseDuration time.Duration
	SendTimeout   time.Duration
	StoreTimeout  time.Duration
	IdleDelay     time.Duration
	RetryMin      time.Duration
	RetryMax      time.Duration
	Now           func() time.Time
	Jitter        func() float64
	Logger        *slog.Logger
}
type Worker struct {
	queue   *store.Outbox
	sender  Sender
	options Options
}

func New(q *store.Outbox, sender Sender, options Options) (*Worker, error) {
	if q == nil || sender == nil {
		return nil, errors.New("delivery worker requires an outbox and sender")
	}
	if options.Workers == 0 {
		options.Workers = 1
	}
	if options.LeaseDuration == 0 {
		options.LeaseDuration = 30 * time.Second
	}
	if options.SendTimeout == 0 {
		options.SendTimeout = 15 * time.Second
	}
	if options.StoreTimeout == 0 {
		options.StoreTimeout = 5 * time.Second
	}
	if options.IdleDelay == 0 {
		options.IdleDelay = time.Second
	}
	if options.RetryMin == 0 {
		options.RetryMin = 5 * time.Second
	}
	if options.RetryMax == 0 {
		options.RetryMax = 5 * time.Minute
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Jitter == nil {
		options.Jitter = rand.Float64
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	if options.Workers < 1 || options.Workers > 16 || options.SendTimeout <= 0 || options.StoreTimeout <= 0 || options.IdleDelay <= 0 || options.RetryMin <= 0 || options.RetryMax < options.RetryMin || options.LeaseDuration > 24*time.Hour || options.SendTimeout >= options.LeaseDuration-options.StoreTimeout {
		return nil, errors.New("unsafe delivery worker limits/deadlines")
	}
	return &Worker{queue: q, sender: sender, options: options}, nil
}

// ProcessNext loads only one payload. Network I/O happens outside SQLite's
// transaction. Acknowledgment/lease cleanup gets a short independent deadline
// so a received success is not lost just because shutdown was requested.
func (w *Worker) ProcessNext(ctx context.Context) (bool, error) {
	delivery, err := w.queue.Claim(ctx, w.options.Now(), w.options.LeaseDuration)
	if err != nil {
		return false, err
	}
	if delivery == nil {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		cleanup, cancel := w.cleanupContext(ctx)
		defer cancel()
		if releaseErr := w.queue.Release(cleanup, delivery.Ref()); releaseErr != nil && !errors.Is(releaseErr, store.ErrLeaseLost) {
			return true, releaseErr
		}
		return true, err
	}
	sendContext, cancelSend := context.WithTimeout(ctx, w.options.SendTimeout)
	messageID, sendErr := w.sender.Send(sendContext, *delivery)
	timedOut := errors.Is(sendContext.Err(), context.DeadlineExceeded)
	cancelSend()
	cleanup, cancelCleanup := w.cleanupContext(ctx)
	defer cancelCleanup()
	now := w.options.Now()
	if sendErr == nil && validMessageReceipt(messageID) {
		err = w.queue.Acknowledge(cleanup, delivery.Ref(), messageID, now)
	} else if ctx.Err() != nil {
		err = w.queue.Release(cleanup, delivery.Ref())
	} else {
		reason := store.SendFailed
		var retryAfter time.Duration
		var failure *Failure
		if errors.As(sendErr, &failure) {
			retryAfter = failure.RetryAfter
			switch failure.Reason {
			case store.MissingPermissions, store.UnknownChannel, store.InvalidMessage, store.SendTimeout:
				reason = failure.Reason
			}
		}
		if reason == store.SendFailed && (timedOut || errors.Is(sendErr, context.DeadlineExceeded)) {
			reason = store.SendTimeout
		}
		switch reason {
		case store.MissingPermissions, store.UnknownChannel:
			err = w.queue.BlockChannel(cleanup, delivery.Ref(), reason, now)
		case store.InvalidMessage:
			err = w.queue.Fail(cleanup, delivery.Ref(), reason)
		default:
			err = w.queue.Retry(cleanup, delivery.Ref(), now.Add(w.retryDelay(delivery.Attempts, retryAfter)), reason)
		}
		// Upstream error bodies may contain request details; never log or
		// persist their raw text. The failure code is enough for recovery.
		w.options.Logger.Warn("delivery failed", "delivery_id", delivery.ID, "channel_id", delivery.ChannelID, "reason", reason)
	}
	if errors.Is(err, store.ErrLeaseLost) {
		w.options.Logger.Warn("delivery lease changed before outcome was recorded", "delivery_id", delivery.ID, "channel_id", delivery.ChannelID)
		err = nil
	}
	if err != nil {
		return true, fmt.Errorf("persist delivery outcome: %w", err)
	}
	return true, ctx.Err()
}

func (w *Worker) cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), w.options.StoreTimeout)
}

func (w *Worker) Run(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, w.options.Workers)
	var group sync.WaitGroup
	for range w.options.Workers {
		group.Go(func() { results <- w.loop(runContext); cancel() })
	}
	group.Wait()
	close(results)
	for err := range results {
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return runContext.Err()
}

func (w *Worker) loop(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		processed, err := w.ProcessNext(ctx)
		if err != nil {
			return err
		}
		if processed {
			continue
		}
		timer := time.NewTimer(w.options.IdleDelay)
		select {
		case <-ctx.Done():
		case <-w.queue.Changed():
		case <-timer.C:
		}
		timer.Stop()
	}
}

// Backoff doubles until its cap, applies +/-20% jitter, and never shortens an
// upstream retry-after. The loop is bounded even for a very large attempt count.
func (w *Worker) retryDelay(attempts int64, retryAfter time.Duration) time.Duration {
	delay := w.options.RetryMin
	for n := int64(1); n < attempts && delay < w.options.RetryMax; n++ {
		if delay > w.options.RetryMax/2 {
			delay = w.options.RetryMax
		} else {
			delay *= 2
		}
	}
	jitter := w.options.Jitter()
	if math.IsNaN(jitter) || jitter < 0 || jitter > 1 {
		jitter = .5
	}
	delay = time.Duration(float64(delay) * (.8 + .4*jitter))
	if delay > w.options.RetryMax {
		delay = w.options.RetryMax
	}
	if delay < time.Microsecond {
		delay = time.Microsecond
	}
	if retryAfter > delay {
		delay = retryAfter
	}
	return delay
}

func validMessageReceipt(id string) bool {
	parsed, err := strconv.ParseUint(id, 10, 64)
	return err == nil && parsed > 0 && strconv.FormatUint(parsed, 10) == id
}
