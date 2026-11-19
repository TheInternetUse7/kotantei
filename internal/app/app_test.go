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
	"testing"
	"time"

	"kotantei/internal/store"
	"kotantei/internal/update"
)

func TestHelpDoesNotRequireConfigurationOrCreateStorage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"help"}, &stdout, &stderr, nil); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "init") || !strings.Contains(stdout.String(), "import") || !strings.Contains(stdout.String(), "doctor") {
		t.Fatal("maintenance commands missing from help")
	}
}

func TestInitAndDoctorWorkWithoutCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kotantei.db")
	lookup := envFor(path, "")
	for _, command := range []string{"init", "doctor"} {
		var stdout, stderr bytes.Buffer
		if code := Run(context.Background(), []string{command}, &stdout, &stderr, lookup); code != 0 {
			t.Fatalf("%s: exit %d: %s", command, code, stderr.String())
		}
		var output map[string]any
		if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
			t.Fatalf("expected JSON report: %v", err)
		}
	}
}

func TestDoctorDoesNotCreateMissingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"doctor"}, &stdout, &stderr, envFor(path, "")); code == 0 {
		t.Fatal("doctor reported success for a missing database")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("doctor created missing storage")
	}
}

func TestInitAutomaticallyImportsConventionalLegacyInput(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "data.db")
	createEmptyLegacy(t, source)
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"init"}, &stdout, &stderr, envFor(filepath.Join(dir, "kotantei.db"), source)); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var output struct {
		Import *struct {
			AlreadyImported bool `json:"already_imported"`
		} `json:"import"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	if output.Import == nil || output.Import.AlreadyImported {
		t.Fatal("auto-import did not run")
	}
	stdout.Reset()
	if code := Run(context.Background(), []string{"init"}, &stdout, &stderr, envFor(filepath.Join(dir, "kotantei.db"), source)); code != 0 {
		t.Fatalf("repeat init exit %d: %s", code, stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	if output.Import == nil || !output.Import.AlreadyImported {
		t.Fatal("repeat init did not report idempotent import")
	}
}

func TestExplicitImportReportsMissingSourceAsFailure(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"import", "--source", filepath.Join(dir, "missing.db")}, &stdout, &stderr, envFor(filepath.Join(dir, "kotantei.db"), "")); code == 0 {
		t.Fatal("explicit missing import was silently skipped")
	}
}

func TestConfigFileCanSelectStorage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kotantei.db")
	configFile := filepath.Join(dir, "config.json")
	data, err := json.Marshal(map[string]any{"storage": map[string]any{"database": path, "tantei_database": ""}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configFile, data, 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"--config", configFile, "init"}, &stdout, &stderr, nil); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestUnknownCommandsAndUnexpectedArgumentsAreRejected(t *testing.T) {
	for _, args := range [][]string{{"unknown"}, {"init", "unexpected"}, {"doctor", "unexpected"}, {"import"}, {"--unknown"}} {
		var stdout, stderr bytes.Buffer
		if code := Run(context.Background(), args, &stdout, &stderr, nil); code == 0 {
			t.Fatalf("accepted invalid arguments: %v", args)
		}
	}
}

func TestCanceledCommandReportsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	if code := Run(ctx, []string{"init"}, &stdout, &stderr, envFor(filepath.Join(t.TempDir(), "kotantei.db"), "")); code != 130 {
		t.Fatalf("expected cancellation exit 130, got %d: %s", code, stderr.String())
	}
}

func TestDoctorReportsOnlyNonSensitiveCounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kotantei.db")
	s, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO users(id) VALUES ('1'); INSERT INTO accounts(provider,external_id,user_id,username) VALUES ('mal','2','1','DO-NOT-PRINT-USERNAME')`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"doctor"}, &stdout, &stderr, envFor(path, "")); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), "DO-NOT-PRINT") {
		t.Fatal("inspection printed account data")
	}
}

