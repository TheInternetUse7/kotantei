package scheduler

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kotantei/internal/store"
)

func schedulerFixture(t *testing.T) (*store.Store, *store.Outbox) {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "scheduler.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.DB().Exec(`INSERT INTO users(id) VALUES ('1'),('2'),('3');INSERT INTO guilds(id,channel_id) VALUES ('10','11');INSERT INTO guild_users(guild_id,user_id) VALUES ('10','1'),('10','2');
		INSERT INTO accounts(id,provider,external_id,user_id) VALUES (1,'anilist','101','1'),(2,'anilist','102','2'),(3,'anilist','103','3'),(4,'mal','201','1');`); err != nil {
		t.Fatal(err)
	}
	q, err := store.NewOutbox(s, store.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	return s, q
}

func TestCycleStreamsOnlyActiveAccountsAndReturnsBoundedHealthCounts(t *testing.T) {
	_, q := schedulerFixture(t)
	var checked []int64
	s, err := New(q, []Provider{{Name: "anilist", Delay: time.Minute, Check: func(ctx context.Context, id int64) (Counts, error) {
		checked = append(checked, id)
		return Counts{Events: 1, Deliveries: 2, Warnings: 3}, nil
	}}}, Options{PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Cycle(context.Background(), "anilist"); err != nil {
		t.Fatal(err)
	}
	status := s.Status()
	if !reflect.DeepEqual(checked, []int64{1, 2}) || len(status) != 1 || status[0].Running || status[0].Checked != 2 || status[0].Events != 2 || status[0].Deliveries != 4 || status[0].Warnings != 6 || status[0].CompletedAt.IsZero() {
		t.Fatal("cycle fetched orphan accounts or lost health counts", checked, status)
	}
}

func TestAccountFenceDefersNewLinksUntilTheNextCycle(t *testing.T) {
	db, q := schedulerFixture(t)
	var checked []int64
	added := false
	s, err := New(q, []Provider{{Name: "anilist", Delay: time.Minute, Check: func(ctx context.Context, id int64) (Counts, error) {
		checked = append(checked, id)
		if !added {
			added = true
			_, err := db.DB().Exec(`INSERT INTO users(id) VALUES ('4');INSERT INTO accounts(id,provider,external_id,user_id) VALUES (5,'anilist','104','4');INSERT INTO guild_users(guild_id,user_id) VALUES ('10','3'),('10','4')`)
			if err != nil {
				t.Error(err)
			}
		}
		return Counts{}, nil
	}}}, Options{PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Cycle(context.Background(), "anilist"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(checked, []int64{1, 2, 3}) {
		t.Fatal("cycle exceeded its captured account fence", checked)
	}
	checked = nil
	if err := s.Cycle(context.Background(), "anilist"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(checked, []int64{1, 2, 3, 5}) {
		t.Fatal("next cycle did not discover new links", checked)
	}
}

func TestProviderFailuresAreIsolatedAndNeverLogUpstreamBodies(t *testing.T) {
	_, q := schedulerFixture(t)
	var log bytes.Buffer
	s, err := New(q, []Provider{{Name: "anilist", Delay: time.Minute, Check: func(ctx context.Context, id int64) (Counts, error) {
		if id == 1 {
			return Counts{}, errors.New("DO-NOT-LOG-TOKEN")
		}
		return Counts{Events: 1}, nil
	}}}, Options{Logger: slog.New(slog.NewJSONHandler(&log, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Cycle(context.Background(), "anilist"); err != nil {
		t.Fatal(err)
	}
	status := s.Status()
	if len(status) != 1 || status[0].Failures != 1 || status[0].Events != 1 || status[0].LastError != "poll_failed" || strings.Contains(log.String(), "DO-NOT-LOG") {
		t.Fatal("account failure stopped other accounts or leaked error bodies", status, log.String())
	}
}

func TestConcurrentCyclesAreRejectedAndCancellationStopsRemainingAccounts(t *testing.T) {
	_, q := schedulerFixture(t)
	started := make(chan struct{})
	var once sync.Once
	var calls atomic.Int64
	s, err := New(q, []Provider{{Name: "anilist", Delay: time.Minute, Check: func(ctx context.Context, id int64) (Counts, error) {
		calls.Add(1)
		once.Do(func() { close(started) })
		<-ctx.Done()
		return Counts{}, ctx.Err()
	}}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Cycle(ctx, "anilist") }()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("cycle did not start")
	}
	if err := s.Cycle(context.Background(), "anilist"); !errors.Is(err, ErrBusy) {
		t.Fatal("concurrent cycle allowed", err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cycle ignored cancellation")
	}
	if calls.Load() != 1 || s.Status()[0].Running {
		t.Fatal("canceled cycle continued collecting")
	}
}

func TestProviderLoopsAreIndependentAndTriggersWakeWithoutRestartingWorkers(t *testing.T) {
	_, q := schedulerFixture(t)
	aniStarted := make(chan struct{})
	malCalls := make(chan struct{}, 4)
	var once sync.Once
	s, err := New(q, []Provider{
		{Name: "anilist", Delay: time.Hour, Check: func(ctx context.Context, id int64) (Counts, error) {
			once.Do(func() { close(aniStarted) })
			<-ctx.Done()
			return Counts{}, ctx.Err()
		}},
		{Name: "mal", Delay: time.Hour, Check: func(ctx context.Context, id int64) (Counts, error) { malCalls <- struct{}{}; return Counts{}, nil }},
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	select {
	case <-aniStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("AniList did not start")
	}
	select {
	case <-malCalls:
	case <-time.After(10 * time.Second):
		t.Fatal("one provider blocked another")
	}
	if err := s.Trigger("mal"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-malCalls:
	case <-time.After(10 * time.Second):
		t.Fatal("trigger did not wake a sleeping provider")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("provider loops did not shut down")
	}
}

func TestSchedulerValidatesBoundsNamesAndAlreadyCanceledRuns(t *testing.T) {
	_, q := schedulerFixture(t)
	for _, providers := range [][]Provider{nil, {{Name: "unknown", Delay: time.Second, Check: func(context.Context, int64) (Counts, error) { return Counts{}, nil }}}, {{Name: "anilist", Delay: -time.Second, Check: func(context.Context, int64) (Counts, error) { return Counts{}, nil }}}} {
		if _, err := New(q, providers, Options{}); err == nil {
			t.Fatal("invalid scheduler accepted")
		}
	}
	s, err := New(q, []Provider{{Name: "anilist", Delay: time.Hour, Check: func(context.Context, int64) (Counts, error) {
		t.Error("canceled run checked an account")
		return Counts{}, nil
	}}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation was masked", err)
	}
	if err := s.Trigger("unknown"); err == nil {
		t.Fatal("trigger accepted an unknown provider")
	}
}
