package store

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"kotantei/internal/update"
)

func TestGuildConfigurationRequiresExplicitCreateOrUpdate(t *testing.T) {
	s, q := outboxFixture(t, Limits{})
	ctx := context.Background()
	if err := q.ConfigureGuild(ctx, "40", "41", false); err != nil {
		t.Fatal(err)
	}
	if err := q.ConfigureGuild(ctx, "40", "42", false); !errors.Is(err, ErrGuildAlreadyConfigured) {
		t.Fatal("create silently replaced a channel", err)
	}
	if err := q.ConfigureGuild(ctx, "999", "42", true); !errors.Is(err, ErrGuildNotConfigured) {
		t.Fatal("update implicitly created a server", err)
	}
	if err := q.ConfigureGuild(ctx, "40", "41", true); !errors.Is(err, ErrUnchangedChannel) {
		t.Fatal("unchanged channel accepted", err)
	}
	if err := q.ConfigureGuild(ctx, "40", "42", true); err != nil {
		t.Fatal(err)
	}
	status, err := q.GuildStatus(ctx, "40")
	if err != nil || status.ChannelID != "42" {
		t.Fatal(status, err)
	}
	if countRows(t, s, "guilds") != 4 {
		t.Fatal("configuration changed other servers")
	}
}

func TestChannelChangeCancelsUnsentOldTargetsWithoutReplayingAcknowledgedPosts(t *testing.T) {
	s, q := outboxFixture(t, Limits{})
	ctx := context.Background()
	at := time.Now().UTC()
	if _, err := q.Record(ctx, poll(notification("1", "42", 101))); err != nil {
		t.Fatal(err)
	}
	claimed, err := q.Claim(ctx, at, 30*time.Second)
	if err != nil || claimed == nil {
		t.Fatal(err)
	}
	if err := q.Acknowledge(ctx, claimed.Ref(), "1001", at); err != nil {
		t.Fatal(err)
	}
	if err := q.ConfigureGuild(ctx, "20", "22", true); err != nil {
		t.Fatal(err)
	}
	status, err := q.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Sent != 1 || status.Canceled != 1 || status.Pending != 0 || countRows(t, s, "deliveries") != 2 {
		t.Fatal("channel switch replayed or retargeted old posts", status)
	}
	input := poll(notification("2", "43", 102))
	input.ExpectedRevision = 1
	if _, err := q.Record(ctx, input); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM deliveries WHERE channel_id='22'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("new updates did not use the new channel", err)
	}
}

