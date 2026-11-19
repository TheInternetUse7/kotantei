package store

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"kotantei/internal/update"
)

func TestSnapshotIncludesIdentityAndImportedSettings(t *testing.T) {
	s, q := outboxFixture(t, Limits{})
	if _, err := s.DB().Exec(`UPDATE accounts SET username='Reader',features=4097,colors='[{"ColorValue":123,"UpdateType":5}]' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	state, err := q.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if state.ExternalID != "101" || state.UserID != "1" || state.Username != "Reader" || state.Features != 4097 || !reflect.DeepEqual(state.Colors, []ColorOverride{{ColorValue: 123, UpdateType: 5}}) {
		t.Fatalf("incomplete account snapshot: %+v", state)
	}
}

func baselineLink() AccountLink {
	return AccountLink{Provider: "anilist", ExternalID: "303", UserID: "3", GuildID: "10", Username: "Reader", Cursors: map[update.Stream]update.Cursor{
		update.Activity: {Value: 200, SourceIDs: []string{"9", "10"}}, update.ReviewStream: {Value: 200, SourceIDs: []string{"20"}},
	}, Favorites: []Favorite{{Subject: update.Subject{Kind: update.AnimeSubject, ID: "42"}}}}
}

func TestConnectSilentlyCommitsAccountAndCompleteBaselines(t *testing.T) {
	s, q := outboxFixture(t, Limits{})
	result, err := q.Connect(context.Background(), baselineLink())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created || result.AccountID <= 0 {
		t.Fatalf("account was not created: %+v", result)
	}
	state, err := q.ForUser(context.Background(), "3", "anilist")
	if err != nil {
		t.Fatal(err)
	}
	if state.AccountID != result.AccountID || state.Revision != 0 || state.Features != 127 || state.ExternalID != "303" || state.Username != "Reader" || len(state.Favorites) != 1 || !reflect.DeepEqual(state.Cursors[update.Activity].SourceIDs, []string{"10", "9"}) {
		t.Fatalf("wrong linked baseline: %+v", state)
	}
	if countRows(t, s, "events") != 0 || countRows(t, s, "deliveries") != 0 {
		t.Fatal("linking replayed historical updates")
	}
	var membership int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM guild_users WHERE user_id='3' AND guild_id='10'`).Scan(&membership); err != nil {
		t.Fatal(err)
	}
	if membership != 1 {
		t.Fatal("linking did not create a subscription")
	}
}

