package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"kotantei/internal/update"
)

func outboxFixture(t *testing.T, limits Limits) (*Store, *Outbox) {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "outbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.DB().Exec(`
		INSERT INTO users(id) VALUES ('1'),('2');
		INSERT INTO guilds(id,channel_id) VALUES ('10','11'),('20','21'),('30','31');
		INSERT INTO guild_users(guild_id,user_id) VALUES ('10','1'),('20','1'),('30','2');
		INSERT INTO accounts(id,provider,external_id,user_id) VALUES (1,'anilist','101','1'),(2,'mal','201','1');
		INSERT INTO checkpoints(account_id,kind,value) VALUES (1,'activity',100),(1,'review',100),(2,'anime',100000000),(2,'manga',100000000);
	`); err != nil {
		t.Fatal(err)
	}
	q, err := NewOutbox(s, limits)
	if err != nil {
		t.Fatal(err)
	}
	return s, q
}

func notification(source, subject string, seconds int64) update.Event {
	return update.Event{Stream: update.Activity, Kind: update.List, SourceID: source, Subject: update.Subject{Kind: update.AnimeSubject, ID: subject}, OccurredAt: time.Unix(seconds, 0).UTC(), Payload: json.RawMessage(`{"title":"Example"}`)}
}

func poll(events ...update.Event) Poll {
	return Poll{AccountID: 1, ExpectedRevision: 0, Events: events, Now: time.Unix(200, 0).UTC()}
}

func countRows(t *testing.T, s *Store, table string) int {
	t.Helper()
	var count int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestOutboxUsesBoundedDefaultsAndRejectsInvalidLimits(t *testing.T) {
	s, q := outboxFixture(t, Limits{})
	if q.limits.MaxRecords != 512 || q.limits.MaxPending != 4096 || q.limits.MaxPendingBytes != 16<<20 {
		t.Fatalf("unexpected bounds: %+v", q.limits)
	}
	for _, change := range []func(*Limits){func(l *Limits) { l.MaxPending = -1 }, func(l *Limits) { l.MaxRecords = 0 }, func(l *Limits) { l.MaxPayloadBytes = 0 }, func(l *Limits) { l.MaxBatchBytes = -1 }, func(l *Limits) { l.MaxTargets = 0 }, func(l *Limits) { l.MaxBoundaryIDs = 0 }, func(l *Limits) { l.MaxFavorites = 0 }} {
		limits := DefaultLimits()
		change(&limits)
		if _, err := NewOutbox(s, limits); err == nil {
			t.Fatal("invalid limits accepted")
		}
	}
	if _, err := NewOutbox(nil, Limits{}); err == nil {
		t.Fatal("nil store accepted")
	}
}

func TestSnapshotReadsImportedCheckpointAndFavoriteState(t *testing.T) {
	s, q := outboxFixture(t, Limits{})
	if _, err := s.DB().Exec(`INSERT INTO checkpoint_items(account_id,kind,source_id) VALUES (1,'activity','old'); INSERT INTO favorites(account_id,kind,external_id,name) VALUES (1,'anime','42','Example')`); err != nil {
		t.Fatal(err)
	}
	state, err := q.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if state.Revision != 0 || state.Provider != "anilist" || state.Cursors[update.Activity].Value != 100 || !reflect.DeepEqual(state.Cursors[update.Activity].SourceIDs, []string{"old"}) {
		t.Fatalf("incorrect snapshot: %+v", state)
	}
	if len(state.Favorites) != 1 || state.Favorites[0].Subject.ID != "42" {
		t.Fatal("favorite baseline not read")
	}
}

func TestRecordPollPersistsEventTargetsAndCheckpointAtomically(t *testing.T) {
	s, q := outboxFixture(t, Limits{})
	result, err := q.Record(context.Background(), poll(notification("1", "42", 101)))
	if err != nil {
		t.Fatal(err)
	}
	if result.Events != 1 || result.Deliveries != 2 || result.Revision != 1 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if countRows(t, s, "events") != 1 || countRows(t, s, "deliveries") != 2 {
		t.Fatal("fan-out was not persisted")
	}
	state, err := q.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if state.Revision != 1 || state.Cursors[update.Activity].Value != 101 || !reflect.DeepEqual(state.Cursors[update.Activity].SourceIDs, []string{"1"}) {
		t.Fatalf("checkpoint not committed: %+v", state)
	}
}

func TestDuplicatePollDoesNotReplayOrOverwriteStoredPayload(t *testing.T) {
	s, q := outboxFixture(t, Limits{})
	event := notification("1", "42", 101)
	if _, err := q.Record(context.Background(), poll(event)); err != nil {
		t.Fatal(err)
	}
	duplicate := poll(event)
	duplicate.ExpectedRevision = 1
	duplicate.Events[0].Payload = json.RawMessage(`{"title":"Changed metadata"}`)
	result, err := q.Record(context.Background(), duplicate)
	if err != nil {
		t.Fatal(err)
	}
	if result.Events != 0 || result.Deliveries != 0 || result.Revision != 1 {
		t.Fatalf("duplicate was not a no-op: %+v", result)
	}
	var payload string
	if err := s.DB().QueryRow(`SELECT payload FROM events`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload, "Changed metadata") || countRows(t, s, "deliveries") != 2 {
		t.Fatal("duplicate changed an existing event")
	}
}

func TestRecordKeepsAllCoalescedIdentitiesAtAnEqualTimestamp(t *testing.T) {
	s, q := outboxFixture(t, Limits{})
	result, err := q.Record(context.Background(), poll(notification("9", "42", 101), notification("10", "42", 101)))
	if err != nil {
		t.Fatal(err)
	}
	if result.Events != 1 {
		t.Fatal("same-title updates were not coalesced")
	}
	state, err := q.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(state.Cursors[update.Activity].SourceIDs, []string{"10", "9"}) {
		t.Fatalf("lost consumed identities: %+v", state.Cursors)
	}
	repeated := poll(notification("9", "42", 101))
	repeated.ExpectedRevision = state.Revision
	if result, err := q.Record(context.Background(), repeated); err != nil || result.Events != 0 {
		t.Fatalf("coalesced loser replayed: %+v %v", result, err)
	}
	newTie := poll(notification("11", "43", 101))
	newTie.ExpectedRevision = state.Revision
	if result, err := q.Record(context.Background(), newTie); err != nil || result.Events != 1 {
		t.Fatalf("unseen timestamp tie skipped: %+v %v", result, err)
	}
	if countRows(t, s, "events") != 2 {
		t.Fatal("wrong event count")
	}
}

func TestRecordObservationsCanAdvanceWithoutMessages(t *testing.T) {
	s, q := outboxFixture(t, Limits{})
	input := poll()
	input.Observations = []update.Observation{{Stream: update.Activity, Value: 101, SourceID: "deleted-media"}}
	result, err := q.Record(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if result.Events != 0 || result.Revision != 1 || countRows(t, s, "deliveries") != 0 {
		t.Fatalf("unexpected observation result: %+v", result)
	}
	state, err := q.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if state.Cursors[update.Activity].Value != 101 {
		t.Fatal("observation checkpoint not saved")
	}
	input.ExpectedRevision = 1
	input.Observations = []update.Observation{{Stream: update.Activity, Value: 99, SourceID: "older"}}
	result, err = q.Record(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if result.Revision != 1 {
		t.Fatal("stale observation changed the watermark")
	}
}

func TestRecordRefusesStalePollSnapshots(t *testing.T) {
	s, q := outboxFixture(t, Limits{})
	if _, err := s.DB().Exec(`UPDATE accounts SET features=0,revision=revision+1 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Record(context.Background(), poll(notification("1", "42", 101))); !errors.Is(err, ErrStalePoll) {
		t.Fatalf("expected stale-poll error, got %v", err)
	}
	if countRows(t, s, "events") != 0 {
		t.Fatal("stale poll enqueued messages")
	}
}

