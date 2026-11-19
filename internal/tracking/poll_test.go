package tracking

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"kotantei/internal/store"
	"kotantei/internal/update"
)

func testOutbox(t *testing.T) (*store.Store, *store.Outbox) {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "poll.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.DB().Exec(`INSERT INTO users(id) VALUES ('1'); INSERT INTO guilds(id,channel_id) VALUES ('10','11');
		INSERT INTO guild_users(guild_id,user_id) VALUES ('10','1');
		INSERT INTO accounts(id,provider,external_id,user_id) VALUES (1,'anilist','101','1');
		INSERT INTO checkpoints(account_id,kind,value) VALUES (1,'activity',100)`); err != nil {
		t.Fatal(err)
	}
	q, err := store.NewOutbox(s, store.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	return s, q
}

func listEvent(source, subject string, seconds int64) update.Event {
	return update.Event{Stream: update.Activity, Kind: update.List, SourceID: source, Subject: update.Subject{Kind: update.AnimeSubject, ID: subject}, OccurredAt: time.Unix(seconds, 0).UTC(), Payload: json.RawMessage(`{"title":"Example"}`)}
}

func TestCheckCollectsAllDescendingPagesBeforeAdvancingTheCheckpoint(t *testing.T) {
	s, q := testOutbox(t)
	pages := map[string]Page{
		"":       {Events: []update.Event{listEvent("3", "43", 103)}, Next: "second"},
		"second": {Events: []update.Event{listEvent("2", "42", 102)}, Next: "third"},
		"third":  {Events: []update.Event{listEvent("1", "41", 101), listEvent("4", "44", 103)}},
	}
	var requested []string
	result, err := Check(context.Background(), q, 1, time.Unix(200, 0), func(ctx context.Context, request Request) (Page, error) {
		requested = append(requested, request.Token)
		if request.Snapshot.Cursors[update.Activity].Value != 100 {
			t.Fatal("cursor advanced during pagination")
		}
		return pages[request.Token], nil
	}, CollectLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Events != 4 || result.Deliveries != 4 || !reflect.DeepEqual(requested, []string{"", "second", "third"}) {
		t.Fatalf("incomplete collection: %+v %v", result, requested)
	}
	state, err := q.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if state.Cursors[update.Activity].Value != 103 || !reflect.DeepEqual(state.Cursors[update.Activity].SourceIDs, []string{"3", "4"}) {
		t.Fatalf("timestamp ties across pages were lost: %+v", state.Cursors)
	}
	rows, err := s.DB().Query(`SELECT source_id FROM events ORDER BY occurred_at,id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var order []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		order = append(order, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"1", "2", "3", "4"}) {
		t.Fatalf("wrong chronological storage order: %v", order)
	}
}

