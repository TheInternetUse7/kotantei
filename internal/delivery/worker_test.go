package delivery

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kotantei/internal/store"
	"kotantei/internal/update"
)

type senderFunc func(context.Context, store.Delivery) (string, error)

func (f senderFunc) Send(ctx context.Context, delivery store.Delivery) (string, error) {
	return f(ctx, delivery)
}

func workerOutbox(t *testing.T, events int) (*store.Store, *store.Outbox) {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "worker.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.DB().Exec(`INSERT INTO users(id) VALUES ('1');
		INSERT INTO guilds(id,channel_id) VALUES ('10','11'),('20','21');
		INSERT INTO guild_users(guild_id,user_id) VALUES ('10','1'),('20','1');
		INSERT INTO accounts(id,provider,external_id,user_id) VALUES (1,'anilist','101','1')`); err != nil {
		t.Fatal(err)
	}
	q, err := store.NewOutbox(s, store.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	input := store.Poll{AccountID: 1, Now: time.Unix(200, 0)}
	for n := range events {
		input.Events = append(input.Events, update.Event{Stream: update.Activity, Kind: update.List, SourceID: strconv.Itoa(n + 1), Subject: update.Subject{Kind: update.AnimeSubject, ID: strconv.Itoa(n + 42)}, OccurredAt: time.Unix(int64(101+n), 0).UTC(), Payload: json.RawMessage(`{"title":"Example"}`)})
	}
	if _, err := q.Record(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	return s, q
}

func testOptions() Options {
	return Options{Now: func() time.Time { return time.Unix(200, 0).UTC() }, Jitter: func() float64 { return .5 }, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

func mustWorker(t *testing.T, q *store.Outbox, sender Sender, options Options) *Worker {
	t.Helper()
	worker, err := New(q, sender, options)
	if err != nil {
		t.Fatal(err)
	}
	return worker
}

func TestWorkerAcknowledgesSuccessfulChannelsInPersistentStorage(t *testing.T) {
	_, q := workerOutbox(t, 1)
	worker := mustWorker(t, q, senderFunc(func(context.Context, store.Delivery) (string, error) { return "1000", nil }), testOptions())
	for range 2 {
		if processed, err := worker.ProcessNext(context.Background()); err != nil || !processed {
			t.Fatalf("dispatch failed: processed=%v err=%v", processed, err)
		}
	}
	if processed, err := worker.ProcessNext(context.Background()); err != nil || processed {
		t.Fatalf("sent work replayed: processed=%v err=%v", processed, err)
	}
	status, err := q.Status(context.Background())
	if err != nil || status.Sent != 2 || status.Pending != 0 {
		t.Fatalf("missing acknowledgments: %+v %v", status, err)
	}
}

func TestWorkerRetriesWithoutReplayingASuccessfulChannel(t *testing.T) {
	s, q := workerOutbox(t, 1)
	now := time.Unix(200, 0).UTC()
	options := testOptions()
	options.Now = func() time.Time { return now }
	calls := map[string]int{}
	worker := mustWorker(t, q, senderFunc(func(ctx context.Context, delivery store.Delivery) (string, error) {
		calls[delivery.ChannelID]++
		if delivery.ChannelID == "11" && calls[delivery.ChannelID] == 1 {
			return "", errors.New("transient network error")
		}
		return "1000", nil
	}), options)
	for range 2 {
		if processed, err := worker.ProcessNext(context.Background()); err != nil || !processed {
			t.Fatalf("dispatch failed: %v %v", processed, err)
		}
	}
	var due int64
	if err := s.DB().QueryRow(`SELECT available_at FROM deliveries WHERE channel_id='11'`).Scan(&due); err != nil {
		t.Fatal(err)
	}
	if due != now.Add(5*time.Second).UnixMicro() {
		t.Fatal("retry backoff was not persisted")
	}
	if processed, err := worker.ProcessNext(context.Background()); err != nil || processed {
		t.Fatal("retry ignored its due time")
	}
	now = now.Add(5 * time.Second)
	if processed, err := worker.ProcessNext(context.Background()); err != nil || !processed {
		t.Fatalf("retry failed: %v %v", processed, err)
	}
	status, err := q.Status(context.Background())
	if err != nil || status.Sent != 2 || calls["11"] != 2 || calls["21"] != 1 {
		t.Fatalf("successful target replayed: %+v %v calls=%v", status, err, calls)
	}
}

func TestWorkerHonorsUpstreamRetryAfterBeyondItsBackoffCap(t *testing.T) {
	s, q := workerOutbox(t, 1)
	options := testOptions()
	options.RetryMax = 10 * time.Second
	worker := mustWorker(t, q, senderFunc(func(context.Context, store.Delivery) (string, error) {
		return "", &Failure{Reason: store.SendFailed, RetryAfter: 90 * time.Second}
	}), options)
	if processed, err := worker.ProcessNext(context.Background()); err != nil || !processed {
		t.Fatalf("dispatch failed: %v %v", processed, err)
	}
	var due int64
	if err := s.DB().QueryRow(`SELECT available_at FROM deliveries WHERE channel_id='11'`).Scan(&due); err != nil {
		t.Fatal(err)
	}
	if due != time.Unix(290, 0).UnixMicro() {
		t.Fatal("server retry-after was shortened")
	}
}

func TestWorkerIsolatesPermanentChannelFailures(t *testing.T) {
	_, q := workerOutbox(t, 2)
	worker := mustWorker(t, q, senderFunc(func(ctx context.Context, delivery store.Delivery) (string, error) {
		if delivery.ChannelID == "11" {
			return "", &Failure{Reason: store.MissingPermissions}
		}
		return "1000", nil
	}), testOptions())
	for range 3 {
		if processed, err := worker.ProcessNext(context.Background()); err != nil || !processed {
			t.Fatalf("dispatch failed: %v %v", processed, err)
		}
	}
	status, err := q.Status(context.Background())
	if err != nil || status.Failed != 2 || status.Sent != 2 || status.BlockedChannels != 1 {
		t.Fatalf("channel failure stopped healthy delivery: %+v %v", status, err)
	}
}

func TestWorkerTreatsInvalidMessagesAsIndividualFailures(t *testing.T) {
	_, q := workerOutbox(t, 1)
	worker := mustWorker(t, q, senderFunc(func(context.Context, store.Delivery) (string, error) {
		return "", &Failure{Reason: store.InvalidMessage}
	}), testOptions())
	if processed, err := worker.ProcessNext(context.Background()); err != nil || !processed {
		t.Fatalf("dispatch failed: %v %v", processed, err)
	}
	status, err := q.Status(context.Background())
	if err != nil || status.Failed != 1 || status.BlockedChannels != 0 {
		t.Fatalf("bad message blocked a channel: %+v %v", status, err)
	}
}

func TestWorkerDoesNotStoreOrLogRawTransportErrorText(t *testing.T) {
	s, q := workerOutbox(t, 1)
	var logs bytes.Buffer
	options := testOptions()
	options.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	worker := mustWorker(t, q, senderFunc(func(context.Context, store.Delivery) (string, error) { return "", errors.New("DO-NOT-LOG-SECRET") }), options)
	if _, err := worker.ProcessNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	var code string
	if err := s.DB().QueryRow(`SELECT last_error_code FROM deliveries WHERE channel_id='11'`).Scan(&code); err != nil {
		t.Fatal(err)
	}
	if code != "send_failed" || strings.Contains(logs.String(), "DO-NOT-LOG") {
		t.Fatal("raw transport error leaked")
	}
}

func TestWorkerTimeoutReleasesLeaseAndSchedulesRetry(t *testing.T) {
	s, q := workerOutbox(t, 1)
	options := testOptions()
	options.SendTimeout = 20 * time.Millisecond
	worker := mustWorker(t, q, senderFunc(func(ctx context.Context, _ store.Delivery) (string, error) { <-ctx.Done(); return "", ctx.Err() }), options)
	if processed, err := worker.ProcessNext(context.Background()); err != nil || !processed {
		t.Fatalf("timeout handling failed: %v %v", processed, err)
	}
	var code, lease string
	if err := s.DB().QueryRow(`SELECT last_error_code,lease_token FROM deliveries WHERE channel_id='11'`).Scan(&code, &lease); err != nil {
		t.Fatal(err)
	}
	if code != "timeout" || lease != "" {
		t.Fatal("timed-out work kept a lease or lost retry state")
	}
}

func TestSuccessfulSendIsAcknowledgedEvenIfParentWasCanceled(t *testing.T) {
	_, q := workerOutbox(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker := mustWorker(t, q, senderFunc(func(context.Context, store.Delivery) (string, error) { cancel(); return "1000", nil }), testOptions())
	processed, err := worker.ProcessNext(ctx)
	if !processed || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation not surfaced: %v %v", processed, err)
	}
	status, err := q.Status(context.Background())
	if err != nil || status.Sent != 1 || status.Pending != 1 || status.Leased != 0 {
		t.Fatalf("acknowledged response was lost on shutdown: %+v %v", status, err)
	}
}

func TestRunStopsAndReleasesAnInterruptedSend(t *testing.T) {
	_, q := workerOutbox(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{}, 1)
	worker := mustWorker(t, q, senderFunc(func(ctx context.Context, _ store.Delivery) (string, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return "", ctx.Err()
	}), testOptions())
	finished := make(chan error, 1)
	go func() { finished <- worker.Run(ctx) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not start")
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected exit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop")
	}
	status, err := q.Status(context.Background())
	if err != nil || status.Pending != 2 || status.Leased != 0 || status.Failed != 0 {
		t.Fatalf("shutdown lost pending work: %+v %v", status, err)
	}
}

func TestWorkerPoolIsBoundedAndNeverSendsConcurrentlyToOneChannel(t *testing.T) {
	_, q := workerOutbox(t, 3)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var calls atomic.Int64
	var concurrent, maxConcurrent atomic.Int64
	var mutex sync.Mutex
	active := map[string]bool{}
	var overlap atomic.Bool
	sender := senderFunc(func(ctx context.Context, delivery store.Delivery) (string, error) {
		mutex.Lock()
		if active[delivery.ChannelID] {
			overlap.Store(true)
		}
		active[delivery.ChannelID] = true
		mutex.Unlock()
		current := concurrent.Add(1)
		for {
			old := maxConcurrent.Load()
			if old >= current || maxConcurrent.CompareAndSwap(old, current) {
				break
			}
		}
		number := calls.Add(1)
		if number <= 2 {
			entered <- struct{}{}
		}
		select {
		case <-release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		concurrent.Add(-1)
		mutex.Lock()
		delete(active, delivery.ChannelID)
		mutex.Unlock()
		if number == 6 {
			cancel()
		}
		return strconv.FormatInt(1000+number, 10), nil
	})
	options := testOptions()
	options.Workers = 2
	worker := mustWorker(t, q, sender, options)
	finished := make(chan error, 1)
	go func() { finished <- worker.Run(ctx) }()
	for range 2 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("worker pool did not fill available independent channels")
		}
	}
	close(release)
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker pool did not drain/stop")
	}
	status, err := q.Status(context.Background())
	if err != nil || status.Sent != 6 || status.Pending != 0 || overlap.Load() || maxConcurrent.Load() != 2 {
		t.Fatalf("invalid bounded dispatch: %+v %v overlap=%v max=%d", status, err, overlap.Load(), maxConcurrent.Load())
	}
}

func TestRetryDelayIsCappedAndHonorsJitterAndRetryAfter(t *testing.T) {
	worker := Worker{options: Options{RetryMin: 5 * time.Second, RetryMax: 5 * time.Minute, Jitter: func() float64 { return .5 }}}
	for _, tc := range []struct {
		attempts int64
		want     time.Duration
	}{{1, 5 * time.Second}, {2, 10 * time.Second}, {3, 20 * time.Second}, {1000000, 5 * time.Minute}} {
		if got := worker.retryDelay(tc.attempts, 0); got != tc.want {
			t.Fatalf("attempt %d: %s, want %s", tc.attempts, got, tc.want)
		}
	}
	if got := worker.retryDelay(1, time.Hour); got != time.Hour {
		t.Fatal("upstream retry-after was capped")
	}
	worker.options.Jitter = func() float64 { return 0 }
	if got := worker.retryDelay(1, 0); got != 4*time.Second {
		t.Fatalf("jitter not applied: %s", got)
	}
	worker.options.Jitter = func() float64 { return 1 }
	if got := worker.retryDelay(1000000, 0); got > worker.options.RetryMax {
		t.Fatal("jitter exceeded maximum backoff")
	}
}

func TestWorkerOptionsRejectUnsafeConcurrencyAndDeadlines(t *testing.T) {
	_, q := workerOutbox(t, 0)
	sender := senderFunc(func(context.Context, store.Delivery) (string, error) { return "1000", nil })
	if _, err := New(nil, sender, Options{}); err == nil {
		t.Fatal("nil outbox accepted")
	}
	if _, err := New(q, nil, Options{}); err == nil {
		t.Fatal("nil sender accepted")
	}
	for _, options := range []Options{{Workers: -1}, {Workers: 17}, {SendTimeout: -time.Second}, {LeaseDuration: time.Second}, {RetryMin: 10 * time.Second, RetryMax: time.Second}, {IdleDelay: -time.Second}} {
		if _, err := New(q, sender, options); err == nil {
			t.Fatalf("unsafe options accepted: %+v", options)
		}
	}
}

func TestRunDoesNotHideAStorageDeadlineAsShutdownCancellation(t *testing.T) {
	s, q := workerOutbox(t, 1)
	var held *sql.Tx
	defer func() {
		if held != nil {
			held.Rollback()
		}
	}()
	options := testOptions()
	options.StoreTimeout = 20 * time.Millisecond
	worker := mustWorker(t, q, senderFunc(func(context.Context, store.Delivery) (string, error) {
		var err error
		held, err = s.DB().Begin()
		if err != nil {
			return "", err
		}
		return "1000", nil
	}), options)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := worker.Run(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Fatalf("storage timeout was hidden: %v", err)
	}
}

func TestMalformedSendReceiptRetainsWorkForRetry(t *testing.T) {
	_, q := workerOutbox(t, 1)
	worker := mustWorker(t, q, senderFunc(func(context.Context, store.Delivery) (string, error) { return "not-a-message-id", nil }), testOptions())
	if processed, err := worker.ProcessNext(context.Background()); err != nil || !processed {
		t.Fatalf("bad receipt killed delivery instead of retaining it: %v %v", processed, err)
	}
	status, err := q.Status(context.Background())
	if err != nil || status.Pending != 2 || status.Leased != 0 || status.Sent != 0 {
		t.Fatalf("unconfirmed delivery was lost: %+v %v", status, err)
	}
}