func TestRecordRollsBackPartialFanoutWhenSQLiteRejectsAWritingStep(t *testing.T) {
	s, q := outboxFixture(t, Limits{})
	if _, err := s.DB().Exec(`CREATE TRIGGER reject_second_target BEFORE INSERT ON deliveries WHEN NEW.channel_id='21' BEGIN SELECT RAISE(ABORT,'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Record(context.Background(), poll(notification("1", "42", 101))); err == nil {
		t.Fatal("write failure was ignored")
	}
	if countRows(t, s, "events") != 0 || countRows(t, s, "deliveries") != 0 {
		t.Fatal("partial fan-out persisted")
	}
	state, err := q.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if state.Revision != 0 || state.Cursors[update.Activity].Value != 100 {
		t.Fatal("failed fan-out advanced the checkpoint")
	}
}

func TestBackpressureDoesNotAdvanceCheckpoints(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxPending = 1
	s, q := outboxFixture(t, limits)
	if _, err := q.Record(context.Background(), poll(notification("1", "42", 101))); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("expected backpressure, got %v", err)
	}
	if countRows(t, s, "events") != 0 {
		t.Fatal("backpressure left a partial event")
	}
	state, err := q.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if state.Revision != 0 || state.Cursors[update.Activity].Value != 100 {
		t.Fatal("backpressure lost unqueued updates")
	}
}

func TestPayloadByteBudgetCountsUTF8BytesAndChannelFanout(t *testing.T) {
	for _, configure := range []func(*Limits){func(l *Limits) { l.MaxPayloadBytes = 20 }, func(l *Limits) { l.MaxPendingBytes = 20 }} {
		limits := DefaultLimits()
		configure(&limits)
		s, q := outboxFixture(t, limits)
		event := notification("1", "42", 101)
		event.Payload = json.RawMessage(`{"text":"日本語日本語"}`)
		if _, err := q.Record(context.Background(), poll(event)); err == nil {
			t.Fatal("byte budget was ignored")
		}
		if countRows(t, s, "events") != 0 {
			t.Fatal("oversized poll was partly committed")
		}
	}
}

func TestRecordBoundsRawBatchAndBoundaryBeforeCoalescing(t *testing.T) {
	for _, configure := range []func(*Limits){func(l *Limits) { l.MaxRecords = 1 }, func(l *Limits) { l.MaxBoundaryIDs = 1 }, func(l *Limits) { l.MaxTargets = 1 }, func(l *Limits) { l.MaxBatchBytes = 10 }} {
		limits := DefaultLimits()
		configure(&limits)
		s, q := outboxFixture(t, limits)
		if _, err := q.Record(context.Background(), poll(notification("1", "42", 101), notification("2", "42", 101))); err == nil {
			t.Fatal("batch/boundary/fanout budget ignored")
		}
		if countRows(t, s, "events") != 0 {
			t.Fatal("oversized batch changed storage")
		}
	}
}

func TestNewSubscriptionsDoNotReplayAlreadyConsumedEvents(t *testing.T) {
	s, q := outboxFixture(t, Limits{})
	event := notification("1", "42", 101)
	if _, err := q.Record(context.Background(), poll(event)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO guild_users(guild_id,user_id) VALUES ('30','1')`); err != nil {
		t.Fatal(err)
	}
	repeat := poll(event)
	repeat.ExpectedRevision = 1
	if _, err := q.Record(context.Background(), repeat); err != nil {
		t.Fatal(err)
	}
	if countRows(t, s, "deliveries") != 2 {
		t.Fatal("new server received historical replay")
	}
	input := poll(notification("2", "43", 102))
	input.ExpectedRevision = 1
	result, err := q.Record(context.Background(), input)
	if err != nil || result.Deliveries != 3 {
		t.Fatalf("new event was not routed to live memberships: %+v %v", result, err)
	}
}