func TestConnectingExistingAccountPreservesLiveState(t *testing.T) {
	s, q := outboxFixture(t, Limits{})
	if _, err := s.DB().Exec(`UPDATE accounts SET revision=7,features=4097,colors='[{"ColorValue":123,"UpdateType":0}]' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	input := baselineLink()
	input.UserID = "1"
	input.ExternalID = "101"
	input.GuildID = "30"
	result, err := q.Connect(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if result.Created || result.AccountID != 1 {
		t.Fatal("existing account was replaced")
	}
	state, err := q.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if state.Revision != 7 || state.Features != 4097 || state.Cursors[update.Activity].Value != 100 || len(state.Favorites) != 0 || len(state.Colors) != 1 {
		t.Fatal("adding a server reset live account state")
	}
	if _, err := q.Connect(context.Background(), input); err != nil {
		t.Fatal("repeated connection was not idempotent")
	}
}

func TestConnectRejectsUnconfiguredServersAndAccountConflicts(t *testing.T) {
	s, q := outboxFixture(t, Limits{})
	input := baselineLink()
	input.GuildID = "999"
	if _, err := q.Connect(context.Background(), input); !errors.Is(err, ErrGuildNotConfigured) {
		t.Fatalf("unconfigured server accepted: %v", err)
	}
	input = baselineLink()
	input.UserID = "1"
	if _, err := q.Connect(context.Background(), input); !errors.Is(err, ErrAccountConflict) {
		t.Fatalf("existing user's account overwritten: %v", err)
	}
	input = baselineLink()
	input.ExternalID = "101"
	if _, err := q.Connect(context.Background(), input); !errors.Is(err, ErrAccountConflict) {
		t.Fatalf("one provider identity linked twice: %v", err)
	}
	if countRows(t, s, "users") != 2 || countRows(t, s, "accounts") != 2 {
		t.Fatal("failed linking partly persisted")
	}
}

func TestConnectRollsBackARejectedFavoriteWrite(t *testing.T) {
	s, q := outboxFixture(t, Limits{})
	if _, err := s.DB().Exec(`CREATE TRIGGER reject_baseline BEFORE INSERT ON favorites BEGIN SELECT RAISE(ABORT,'fixture failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Connect(context.Background(), baselineLink()); err == nil {
		t.Fatal("failed baseline was ignored")
	}
	if countRows(t, s, "users") != 2 || countRows(t, s, "accounts") != 2 || countRows(t, s, "guild_users") != 3 {
		t.Fatal("partial account baseline committed")
	}
}

func TestConnectValidatesCompleteBoundedBaseline(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxFavorites = 1
	s, q := outboxFixture(t, limits)
	for _, change := range []func(*AccountLink){
		func(a *AccountLink) { a.UserID = "0" }, func(a *AccountLink) { a.ExternalID = "invalid" }, func(a *AccountLink) { a.Provider = "unknown" },
		func(a *AccountLink) { delete(a.Cursors, update.ReviewStream) }, func(a *AccountLink) { a.Cursors[update.Anime] = update.Cursor{Value: 1} },
		func(a *AccountLink) { a.Cursors[update.Activity] = update.Cursor{Value: -1} },
		func(a *AccountLink) {
			a.Favorites = append(a.Favorites, Favorite{Subject: update.Subject{Kind: update.MangaSubject, ID: "43"}})
		},
	} {
		input := baselineLink()
		change(&input)
		if _, err := q.Connect(context.Background(), input); err == nil {
			t.Fatal("invalid account baseline accepted")
		}
	}
	if countRows(t, s, "users") != 2 {
		t.Fatal("invalid baseline changed storage")
	}
}

func TestConcurrentLinksCreateOneAccountWithoutRebaselining(t *testing.T) {
	s, q := outboxFixture(t, Limits{})
	const workers = 8
	results := make(chan error, workers)
	var group sync.WaitGroup
	for range workers {
		group.Go(func() { _, err := q.Connect(context.Background(), baselineLink()); results <- err })
	}
	group.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if countRows(t, s, "accounts") != 3 || countRows(t, s, "favorites") != 1 {
		t.Fatal("concurrent linking duplicated accounts or baselines")
	}
}

func TestRecordCommitsUsernameRecoveryWithoutChangingCheckpoints(t *testing.T) {
	_, q := outboxFixture(t, Limits{})
	name := "Renamed"
	input := poll()
	input.Username = &name
	result, err := q.Record(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	state, err := q.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if result.Revision != 1 || state.Username != name || state.Cursors[update.Activity].Value != 100 {
		t.Fatal("username recovery was lost or rewound the checkpoint")
	}
}

func TestAccountIDsRemainMonotonicAfterDeletionAndReopen(t *testing.T) {
	s, q := outboxFixture(t, Limits{})
	linked, err := q.Connect(context.Background(), baselineLink())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`DELETE FROM accounts WHERE id=?`, linked.AccountID); err != nil {
		t.Fatal(err)
	}
	path := s.Path()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	q, err = NewOutbox(reopened, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	again, err := q.Connect(context.Background(), baselineLink())
	if err != nil {
		t.Fatal(err)
	}
	if again.AccountID <= linked.AccountID {
		t.Fatalf("deleted account ID was reused after restart: %d -> %d", linked.AccountID, again.AccountID)
	}
}
