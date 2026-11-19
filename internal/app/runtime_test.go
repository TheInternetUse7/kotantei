package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"kotantei/internal/bot"
	"kotantei/internal/config"
	"kotantei/internal/store"
	"kotantei/internal/update"
)

func runtimeEnv(database, legacy, token, client string) func(string) (string, bool) {
	base := envFor(database, legacy)
	return func(key string) (string, bool) {
		switch key {
		case "KOTANTEI_DISCORD_TOKEN":
			return token, true
		case "KOTANTEI_MAL_CLIENT_ID":
			return client, true
		}
		return base(key)
	}
}

func TestRunValidatesCredentialsBeforeCreatingStorageOrStartingNetworkWork(t *testing.T) {
	for _, credentials := range [][2]string{{"", "test-client"}, {"test-token", ""}, {"DO-NOT-PRINT\nheader", "test-client"}, {"test-token", "DO-NOT-PRINT\nheader"}} {
		path := filepath.Join(t.TempDir(), "new-directory", "kotantei.db")
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{"run"}, &stdout, &stderr, runtimeEnv(path, "", credentials[0], credentials[1]), func(context.Context, *store.Outbox, config.Config, bot.RuntimeOptions) error {
			t.Error("invalid credentials started the runtime")
			return nil
		})
		if code != 78 || strings.Contains(stderr.String(), "DO-NOT-PRINT") {
			t.Fatal("malformed credentials were not a safe configuration error", code, stderr.String())
		}
		if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("credential validation created storage", err)
		}
	}
}

func TestRunImportsLegacyAccountsBeforeAnyRuntimeWorkAndPreservesTheSource(t *testing.T) {
	dir := t.TempDir()
	source, target := filepath.Join(dir, "data.db"), filepath.Join(dir, "kotantei.db")
	createEmptyLegacy(t, source)
	db, err := sql.Open("sqlite", source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO DiscordUsers VALUES (1); INSERT INTO DiscordGuilds VALUES (10,11); INSERT INTO DiscordGuildDiscordUser VALUES (10,1); INSERT INTO AniListUsers VALUES (101,1,4223,100,90)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	for range 2 {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{"run"}, &stdout, &stderr, runtimeEnv(target, source, "test-token", "test-client"), func(ctx context.Context, q *store.Outbox, cfg config.Config, _ bot.RuntimeOptions) error {
			calls.Add(1)
			state, err := q.ForUser(ctx, "1", "anilist")
			if err != nil || state.ExternalID != "101" || state.Features != 4223 || state.Cursors[update.Activity].Value != 100 || state.Cursors[update.ReviewStream].Value != 90 {
				t.Error("runtime preceded a complete import or reset baselines", state, err)
			}
			return nil
		})
		if code != 0 {
			t.Fatal("runtime import failed", code, stderr.String())
		}
	}
	after, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(before, after) || calls.Load() != 2 {
		t.Fatal("runtime import rewrote source or was not restart-safe", err, calls.Load())
	}
}

func TestRunRejectsUnsupportedLegacyInputBeforeTheRuntimeStarts(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "data.db")
	if err := os.WriteFile(source, []byte("not a supported database"), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"run"}, &stdout, &stderr, runtimeEnv(filepath.Join(dir, "kotantei.db"), source, "test-token", "test-client"), func(context.Context, *store.Outbox, config.Config, bot.RuntimeOptions) error {
		t.Error("unsupported legacy input started Discord/provider work")
		return nil
	})
	if code != 1 {
		t.Fatal("unsupported input did not fail startup", code, stderr.String())
	}
}

func TestRunDoesNotOverwritePopulatedNativeStorageWithDroppedLegacyInput(t *testing.T) {
	dir := t.TempDir()
	target, source := filepath.Join(dir, "kotantei.db"), filepath.Join(dir, "data.db")
	s, err := store.Open(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO users(id) VALUES ('1'); INSERT INTO guilds(id,channel_id) VALUES ('10','11'); INSERT INTO guild_users(guild_id,user_id) VALUES ('10','1'); INSERT INTO accounts(id,provider,external_id,user_id) VALUES (1,'anilist','101','1'); INSERT INTO checkpoints(account_id,kind,value) VALUES (1,'activity',500)`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	createEmptyLegacy(t, source)
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"run"}, &stdout, &stderr, runtimeEnv(target, source, "test-token", "test-client"), func(ctx context.Context, q *store.Outbox, _ config.Config, _ bot.RuntimeOptions) error {
		state, err := q.ForUser(ctx, "1", "anilist")
		if err != nil || state.Cursors[update.Activity].Value != 500 {
			t.Error("native state was overwritten", state, err)
		}
		return nil
	})
	if code != 0 {
		t.Fatal("a populated native database was blocked by optional legacy auto-import", code, stderr.String())
	}
}

func TestRunGracefulCancellationReturnsSuccessButIndependentRuntimeFailureDoesNot(t *testing.T) {
	for _, graceful := range []bool{true, false} {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var stdout, stderr bytes.Buffer
		code := run(ctx, []string{"run"}, &stdout, &stderr, runtimeEnv(filepath.Join(t.TempDir(), "kotantei.db"), "", "test-token", "test-client"), func(context.Context, *store.Outbox, config.Config, bot.RuntimeOptions) error {
			if graceful {
				cancel()
				return context.Canceled
			}
			return bot.ErrDiscordAuth
		})
		if (graceful && code != 0) || (!graceful && code != 1) {
			t.Fatal("runtime exit status lost graceful/failure distinction", code, stderr.String())
		}
	}
}

func TestHealthCLIIsReadOnlyCredentialFreeAndDoesNotClaimRuntimeLiveness(t *testing.T) {
	path := blockedOutbox(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"health"}, &stdout, &stderr, envFor(path, "")); code != 0 {
		t.Fatal("health required credentials or failed", code, stderr.String())
	}
	var report struct {
		Health struct {
			Runtime   string               `json:"runtime"`
			Outbox    store.DeliveryStatus `json:"outbox"`
			Providers []struct {
				Name               string `json:"name"`
				Enabled            bool   `json:"enabled"`
				SubscribedAccounts int    `json:"subscribed_accounts"`
			} `json:"providers"`
		} `json:"health"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Health.Runtime != "unobserved" || report.Health.Outbox.Failed != 1 || len(report.Health.Providers) != 2 || !report.Health.Providers[0].Enabled || report.Health.Providers[0].SubscribedAccounts != 1 {
		t.Fatal("CLI health report missing safe configuration/storage observations", stdout.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), "DO-NOT-PRINT") {
		t.Fatal("health exposed notification payloads")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("health changed storage", err)
	}
	missing := filepath.Join(t.TempDir(), "missing.db")
	stdout.Reset()
	stderr.Reset()
	if code := Run(context.Background(), []string{"health"}, &stdout, &stderr, envFor(missing, "")); code == 0 {
		t.Fatal("health accepted missing storage")
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("health created a missing database")
	}
}

func TestHelpListsRuntimeAndHealthWithoutRequiringConfiguration(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"help"}, &stdout, &stderr, nil); code != 0 || !strings.Contains(stdout.String(), "run") || !strings.Contains(stdout.String(), "health") || strings.Contains(stdout.String(), "not implemented") {
		t.Fatal("help still describes a maintenance-only executable", code, stdout.String())
	}
}
