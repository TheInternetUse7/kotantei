package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMembershipReconciliationRemovesOnlyPositivelyAbsentUsers(t *testing.T) {
	_, q := schedulerFixture(t)
	var calls int
	r, err := NewReconciler(q, func(ctx context.Context, guild, user string) (bool, error) {
		calls++
		if user == "1" {
			return false, nil
		}
		return false, errors.New("blocked API")
	}, ReconcileOptions{RequestInterval: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	counts, err := r.Cycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	members, err := q.Memberships(context.Background(), "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(members) != 1 || members[0].UserID != "2" || counts.Failures != 1 || counts.Removed != 1 {
		t.Fatal("outage removed an unverified member or confirmed departure survived", calls, counts, members)
	}
}

func TestMembershipReconciliationCancellationDoesNotDeleteUnverifiedSubscriptions(t *testing.T) {
	_, q := schedulerFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, err := NewReconciler(q, func(ctx context.Context, guild, user string) (bool, error) { cancel(); return false, ctx.Err() }, ReconcileOptions{RequestInterval: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Cycle(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("reconciliation lost cancellation", err)
	}
	members, err := q.Memberships(context.Background(), "", "", 100)
	if err != nil || len(members) != 2 {
		t.Fatal("cancellation removed a live subscription", members, err)
	}
}