func TestOutboxCommandReportsFailuresWithoutPayloads(t *testing.T) {
	path := blockedOutbox(t)
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"outbox"}, &stdout, &stderr, envFor(path, "")); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var report struct {
		Outbox store.DeliveryStatus `json:"outbox"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Outbox.Failed != 1 || report.Outbox.BlockedChannels != 1 || len(report.Outbox.Channels) != 1 || report.Outbox.Channels[0].ChannelID != "11" {
		t.Fatalf("missing failure details: %+v", report.Outbox)
	}
	if strings.Contains(stdout.String()+stderr.String(), "DO-NOT-PRINT-PAYLOAD") {
		t.Fatal("outbox status exposed an event payload")
	}
}

func TestResumeChannelCommandRequeuesOnlyTheFailedChannel(t *testing.T) {
	path := blockedOutbox(t)
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"resume-channel", "--channel", "11"}, &stdout, &stderr, envFor(path, "")); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var report struct {
		Resumed int `json:"resumed"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Resumed != 1 {
		t.Fatalf("unexpected resumed count: %d", report.Resumed)
	}
	status, err := store.InspectOutbox(context.Background(), path)
	if err != nil || status.Pending != 1 || status.Failed != 0 || status.BlockedChannels != 0 {
		t.Fatalf("resume failed: %+v %v", status, err)
	}
}

func TestOutboxCommandsDoNotCreateMissingDatabases(t *testing.T) {
	for _, args := range [][]string{{"outbox"}, {"resume-channel", "--channel", "11"}} {
		path := filepath.Join(t.TempDir(), "missing.db")
		var stdout, stderr bytes.Buffer
		if code := Run(context.Background(), args, &stdout, &stderr, envFor(path, "")); code == 0 {
			t.Fatal("missing database accepted")
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("maintenance recovery created missing storage")
		}
	}
}

func TestOutboxCommandsRejectUnexpectedOrInvalidArguments(t *testing.T) {
	for _, args := range [][]string{{"outbox", "unexpected"}, {"resume-channel"}, {"resume-channel", "--channel", "0"}, {"resume-channel", "--channel", "invalid"}, {"resume-channel", "--channel", "11", "unexpected"}} {
		var stdout, stderr bytes.Buffer
		if code := Run(context.Background(), args, &stdout, &stderr, nil); code == 0 {
			t.Fatalf("invalid arguments accepted: %v", args)
		}
	}
}

func blockedOutbox(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "blocked.db")
	s, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.DB().Exec(`INSERT INTO users(id) VALUES ('1'); INSERT INTO guilds(id,channel_id) VALUES ('10','11');
		INSERT INTO guild_users(guild_id,user_id) VALUES ('10','1'); INSERT INTO accounts(id,provider,external_id,user_id) VALUES (1,'anilist','101','1')`); err != nil {
		t.Fatal(err)
	}
	q, err := store.NewOutbox(s, store.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.Record(context.Background(), store.Poll{AccountID: 1, Now: time.Unix(200, 0), Events: []update.Event{{Stream: update.Activity, Kind: update.List, SourceID: "1", Subject: update.Subject{Kind: update.AnimeSubject, ID: "42"}, OccurredAt: time.Unix(101, 0), Payload: json.RawMessage(`{"title":"DO-NOT-PRINT-PAYLOAD"}`)}}}); err != nil {
		t.Fatal(err)
	}
	delivery, err := q.Claim(context.Background(), time.Unix(200, 0), 30*time.Second)
	if err != nil || delivery == nil {
		t.Fatalf("claim failed: %+v %v", delivery, err)
	}
	if err := q.BlockChannel(context.Background(), delivery.Ref(), store.MissingPermissions, time.Unix(200, 0)); err != nil {
		t.Fatal(err)
	}
	return path
}

func envFor(database, legacy string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		switch key {
		case "KOTANTEI_DATABASE":
			return database, true
		case "KOTANTEI_TANTEI_DATABASE":
			return legacy, true
		}
		return "", false
	}
}

func createEmptyLegacy(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE DiscordUsers(DiscordUserId INTEGER);
		CREATE TABLE DiscordGuilds(DiscordGuildId INTEGER,PostingChannelId INTEGER);
		CREATE TABLE DiscordGuildDiscordUser(GuildsDiscordGuildId INTEGER,UsersDiscordUserId INTEGER);
		CREATE TABLE AniListUsers(Id INTEGER,DiscordUserId INTEGER,Features INTEGER,LastActivityTimestamp INTEGER,LastReviewTimestamp INTEGER);
		CREATE TABLE MalUsers(UserId INTEGER,DiscordUserId INTEGER,Username TEXT,Features INTEGER,LastUpdatedAnimeListTimestamp INTEGER,LastUpdatedMangaListTimestamp INTEGER);
		CREATE TABLE AniListFavourites(Id INTEGER,FavouriteType INTEGER,UserId INTEGER);
		CREATE TABLE MalFavorites(Id INTEGER,FavoriteType INTEGER,UserId INTEGER,Name TEXT,NameUrl TEXT,ImageUrl TEXT);
	`); err != nil {
		t.Fatal(err)
	}
}
