package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func mustClaim(t *testing.T, q *Outbox, now time.Time) *Delivery {
	t.Helper()
	delivery, err := q.Claim(context.Background(), now, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if delivery == nil {
		t.Fatal("expected ready delivery")
	}
	return delivery
}

func TestClaimPersistsOneLeasePerChannelAndAcknowledgesIndependently(t *testing.T) {
	_, q := outboxFixture(t, Limits{})
	if _, err := q.Record(context.Background(), poll(notification("1", "42", 101))); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(200, 0).UTC()
	first := mustClaim(t, q, now)
	second := mustClaim(t, q, now)
	if first.ChannelID == second.ChannelID || first.ID == second.ID || first.Lease == second.Lease {
		t.Fatal("claims were not isolated")
	}
	if len(first.Lease) != 32 || first.Attempts != 1 || !first.LeaseUntil.Equal(now.Add(30*time.Second)) || first.Event.SourceID != "1" || first.UserID != "1" || first.Provider != "anilist" {
		t.Fatalf("incorrect claim: %+v", first)
	}
	if next, err := q.Claim(context.Background(), now, 30*time.Second); err != nil || next != nil {
		t.Fatalf("active lease was claimed twice: %+v %v", next, err)
	}
	if err := q.Acknowledge(context.Background(), first.Ref(), "1000", now); err != nil {
		t.Fatal(err)
	}
	status, err := q.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Sent != 1 || status.Pending != 1 || status.Leased != 1 {
		t.Fatalf("channel acknowledgment was not isolated: %+v", status)
	}
	if err := q.Acknowledge(context.Background(), first.Ref(), "1000", now); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("old lease acknowledged twice: %v", err)
	}
}

