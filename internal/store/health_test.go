package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHealthInspectionReportsOnlyStorageAndProviderAggregates(t *testing.T) {
	s, q := outboxFixture(t, Limits{})
	if _, err := s.DB().Exec(`UPDATE accounts SET username='DO-NOT-PRINT-NAME'; DELETE FROM guild_users WHERE user_id='1' AND guild_id='20'`); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Record(context.Background(), poll(notification("1", "42", 101))); err != nil {
		t.Fatal(err)
	}
	health, err := InspectHealth(context.Background(), s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if health.SchemaVersion != 3 || health.Summary.Accounts != 2 || health.Outbox.Pending != 1 || len(health.Providers) != 2 {
		t.Fatal("health aggregates incomplete", health)
	}
	if health.Providers[0].Name != "anilist" || health.Providers[0].Accounts != 1 || health.Providers[0].SubscribedAccounts != 1 || health.Providers[0].LatestCheckpointUnix != 101 {
		t.Fatal("incorrect AniList health aggregates", health.Providers)
	}
	if health.Providers[1].Name != "mal" || health.Providers[1].LatestCheckpointUnix != 100 {
		t.Fatal("MAL checkpoint units changed", health.Providers)
	}
	encoded, err := json.Marshal(health)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "DO-NOT-PRINT") || strings.Contains(string(encoded), "payload") || strings.Contains(string(encoded), "lease_token") {
		t.Fatal("health exposed account or delivery data", string(encoded))
	}
}

func TestHealthInspectionDoesNotCreateOrMigrateStorage(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.db")
	if _, err := InspectHealth(context.Background(), missing); err == nil {
		t.Fatal("missing health database was accepted")
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("health created storage", err)
	}
	path := versionOneDatabase(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InspectHealth(context.Background(), path); !errors.Is(err, ErrNeedsMigration) {
		t.Fatal("old delivery schema was not reported", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("read-only health migrated storage", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := InspectHealth(ctx, path); !errors.Is(err, context.Canceled) {
		t.Fatal("health ignored cancellation", err)
	}
}
