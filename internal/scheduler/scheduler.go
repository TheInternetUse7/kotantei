// Package scheduler runs bounded, independent provider cycles against SQLite.
package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"kotantei/internal/store"
)

var ErrBusy = errors.New("a provider cycle is already running")

type Counts struct{ Events, Deliveries, Warnings int }
type Provider struct {
	Name  string
	Delay time.Duration
	Check func(context.Context, int64) (Counts, error)
}
type Status struct {
	Name        string    `json:"name"`
	Running     bool      `json:"running"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	Checked     int       `json:"checked"`
	Failures    int       `json:"failures"`
	Events      int       `json:"events"`
	Deliveries  int       `json:"deliveries"`
	Warnings    int       `json:"warnings"`
	LastError   string    `json:"last_error,omitempty"`
}
type Options struct {
	PageSize  int
	Now       func() time.Time
	Logger    *slog.Logger
	ErrorCode func(error) string
}
type providerState struct {
	provider   Provider
	gate, wake chan struct{}
	status     Status
}
type Scheduler struct {
	queue     *store.Outbox
	options   Options
	providers map[string]*providerState
	names     []string
	mu        sync.Mutex
	runGate   chan struct{}
}

func New(queue *store.Outbox, providers []Provider, options Options) (*Scheduler, error) {
	if queue == nil || len(providers) == 0 || len(providers) > 2 {
		return nil, errors.New("scheduler requires an outbox and one or two providers")
	}
	if options.PageSize == 0 {
		options.PageSize = 64
	}
	if options.PageSize < 1 || options.PageSize > 1000 {
		return nil, errors.New("invalid scheduler page size")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	if options.ErrorCode == nil {
		options.ErrorCode = pollErrorCode
	}
	s := &Scheduler{queue: queue, options: options, providers: map[string]*providerState{}, runGate: make(chan struct{}, 1)}
	for _, provider := range providers {
		if (provider.Name != "anilist" && provider.Name != "mal") || provider.Delay <= 0 || provider.Check == nil || s.providers[provider.Name] != nil {
			return nil, errors.New("invalid or duplicate provider schedule")
		}
		s.names = append(s.names, provider.Name)
		s.providers[provider.Name] = &providerState{provider: provider, gate: make(chan struct{}, 1), wake: make(chan struct{}, 1), status: Status{Name: provider.Name}}
	}
	return s, nil
}

// Cycle captures an ID fence and streams small pages. New higher IDs wait for
// the next cycle, and accounts without any live subscriptions are never polled.
func (s *Scheduler) Cycle(ctx context.Context, name string) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	state := s.providers[name]
	if state == nil {
		return errors.New("unknown provider")
	}
	select {
	case state.gate <- struct{}{}:
	default:
		return ErrBusy
	}
	defer func() { <-state.gate }()
	s.mu.Lock()
	state.status = Status{Name: name, Running: true, StartedAt: s.options.Now().UTC()}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		state.status.Running = false
		state.status.CompletedAt = s.options.Now().UTC()
		if err != nil {
			state.status.LastError = s.options.ErrorCode(err)
		}
	}()
	fence, err := s.queue.AccountFence(ctx)
	if err != nil {
		return err
	}
	after := int64(0)
	for {
		ids, err := s.queue.ActiveAccountIDs(ctx, name, after, fence, s.options.PageSize)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				return err
			}
			counts, checkErr := state.provider.Check(ctx, id)
			if err := ctx.Err(); err != nil {
				return err
			}
			s.mu.Lock()
			state.status.Checked++
			if checkErr != nil {
				state.status.Failures++
				state.status.LastError = s.options.ErrorCode(checkErr)
			} else {
				state.status.Events += counts.Events
				state.status.Deliveries += counts.Deliveries
				state.status.Warnings += counts.Warnings
			}
			s.mu.Unlock()
			if checkErr != nil {
				s.options.Logger.Warn("account poll failed", "provider", name, "account_id", id, "reason", s.options.ErrorCode(checkErr))
			}
			after = id
		}
		if len(ids) < s.options.PageSize {
			return nil
		}
	}
}

// Trigger is a coalescing hint. It never restarts a worker or overlaps a cycle.
func (s *Scheduler) Trigger(name string) error {
	state := s.providers[name]
	if state == nil {
		return errors.New("unknown provider")
	}
	select {
	case state.wake <- struct{}{}:
	default:
	}
	return nil
}
func (s *Scheduler) Status() []Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]Status, 0, len(s.names))
	for _, name := range s.names {
		result = append(result, s.providers[name].status)
	}
	return result
}

func (s *Scheduler) Run(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case s.runGate <- struct{}{}:
	default:
		return ErrBusy
	}
	defer func() { <-s.runGate }()
	ctxRun, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, len(s.names))
	var group sync.WaitGroup
	for _, name := range s.names {
		state := s.providers[name]
		group.Go(func() { results <- s.loop(ctxRun, state); cancel() })
	}
	group.Wait()
	close(results)
	for err := range results {
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return ctxRun.Err()
}
func (s *Scheduler) loop(ctx context.Context, state *providerState) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.Cycle(ctx, state.provider.Name); err != nil && !errors.Is(err, ErrBusy) {
			return err
		}
		timer := time.NewTimer(state.provider.Delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-state.wake:
		case <-timer.C:
		}
		timer.Stop()
	}
}
func pollErrorCode(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, store.ErrStalePoll):
		return "stale_poll"
	case errors.Is(err, store.ErrAccountNotFound):
		return "account_removed"
	case errors.Is(err, store.ErrBackpressure):
		return "backpressure"
	default:
		return "poll_failed"
	}
}
