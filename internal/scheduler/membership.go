package scheduler

import (
	"context"
	"errors"
	"time"

	"kotantei/internal/store"
)

type ReconcileOptions struct {
	PageSize                           int
	Interval, RequestInterval, Timeout time.Duration
}
type ReconcileResult struct{ Checked, Removed, Failures int }
type Reconciler struct {
	queue   *store.Outbox
	check   func(context.Context, string, string) (bool, error)
	options ReconcileOptions
	gate    chan struct{}
}

func NewReconciler(queue *store.Outbox, check func(context.Context, string, string) (bool, error), options ReconcileOptions) (*Reconciler, error) {
	if queue == nil || check == nil {
		return nil, errors.New("membership reconciliation requires storage and a member checker")
	}
	if options.PageSize == 0 {
		options.PageSize = 64
	}
	if options.Interval == 0 {
		options.Interval = time.Hour
	}
	if options.RequestInterval == 0 {
		options.RequestInterval = time.Second
	}
	if options.Timeout == 0 {
		options.Timeout = 10 * time.Second
	}
	if options.PageSize < 1 || options.PageSize > 1000 || options.Interval <= 0 || options.RequestInterval <= 0 || options.Timeout <= 0 {
		return nil, errors.New("invalid membership reconciliation bounds")
	}
	return &Reconciler{queue: queue, check: check, options: options, gate: make(chan struct{}, 1)}, nil
}

// Cycle checks only tracked users, one public guild membership at a time. A
// failed/blocked check preserves the subscription; only a positive absence is
// applied, conditionally against that membership's captured generation.
func (r *Reconciler) Cycle(ctx context.Context) (ReconcileResult, error) {
	result := ReconcileResult{}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	select {
	case r.gate <- struct{}{}:
	default:
		return result, ErrBusy
	}
	defer func() { <-r.gate }()
	guild, user := "", ""
	var next time.Time
	for {
		members, err := r.queue.Memberships(ctx, guild, user, r.options.PageSize)
		if err != nil {
			return result, err
		}
		for _, member := range members {
			if delay := time.Until(next); delay > 0 {
				timer := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					timer.Stop()
					return result, ctx.Err()
				case <-timer.C:
				}
			}
			checkContext, cancel := context.WithTimeout(ctx, r.options.Timeout)
			present, checkErr := r.check(checkContext, member.GuildID, member.UserID)
			if checkErr == nil {
				checkErr = checkContext.Err()
			}
			cancel()
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			next = time.Now().Add(r.options.RequestInterval)
			result.Checked++
			if checkErr != nil {
				result.Failures++
			} else if !present {
				removed, err := r.queue.DisconnectIfCurrent(ctx, member)
				if err != nil {
					return result, err
				}
				if removed {
					result.Removed++
				}
			}
			guild, user = member.GuildID, member.UserID
		}
		if len(members) < r.options.PageSize {
			return result, nil
		}
	}
}

// Run waits for the next interval; callers perform the initial Cycle after
// Discord readiness and before starting providers/delivery.
func (r *Reconciler) Run(ctx context.Context) error {
	for {
		timer := time.NewTimer(r.options.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if _, err := r.Cycle(ctx); err != nil && !errors.Is(err, ErrBusy) {
			return err
		}
	}
}