func TestRecordDoesNotDeliverWhenSubscriptionsDisappearDuringPolling(t *testing.T) {
	s, q := outboxFixture(t, Limits{})
	if _, err := s.DB().Exec(`DELETE FROM guild_users WHERE user_id='1'`); err != nil {
		t.Fatal(err)
	}
	result, err := q.Record(context.Background(), poll(notification("1", "42", 101)))
	if err != nil {
		t.Fatal(err)
	}
	if result.Events != 1 || result.Deliveries != 0 {
		t.Fatalf("stale routing used: %+v", result)
	}
}

func TestFavoriteBaselineAndNotificationsCommitTogether(t *testing.T) {
	s, q := outboxFixture(t, Limits{})
	a := Favorite{Subject: update.Subject{Kind: update.AnimeSubject, ID: "42"}, Name: "Anime"}
	b := Favorite{Subject: update.Subject{Kind: update.MangaSubject, ID: "43"}, Name: "Manga"}
	baseline := []Favorite{a}
	input := poll()
	input.Favorites = &baseline
	if _, err := q.Record(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	addition := notification("manga:43", "43", 101)
	addition.Stream = update.Favorites
	addition.Kind = update.FavoriteAdded
	addition.Subject = b.Subject
	changed := []Favorite{a, b}
	input = poll(addition)
	input.ExpectedRevision = 1
	input.Favorites = &changed
	result, err := q.Record(context.Background(), input)
	if err != nil || result.Events != 1 || result.Revision != 2 {
		t.Fatalf("favorite change failed: %+v %v", result, err)
	}
	input.ExpectedRevision = 2
	if result, err := q.Record(context.Background(), input); err != nil || result.Events != 0 || result.Revision != 2 {
		t.Fatalf("unchanged favorites replayed: %+v %v", result, err)
	}
	removal := addition
	removal.Kind = update.FavoriteRemoved
	input = poll(removal)
	input.ExpectedRevision = 2
	input.Favorites = &baseline
	if _, err := q.Record(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	input = poll(addition)
	input.ExpectedRevision = 3
	input.Favorites = &changed
	if result, err := q.Record(context.Background(), input); err != nil || result.Events != 1 {
		t.Fatalf("later re-add was deduplicated away: %+v %v", result, err)
	}
	var additions int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM events WHERE kind='favorite_added'`).Scan(&additions); err != nil {
		t.Fatal(err)
	}
	if additions != 2 {
		t.Fatalf("got %d favorite additions", additions)
	}
}

func TestFavoriteBaselineIsNotChangedByBackpressure(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxPending = 1
	s, q := outboxFixture(t, limits)
	favorites := []Favorite{{Subject: update.Subject{Kind: update.AnimeSubject, ID: "42"}}}
	event := notification("anime:42", "42", 101)
	event.Stream = update.Favorites
	event.Kind = update.FavoriteAdded
	input := poll(event)
	input.Favorites = &favorites
	if _, err := q.Record(context.Background(), input); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("expected backpressure, got %v", err)
	}
	if countRows(t, s, "favorites") != 0 {
		t.Fatal("favorite baseline changed without recording notification")
	}
}

func TestFavoriteMetadataEncodingCannotBypassPayloadLimit(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxPayloadBytes = 64
	s, q := outboxFixture(t, limits)
	favorites := []Favorite{{
		Subject: update.Subject{Kind: update.AnimeSubject, ID: "42"},
		Extra:   json.RawMessage(`{"text":"` + strings.Repeat("<", 20) + `"}`),
	}}
	input := poll()
	input.Favorites = &favorites
	if _, err := q.Record(context.Background(), input); !errors.Is(err, ErrPollBudget) {
		t.Fatalf("encoded favorite metadata exceeded its payload limit: %v", err)
	}
	if countRows(t, s, "favorites") != 0 {
		t.Fatal("oversized encoded metadata changed the favorite baseline")
	}
}

func TestFavoriteSnapshotBudgetIsCheckedBeforeDecoding(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxBatchBytes = 64
	_, q := outboxFixture(t, limits)
	favorites := []Favorite{{
		Subject: update.Subject{Kind: update.AnimeSubject, ID: "42"},
		Extra:   json.RawMessage(strings.Repeat("x", 128)),
	}}
	input := poll()
	input.Favorites = &favorites
	if _, err := q.Record(context.Background(), input); !errors.Is(err, ErrPollBudget) {
		t.Fatalf("favorite metadata was decoded beyond the retained batch budget: %v", err)
	}
}

func TestRecordRejectsInvalidDataAndIncompatibleProviderStreams(t *testing.T) {
	s, q := outboxFixture(t, Limits{})
	for _, input := range []Poll{
		{AccountID: 0, ExpectedRevision: 0, Now: time.Unix(200, 0)},
		{AccountID: 1, ExpectedRevision: -1, Now: time.Unix(200, 0)},
		{AccountID: 2, Events: []update.Event{notification("1", "42", 101)}, Now: time.Unix(200, 0)},
		{AccountID: 1, Observations: []update.Observation{{Stream: update.Favorites, SourceID: "invalid", Value: 1}}, Now: time.Unix(200, 0)},
	} {
		if _, err := q.Record(context.Background(), input); err == nil {
			t.Fatal("invalid poll accepted")
		}
	}
	if countRows(t, s, "events") != 0 {
		t.Fatal("invalid data reached the outbox")
	}
}

func TestCanceledPollDoesNotPersistWork(t *testing.T) {
	s, q := outboxFixture(t, Limits{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := q.Record(ctx, poll(notification("1", "42", 101))); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if countRows(t, s, "events") != 0 {
		t.Fatal("canceled poll persisted work")
	}
}

func TestConcurrentPollsCannotBothConsumeOneSnapshot(t *testing.T) {
	s, q := outboxFixture(t, Limits{})
	const workers = 8
	results := make(chan error, workers)
	start := make(chan struct{})
	var group sync.WaitGroup
	for range workers {
		group.Go(func() {
			<-start
			_, err := q.Record(context.Background(), poll(notification("1", "42", 101)))
			results <- err
		})
	}
	close(start)
	group.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrStalePoll) {
			t.Fatal(err)
		}
	}
	if successes != 1 || countRows(t, s, "events") != 1 {
		t.Fatal("concurrent polls consumed the same snapshot twice")
	}
}

func TestConcurrentSettingsAndPollWritesDoNotLoseSettings(t *testing.T) {
	s, q := outboxFixture(t, Limits{})
	start := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		<-start
		_, err := q.Record(context.Background(), poll(notification("1", "42", 101)))
		result <- err
	}()
	close(start)
	if _, err := s.DB().Exec(`UPDATE accounts SET features=0,revision=revision+1 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil && !errors.Is(err, ErrStalePoll) {
		t.Fatal(err)
	}
	var features int64
	if err := s.DB().QueryRow(`SELECT features FROM accounts WHERE id=1`).Scan(&features); err != nil {
		t.Fatal(err)
	}
	if features != 0 {
		t.Fatal("poll overwrote concurrent settings")
	}
}