func TestFailedLaterPageDoesNotCommitAnEarlierPage(t *testing.T) {
	s, q := testOutbox(t)
	upstream := errors.New("upstream unavailable")
	_, err := Check(context.Background(), q, 1, time.Unix(200, 0), func(ctx context.Context, request Request) (Page, error) {
		if request.Token == "" {
			return Page{Events: []update.Event{listEvent("3", "43", 103)}, Next: "later"}, nil
		}
		return Page{}, upstream
	}, CollectLimits{})
	if !errors.Is(err, upstream) {
		t.Fatalf("lost upstream error: %v", err)
	}
	var count int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM events`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	state, err := q.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 || state.Revision != 0 || state.Cursors[update.Activity].Value != 100 {
		t.Fatal("partial collection committed a high-water checkpoint")
	}
}

func TestRepeatedPageTokensAreRejectedWithoutAdvancing(t *testing.T) {
	_, q := testOutbox(t)
	_, err := Check(context.Background(), q, 1, time.Unix(200, 0), func(context.Context, Request) (Page, error) {
		return Page{Events: []update.Event{listEvent("1", "42", 101)}, Next: "repeat"}, nil
	}, CollectLimits{})
	if !errors.Is(err, ErrPaginationCycle) {
		t.Fatalf("expected cycle error, got %v", err)
	}
	state, err := q.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if state.Revision != 0 {
		t.Fatal("pagination cycle advanced the cursor")
	}
}

func TestCollectionBoundsPagesRecordsAndPayloadBytes(t *testing.T) {
	for _, configure := range []func(*CollectLimits){func(l *CollectLimits) { l.MaxPages = 1 }, func(l *CollectLimits) { l.MaxRecords = 1 }, func(l *CollectLimits) { l.MaxPayloadBytes = 1 }, func(l *CollectLimits) { l.MaxBytes = 1 }} {
		_, q := testOutbox(t)
		limits := DefaultCollectLimits()
		configure(&limits)
		_, err := Check(context.Background(), q, 1, time.Unix(200, 0), func(ctx context.Context, request Request) (Page, error) {
			if request.Token == "" {
				return Page{Events: []update.Event{listEvent("2", "43", 102)}, Next: "last"}, nil
			}
			return Page{Events: []update.Event{listEvent("1", "42", 101)}}, nil
		}, limits)
		if !errors.Is(err, ErrCollectionBudget) {
			t.Fatalf("expected budget failure, got %v", err)
		}
		state, err := q.Snapshot(context.Background(), 1)
		if err != nil {
			t.Fatal(err)
		}
		if state.Revision != 0 {
			t.Fatal("bounded collection advanced a checkpoint without complete data")
		}
	}
}

func TestCancelBetweenPagesDiscardsCollectedWork(t *testing.T) {
	_, q := testOutbox(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := Check(ctx, q, 1, time.Unix(200, 0), func(context.Context, Request) (Page, error) {
		cancel()
		return Page{Events: []update.Event{listEvent("1", "42", 101)}, Next: "later"}, nil
	}, CollectLimits{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	state, err := q.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if state.Revision != 0 {
		t.Fatal("canceled fetch was committed")
	}
}

func TestEachPageReceivesAnUnchangedSnapshot(t *testing.T) {
	state := store.Snapshot{AccountID: 1, Provider: "anilist", Revision: 0, Cursors: map[update.Stream]update.Cursor{update.Activity: {Value: 100, SourceIDs: []string{"old"}}}}
	result, err := Collect(context.Background(), state, func(ctx context.Context, request Request) (Page, error) {
		if request.Snapshot.Cursors[update.Activity].Value != 100 || request.Snapshot.Cursors[update.Activity].SourceIDs[0] != "old" {
			t.Fatal("page fetch mutated the captured snapshot")
		}
		request.Snapshot.Cursors[update.Activity] = update.Cursor{Value: 999}
		if request.Token == "" {
			return Page{Next: "last"}, nil
		}
		return Page{}, nil
	}, CollectLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExpectedRevision != 0 || state.Cursors[update.Activity].Value != 100 {
		t.Fatal("collector changed account state")
	}
}

func TestPageFetchCannotChangeCapturedColorSettings(t *testing.T) {
	state := store.Snapshot{AccountID: 1, Colors: []store.ColorOverride{{UpdateType: 0, ColorValue: 123}}}
	_, err := Collect(context.Background(), state, func(ctx context.Context, request Request) (Page, error) {
		if request.Snapshot.Colors[0].ColorValue != 123 {
			t.Fatal("page fetch changed captured color settings")
		}
		request.Snapshot.Colors[0].ColorValue = 999
		if request.Token == "" {
			return Page{Next: "last"}, nil
		}
		return Page{}, nil
	}, CollectLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if state.Colors[0].ColorValue != 123 {
		t.Fatal("collector changed the caller's colors")
	}
}

func TestCollectionRejectsMissingFetcherAndInvalidLimits(t *testing.T) {
	state := store.Snapshot{AccountID: 1}
	if _, err := Collect(context.Background(), state, nil, CollectLimits{}); err == nil {
		t.Fatal("missing fetcher accepted")
	}
	limits := DefaultCollectLimits()
	limits.MaxPages = -1
	if _, err := Collect(context.Background(), state, func(context.Context, Request) (Page, error) { return Page{}, nil }, limits); err == nil {
		t.Fatal("invalid collection limits accepted")
	}
}

func TestCollectorCopiesPayloadsFromAReusedPageBuffer(t *testing.T) {
	buffer := json.RawMessage(`{"value":1}`)
	event := listEvent("1", "42", 101)
	event.Payload = buffer
	result, err := Collect(context.Background(), store.Snapshot{AccountID: 1}, func(ctx context.Context, request Request) (Page, error) {
		if request.Token == "" {
			return Page{Events: []update.Event{event}, Next: "last"}, nil
		}
		copy(buffer, []byte(`{"value":2}`))
		event.SourceID = "2"
		event.Subject.ID = "43"
		return Page{Events: []update.Event{event}}, nil
	}, CollectLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Events[0].Payload) != `{"value":1}` || string(result.Events[1].Payload) != `{"value":2}` {
		t.Fatal("a later fetch overwrote a collected payload")
	}
}