func TestRemoveHereIsSharedAcrossProvidersAndPreservesAccountBaselines(t *testing.T) {
	s, q := outboxFixture(t, Limits{})
	ctx := context.Background()
	if _, err := q.Record(ctx, poll(notification("1", "42", 101))); err != nil {
		t.Fatal(err)
	}
	malPoll := Poll{AccountID: 2, Now: time.Unix(200, 0), Events: []update.Event{{Stream: update.Anime, Kind: update.List, SourceID: "42:101000000:state", Subject: update.Subject{Kind: update.AnimeSubject, ID: "42"}, OccurredAt: time.Unix(101, 0), Payload: notification("1", "42", 101).Payload}}}
	if _, err := q.Record(ctx, malPoll); err != nil {
		t.Fatal(err)
	}
	before, err := q.Snapshot(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.DisconnectUser(ctx, "1", "10"); err != nil {
		t.Fatal(err)
	}
	after, err := q.Snapshot(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) || countRows(t, s, "accounts") != 2 {
		t.Fatal("server removal reset linked provider state")
	}
	status, err := q.Status(ctx)
	if err != nil || status.Canceled != 2 || status.Pending != 2 {
		t.Fatal("removehere was provider-specific or affected another server", status, err)
	}
	if err := q.DisconnectUser(ctx, "1", "10"); !errors.Is(err, ErrMembershipNotFound) {
		t.Fatal("missing subscription was not reported", err)
	}
}

func TestSubscriptionChangesPreserveActiveChannelLeases(t *testing.T) {
	_, q := outboxFixture(t, Limits{})
	ctx := context.Background()
	at := time.Now().UTC()
	if _, err := q.Record(ctx, poll(notification("1", "42", 101))); err != nil {
		t.Fatal(err)
	}
	active, err := q.Claim(ctx, at, time.Minute)
	if err != nil || active == nil {
		t.Fatal(err)
	}
	if err := q.DisconnectUser(ctx, "1", "10"); err != nil {
		t.Fatal(err)
	}
	status, err := q.Status(ctx)
	if err != nil || status.Leased != 1 || status.Pending != 2 {
		t.Fatal("removal revoked a request already in flight", status, err)
	}
	if err := q.Acknowledge(ctx, active.Ref(), "1001", at); err != nil {
		t.Fatal("active receipt was rejected", err)
	}
}

func TestRemoveAccountIsGlobalForOnlyTheChosenProvider(t *testing.T) {
	s, q := outboxFixture(t, Limits{})
	ctx := context.Background()
	if _, err := q.Record(ctx, poll(notification("1", "42", 101))); err != nil {
		t.Fatal(err)
	}
	if err := q.RemoveAccount(ctx, "1", "anilist"); err != nil {
		t.Fatal(err)
	}
	if _, err := q.ForUser(ctx, "1", "anilist"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatal("removed account still tracked", err)
	}
	if _, err := q.ForUser(ctx, "1", "mal"); err != nil {
		t.Fatal("removal deleted the other provider", err)
	}
	if countRows(t, s, "events") != 0 || countRows(t, s, "deliveries") != 0 || countRows(t, s, "guild_users") != 3 {
		t.Fatal("global provider removal left posts or destroyed shared membership")
	}
}

func TestUnlinkRelinkCannotResurrectAnInflightSnapshot(t *testing.T) {
	s, q := outboxFixture(t, Limits{})
	ctx := context.Background()
	linked, err := q.Connect(ctx, baselineLink())
	if err != nil {
		t.Fatal(err)
	}
	if err := q.RemoveAccount(ctx, "3", "anilist"); err != nil {
		t.Fatal(err)
	}
	input := baselineLink()
	input.ExternalID = "304"
	again, err := q.Connect(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if again.AccountID <= linked.AccountID {
		t.Fatal("SQLite reused a deleted account ID, making old polls appear current")
	}
	stale := poll(notification("30", "43", 201))
	stale.AccountID = linked.AccountID
	if _, err := q.Record(ctx, stale); !errors.Is(err, ErrAccountNotFound) {
		t.Fatal("old poll attached to a different relinked account", err)
	}
	if countRows(t, s, "events") != 0 {
		t.Fatal("old account data was posted after relinking")
	}
}

func TestSettingsWritesIncrementOnlyActualChangesAndRejectStalePolls(t *testing.T) {
	_, q := outboxFixture(t, Limits{})
	ctx := context.Background()
	if err := q.SetFeature(ctx, "1", "anilist", 4096, true); err != nil {
		t.Fatal(err)
	}
	if err := q.SetFeature(ctx, "1", "anilist", 4096, true); err != nil {
		t.Fatal(err)
	}
	state, err := q.Settings(ctx, "1", "anilist")
	if err != nil {
		t.Fatal(err)
	}
	if state.Features != 4223 || state.Revision != 1 {
		t.Fatal("feature bit or revision was not preserved", state)
	}
	if _, err := q.Record(ctx, poll(notification("1", "42", 101))); !errors.Is(err, ErrStalePoll) {
		t.Fatal("settings changes accepted a stale poll", err)
	}
	color := uint32(0xaabbcc)
	if err := q.SetColor(ctx, "1", "anilist", 14, &color); err != nil {
		t.Fatal(err)
	}
	if err := q.SetColor(ctx, "1", "anilist", 14, &color); err != nil {
		t.Fatal(err)
	}
	state, err = q.Settings(ctx, "1", "anilist")
	if err != nil {
		t.Fatal(err)
	}
	if state.Revision != 2 || !reflect.DeepEqual(state.Colors, []ColorOverride{{UpdateType: 14, ColorValue: color}}) {
		t.Fatal("color revision/value lost", state)
	}
	state.Colors[0].ColorValue = 0
	if err := q.SetColor(ctx, "1", "anilist", 14, nil); err != nil {
		t.Fatal(err)
	}
	if err := q.SetFeature(ctx, "1", "anilist", 4096, false); err != nil {
		t.Fatal(err)
	}
	state, err = q.Settings(ctx, "1", "anilist")
	if err != nil || state.Features != 127 || state.Revision != 4 || len(state.Colors) != 0 {
		t.Fatal("reset changed unrelated settings", state, err)
	}
}

func TestCommandSettingsPreserveUnknownImportedBitsAndColors(t *testing.T) {
	s, q := outboxFixture(t, Limits{})
	ctx := context.Background()
	if _, err := s.DB().Exec(`UPDATE accounts SET features=features|1048576,colors='[{"ColorValue":123,"UpdateType":255}]' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err := q.SetFeature(ctx, "1", "anilist", 1, false); err != nil {
		t.Fatal(err)
	}
	color := uint32(321)
	if err := q.SetColor(ctx, "1", "anilist", 0, &color); err != nil {
		t.Fatal(err)
	}
	state, err := q.Settings(ctx, "1", "anilist")
	if err != nil {
		t.Fatal(err)
	}
	if state.Features != 1048702 || len(state.Colors) != 2 || state.Colors[1].UpdateType != 255 {
		t.Fatal("editing supported settings discarded imported values", state)
	}
}

func TestManagementRejectsInvalidIDsFlagsColorsAndExhaustedRevisions(t *testing.T) {
	s, q := outboxFixture(t, Limits{})
	ctx := context.Background()
	for _, call := range []func() error{
		func() error { return q.ConfigureGuild(ctx, "0", "41", false) }, func() error { return q.ConfigureGuild(ctx, "40", "invalid", false) },
		func() error { return q.SetFeature(ctx, "1", "unknown", 1, true) }, func() error { return q.SetFeature(ctx, "1", "anilist", 3, true) },
		func() error { return q.SetFeature(ctx, "1", "anilist", 65536, true) }, func() error { return q.SetColor(ctx, "1", "mal", 14, nil) },
		func() error { color := uint32(0x1000000); return q.SetColor(ctx, "1", "anilist", 0, &color) },
		func() error { return q.DisconnectUser(ctx, "00", "10") }, func() error { return q.RemoveAccount(ctx, "1", "unknown") },
	} {
		if err := call(); err == nil {
			t.Fatal("invalid management input accepted")
		}
	}
	if _, err := s.DB().Exec(`UPDATE accounts SET revision=? WHERE id=1`, int64(math.MaxInt64)); err != nil {
		t.Fatal(err)
	}
	if err := q.SetFeature(ctx, "1", "anilist", 4096, true); err == nil {
		t.Fatal("revision overflow was accepted")
	}
}

func TestAccountAndMembershipPaginationIsBoundedAndOnlyIncludesLiveSubscriptions(t *testing.T) {
	s, q := outboxFixture(t, Limits{})
	ctx := context.Background()
	if _, err := q.Connect(ctx, baselineLink()); err != nil {
		t.Fatal(err)
	}
	fence, err := q.AccountFence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := q.ActiveAccountIDs(ctx, "anilist", 0, fence, 1)
	if err != nil || !reflect.DeepEqual(ids, []int64{1}) {
		t.Fatal(ids, err)
	}
	next, err := q.ActiveAccountIDs(ctx, "anilist", 1, fence, 1)
	if err != nil || len(next) != 1 || next[0] <= 1 {
		t.Fatal(next, err)
	}
	accounts, more, err := q.ListAccounts(ctx, "10", "anilist", 0, 1)
	if err != nil || len(accounts) != 1 || !more {
		t.Fatal(accounts, more, err)
	}
	members, err := q.Memberships(ctx, "", "", 1)
	if err != nil || len(members) != 1 || members[0].GuildID != "10" {
		t.Fatal(members, err)
	}
	if _, err := s.DB().Exec(`DELETE FROM guild_users WHERE user_id='3'`); err != nil {
		t.Fatal(err)
	}
	ids, err = q.ActiveAccountIDs(ctx, "anilist", 1, fence, 100)
	if err != nil || len(ids) != 0 {
		t.Fatal("orphan accounts were polled", ids, err)
	}
	if _, err := q.ActiveAccountIDs(ctx, "anilist", 0, fence, 1001); err == nil {
		t.Fatal("unbounded account query allowed")
	}
	if _, err := q.Memberships(ctx, "", "", 1001); err == nil {
		t.Fatal("unbounded membership query allowed")
	}
}

func TestReconciliationCannotDeleteAReassertedMembership(t *testing.T) {
	_, q := outboxFixture(t, Limits{})
	ctx := context.Background()
	input := baselineLink()
	if _, err := q.Connect(ctx, input); err != nil {
		t.Fatal(err)
	}
	members, err := q.Memberships(ctx, "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	var old Membership
	for _, member := range members {
		if member.UserID == "3" {
			old = member
		}
	}
	if old.UserID == "" {
		t.Fatal("membership was not returned")
	}
	if err := q.DisconnectUser(ctx, "3", "10"); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Connect(ctx, input); err != nil {
		t.Fatal(err)
	}
	removed, err := q.DisconnectIfCurrent(ctx, old)
	if err != nil || removed {
		t.Fatal("stale membership check deleted a relinked subscription", removed, err)
	}
	members, err = q.Memberships(ctx, "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range members {
		if member.UserID == "3" {
			removed, err = q.DisconnectIfCurrent(ctx, member)
			if err != nil || !removed {
				t.Fatal("current verified departure was not removed", removed, err)
			}
			return
		}
	}
	t.Fatal("reasserted membership was lost")
}

func TestGuildRemovalAndBoundedOrphanCleanupDoNotDeleteOtherServers(t *testing.T) {
	s, q := outboxFixture(t, Limits{})
	ctx := context.Background()
	if err := q.RemoveGuild(ctx, "10"); err != nil {
		t.Fatal(err)
	}
	if countRows(t, s, "guilds") != 2 || countRows(t, s, "accounts") != 2 {
		t.Fatal("server removal globally unlinked users")
	}
	if err := q.RemoveGuild(ctx, "20"); err != nil {
		t.Fatal(err)
	}
	count, err := q.Cleanup(ctx, 1)
	if err != nil || count != 1 || countRows(t, s, "accounts") != 0 || countRows(t, s, "users") != 1 {
		t.Fatal("orphan cleanup was not bounded or removed live members", count, err)
	}
}

func TestManagementRollsBackCanceledOrRejectedWrites(t *testing.T) {
	s, q := outboxFixture(t, Limits{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := q.ConfigureGuild(ctx, "40", "41", false); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost", err)
	}
	if _, err := s.DB().Exec(`CREATE TRIGGER reject_setting BEFORE UPDATE OF features ON accounts BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if err := q.SetFeature(context.Background(), "1", "anilist", 4096, true); err == nil {
		t.Fatal("rejected setting write succeeded")
	}
	state, err := q.Snapshot(context.Background(), 1)
	if err != nil || state.Revision != 0 || state.Features != 127 {
		t.Fatal("rejected write partly changed revision", state, err)
	}
}