func TestExpiredLeasesRecoverAfterDatabaseReopen(t *testing.T) {
	s, q := outboxFixture(t, Limits{})
	if _, err := q.Record(context.Background(), poll(notification("1", "42", 101))); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(200, 0).UTC()
	first := mustClaim(t, q, now)
	_ = mustClaim(t, q, now)
	path := s.Path()
	s.Close()
	reopened, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	q, err = NewOutbox(reopened, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if next, err := q.Claim(context.Background(), now.Add(29*time.Second), 30*time.Second); err != nil || next != nil {
		t.Fatalf("unexpired lease replayed: %+v %v", next, err)
	}
	recovered := mustClaim(t, q, now.Add(31*time.Second))
	if recovered.ID != first.ID || recovered.Lease == first.Lease || recovered.Attempts != 2 {
		t.Fatalf("incorrect recovery: %+v", recovered)
	}
	if err := q.Acknowledge(context.Background(), first.Ref(), "1000", now); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale worker overwrote a replacement lease: %v", err)
	}
	if err := q.Acknowledge(context.Background(), recovered.Ref(), "1001", now.Add(31*time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestRetryHoldsChannelOrderWithoutBlockingOtherChannels(t *testing.T) {
	_, q := outboxFixture(t, Limits{})
	if _, err := q.Record(context.Background(), poll(notification("1", "42", 101), notification("2", "43", 102))); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(200, 0).UTC()
	failed := mustClaim(t, q, now)
	if err := q.Retry(context.Background(), failed.Ref(), now.Add(100*time.Second), SendFailed); err != nil {
		t.Fatal(err)
	}
	other := mustClaim(t, q, now)
	if other.ChannelID == failed.ChannelID || other.Event.SourceID != "1" {
		t.Fatal("retry was bypassed in the same channel")
	}
	if err := q.Acknowledge(context.Background(), other.Ref(), "1000", now); err != nil {
		t.Fatal(err)
	}
	otherNext := mustClaim(t, q, now)
	if otherNext.ChannelID != other.ChannelID || otherNext.Event.SourceID != "2" {
		t.Fatal("healthy channel did not progress independently")
	}
	if err := q.Acknowledge(context.Background(), otherNext.Ref(), "1001", now); err != nil {
		t.Fatal(err)
	}
	if next, err := q.Claim(context.Background(), now.Add(99*time.Second), 30*time.Second); err != nil || next != nil {
		t.Fatalf("early retry/out-of-order send: %+v %v", next, err)
	}
	retry := mustClaim(t, q, now.Add(100*time.Second))
	if retry.ChannelID != failed.ChannelID || retry.Event.SourceID != "1" || retry.Attempts != 2 {
		t.Fatalf("incorrect retry: %+v", retry)
	}
	if err := q.Acknowledge(context.Background(), retry.Ref(), "1002", now.Add(100*time.Second)); err != nil {
		t.Fatal(err)
	}
	last := mustClaim(t, q, now.Add(100*time.Second))
	if last.Event.SourceID != "2" || last.ChannelID != failed.ChannelID {
		t.Fatal("channel chronology was not preserved")
	}
}

func TestClaimUsesChronologicalOrderEvenForLaterBackfilledRecords(t *testing.T) {
	_, q := outboxFixture(t, Limits{})
	if _, err := q.Record(context.Background(), poll(notification("1", "42", 110))); err != nil {
		t.Fatal(err)
	}
	earlier := notification("2", "43", 105)
	earlier.Stream = "anime"
	input := poll(earlier)
	input.AccountID = 2
	if _, err := q.Record(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	claimed := mustClaim(t, q, time.Unix(200, 0))
	if claimed.Event.SourceID != "2" {
		t.Fatal("queue ordered by insertion instead of event chronology")
	}
}

func TestPermanentChannelFailureIsIsolatedAndResumable(t *testing.T) {
	_, q := outboxFixture(t, Limits{})
	ctx := context.Background()
	if _, err := q.Record(ctx, poll(notification("1", "42", 101), notification("2", "43", 102))); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(200, 0).UTC()
	bad := mustClaim(t, q, now)
	if err := q.BlockChannel(ctx, bad.Ref(), MissingPermissions, now); err != nil {
		t.Fatal(err)
	}
	for n := range 2 {
		healthy := mustClaim(t, q, now)
		if healthy.ChannelID == bad.ChannelID {
			t.Fatal("blocked channel was retried automatically")
		}
		if err := q.Acknowledge(ctx, healthy.Ref(), []string{"1000", "1001"}[n], now); err != nil {
			t.Fatal(err)
		}
	}
	input := poll(notification("3", "44", 103))
	input.ExpectedRevision = 1
	result, err := q.Record(ctx, input)
	if err != nil || result.Deliveries != 1 || result.FailedDeliveries != 1 {
		t.Fatalf("blocked target stopped healthy fan-out: %+v %v", result, err)
	}
	status, err := q.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Pending != 1 || status.Failed != 3 || status.Sent != 2 || status.BlockedChannels != 1 || len(status.Channels) != 1 || status.Channels[0].ChannelID != bad.ChannelID || status.Channels[0].Reason != MissingPermissions {
		t.Fatalf("missing actionable failure status: %+v", status)
	}
	resumed, err := q.ResumeChannel(ctx, bad.ChannelID, now.Add(time.Second))
	if err != nil || resumed != 3 {
		t.Fatalf("resume failed: count=%d err=%v", resumed, err)
	}
	status, err = q.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Pending != 4 || status.Failed != 0 || status.Sent != 2 || status.BlockedChannels != 0 {
		t.Fatalf("resume replayed successful targets: %+v", status)
	}
	if next := mustClaim(t, q, now.Add(time.Second)); next.ChannelID != bad.ChannelID || next.Event.SourceID != "1" {
		t.Fatal("resume lost chronological order")
	}
}

func TestResumeHonorsBackpressureWithoutChangingFailureState(t *testing.T) {
	s, q := outboxFixture(t, Limits{})
	ctx := context.Background()
	if _, err := q.Record(ctx, poll(notification("1", "42", 101))); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(200, 0)
	bad := mustClaim(t, q, now)
	if err := q.BlockChannel(ctx, bad.Ref(), UnknownChannel, now); err != nil {
		t.Fatal(err)
	}
	limits := DefaultLimits()
	limits.MaxPending = 1
	limited, err := NewOutbox(s, limits)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := limited.ResumeChannel(ctx, bad.ChannelID, now); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("expected resume backpressure, got %v", err)
	}
	status, err := q.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Pending != 1 || status.Failed != 1 || status.BlockedChannels != 1 {
		t.Fatal("failed resume changed queue state")
	}
}

func TestInvalidMessageFailsOnlyOneDeliveryNotItsChannel(t *testing.T) {
	_, q := outboxFixture(t, Limits{})
	ctx := context.Background()
	if _, err := q.Record(ctx, poll(notification("1", "42", 101), notification("2", "43", 102))); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(200, 0)
	first := mustClaim(t, q, now)
	if err := q.Fail(ctx, first.Ref(), InvalidMessage); err != nil {
		t.Fatal(err)
	}
	second := mustClaim(t, q, now)
	if err := q.Acknowledge(ctx, second.Ref(), "1000", now); err != nil {
		t.Fatal(err)
	}
	next := mustClaim(t, q, now)
	if next.Event.SourceID != "2" {
		t.Fatal("one bad payload blocked the channel head")
	}
	status, err := q.Status(ctx)
	if err != nil || status.Failed != 1 || status.BlockedChannels != 0 {
		t.Fatalf("bad payload blocked a channel: %+v %v", status, err)
	}
}

func TestReleaseMakesCanceledAttemptAvailableWithoutWaitingForLeaseExpiry(t *testing.T) {
	_, q := outboxFixture(t, Limits{})
	if _, err := q.Record(context.Background(), poll(notification("1", "42", 101))); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(200, 0)
	first := mustClaim(t, q, now)
	if err := q.Release(context.Background(), first.Ref()); err != nil {
		t.Fatal(err)
	}
	second := mustClaim(t, q, now)
	if second.ID != first.ID || second.Lease == first.Lease || second.Attempts != 2 {
		t.Fatal("released lease was not immediately recoverable")
	}
}

func TestRemovedTargetsAreCanceledBeforeSending(t *testing.T) {
	s, q := outboxFixture(t, Limits{})
	if _, err := q.Record(context.Background(), poll(notification("1", "42", 101))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`DELETE FROM guild_users WHERE user_id='1'`); err != nil {
		t.Fatal(err)
	}
	if next, err := q.Claim(context.Background(), time.Unix(200, 0), 30*time.Second); err != nil || next != nil {
		t.Fatalf("stale membership was used: %+v %v", next, err)
	}
	status, err := q.Status(context.Background())
	if err != nil || status.Canceled != 2 || status.Pending != 0 {
		t.Fatalf("removed targets were not canceled: %+v %v", status, err)
	}
}

func TestRemovingSubscriptionKeepsAnInFlightChannelLease(t *testing.T) {
	s, q := outboxFixture(t, Limits{})
	ctx := context.Background()
	if _, err := s.DB().Exec(`
		DELETE FROM guild_users WHERE guild_id='30' AND user_id='2';
		INSERT INTO guild_users(guild_id,user_id) VALUES ('10','2');
		INSERT INTO accounts(id,provider,external_id,user_id) VALUES (3,'anilist','301','2');
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Record(ctx, poll(notification("1", "42", 101))); err != nil {
		t.Fatal(err)
	}
	later := poll(notification("2", "43", 102))
	later.AccountID = 3
	if _, err := q.Record(ctx, later); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(200, 0).UTC()
	first := mustClaim(t, q, now)
	_ = mustClaim(t, q, now)
	if first.ChannelID != "11" {
		t.Fatal("expected the shared channel's first delivery")
	}
	if _, err := s.DB().Exec(`DELETE FROM guild_users WHERE guild_id='10' AND user_id='1'`); err != nil {
		t.Fatal(err)
	}
	if next, err := q.Claim(ctx, now, 30*time.Second); err != nil || next != nil {
		t.Fatalf("subscription removal allowed overlapping channel sends: %+v %v", next, err)
	}
	if err := q.Release(ctx, first.Ref()); err != nil {
		t.Fatalf("the in-flight lease was discarded: %v", err)
	}
	next := mustClaim(t, q, now)
	if next.AccountID != 3 || next.ChannelID != first.ChannelID {
		t.Fatal("the live subscription did not progress after the in-flight send ended")
	}
	status, err := q.Status(ctx)
	if err != nil || status.Canceled != 1 {
		t.Fatalf("the removed target was not canceled after release: %+v %v", status, err)
	}
}

func TestConcurrentClaimersCannotLeaseOneDeliveryTwice(t *testing.T) {
	_, q := outboxFixture(t, Limits{})
	if _, err := q.Record(context.Background(), poll(notification("1", "42", 101))); err != nil {
		t.Fatal(err)
	}
	const workers = 16
	results := make(chan *Delivery, workers)
	errorsFound := make(chan error, workers)
	for range workers {
		go func() {
			delivery, err := q.Claim(context.Background(), time.Unix(200, 0), 30*time.Second)
			results <- delivery
			errorsFound <- err
		}()
	}
	seen := map[int64]bool{}
	for range workers {
		delivery := <-results
		if delivery != nil {
			if seen[delivery.ID] {
				t.Fatal("duplicate delivery lease")
			}
			seen[delivery.ID] = true
		}
		if err := <-errorsFound; err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 2 {
		t.Fatalf("claimed %d targets", len(seen))
	}
}

func TestDeliveryUpdatesRejectUnknownFailureCodesAndWrongLeases(t *testing.T) {
	_, q := outboxFixture(t, Limits{})
	ctx := context.Background()
	if _, err := q.Record(ctx, poll(notification("1", "42", 101))); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(200, 0)
	delivery := mustClaim(t, q, now)
	if err := q.Retry(ctx, delivery.Ref(), now, Reason("DO-NOT-STORE-SECRET")); err == nil {
		t.Fatal("arbitrary error text persisted")
	}
	wrong := delivery.Ref()
	wrong.Lease = "wrong-lease"
	if err := q.Release(ctx, wrong); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("wrong lease was accepted: %v", err)
	}
	if err := q.Acknowledge(ctx, delivery.Ref(), "", now); err == nil {
		t.Fatal("empty send receipt accepted")
	}
	status, err := q.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(status.Channels, []ChannelFailure{}) || status.Sent != 0 {
		t.Fatal("invalid updates changed channel/send state")
	}
}

func TestCanceledClaimDoesNotTakeAWorkLease(t *testing.T) {
	_, q := outboxFixture(t, Limits{})
	if _, err := q.Record(context.Background(), poll(notification("1", "42", 101))); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := q.Claim(ctx, time.Unix(200, 0), 30*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	status, err := q.Status(context.Background())
	if err != nil || status.Leased != 0 {
		t.Fatalf("canceled claim took a lease: %+v %v", status, err)
	}
}

func TestOutboxInspectionIsReadOnlyAndRequiresTheDeliverySchema(t *testing.T) {
	old := versionOneDatabase(t)
	before := fileDigest(t, old)
	if _, err := InspectOutbox(context.Background(), old); !errors.Is(err, ErrNeedsMigration) {
		t.Fatalf("expected explicit upgrade requirement, got %v", err)
	}
	if fileDigest(t, old) != before {
		t.Fatal("outbox inspection migrated storage")
	}
	s, q := outboxFixture(t, Limits{})
	if _, err := q.Record(context.Background(), poll(notification("1", "42", 101))); err != nil {
		t.Fatal(err)
	}
	path := s.Path()
	s.Close()
	before = fileDigest(t, path)
	status, err := InspectOutbox(context.Background(), path)
	if err != nil || status.Pending != 2 {
		t.Fatalf("outbox inspection failed: %+v %v", status, err)
	}
	if fileDigest(t, path) != before {
		t.Fatal("read-only outbox inspection changed the database")
	}
}

func TestFailureStatusDoesNotLoadUnboundedChannelLists(t *testing.T) {
	s, q := outboxFixture(t, Limits{})
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for n := range 101 {
		if _, err := tx.Exec(`INSERT INTO delivery_channels(channel_id,blocked_reason,blocked_at) VALUES (?,'unknown_channel',0)`, 1000+n); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	status, err := q.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.BlockedChannels != 101 || len(status.Channels) != 100 || !status.MoreChannels {
		t.Fatalf("status was not bounded: %+v", status)
	}
}
