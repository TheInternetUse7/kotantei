package tantei

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kotantei/internal/store"
)

func TestImportPreservesAccountsSettingsCheckpointsAndMemberships(t *testing.T) {
	ctx := context.Background()
	source := fixture(t)
	before := digest(t, source)
	target := destination(t)
	report, err := Import(ctx, source, target)
	if err != nil {
		t.Fatal(err)
	}
	if report.Users != 3 || report.Guilds != 2 || report.Memberships != 4 || report.AniListAccounts != 1 || report.MALAccounts != 1 || report.AniListFavorites != 2 || report.MALFavorites != 2 || report.SkippedShikimoriAccounts != 1 {
		t.Fatalf("unexpected report: %+v", report)
	}
	if digest(t, source) != before {
		t.Fatal("import changed the legacy database")
	}
	var channel string
	if err := target.DB().QueryRow(`SELECT channel_id FROM guilds WHERE id='10'`).Scan(&channel); err != nil {
		t.Fatal(err)
	}
	if channel != "11" {
		t.Fatal("posting channel not preserved")
	}
	var externalID, username, colors string
	var features int64
	if err := target.DB().QueryRow(`SELECT external_id,username,features,colors FROM accounts WHERE provider='mal'`).Scan(&externalID, &username, &features, &colors); err != nil {
		t.Fatal(err)
	}
	if externalID != "201" || username != "friend" || features != 127 {
		t.Fatalf("incorrect MAL account: %s %s %d", externalID, username, features)
	}
	var overrides []map[string]int
	if err := json.Unmarshal([]byte(colors), &overrides); err != nil {
		t.Fatal(err)
	}
	if len(overrides) != 1 || overrides[0]["ColorValue"] != 16711680 || overrides[0]["UpdateType"] != 2 {
		t.Fatal("color override not preserved")
	}
	var value int64
	if err := target.DB().QueryRow(`SELECT value FROM checkpoints JOIN accounts ON accounts.id=checkpoints.account_id WHERE provider='anilist' AND kind='activity'`).Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != 1794000000 {
		t.Fatalf("incorrect AL activity checkpoint: %d", value)
	}
	if err := target.DB().QueryRow(`SELECT value FROM checkpoints JOIN accounts ON accounts.id=checkpoints.account_id WHERE provider='mal' AND kind='anime'`).Scan(&value); err != nil {
		t.Fatal(err)
	}
	want, _ := time.Parse(time.RFC3339Nano, "2026-11-07T07:44:15.1234Z")
	if value != want.UnixMicro() {
		t.Fatalf("incorrect MAL checkpoint: %d, want %d", value, want.UnixMicro())
	}
	var count int
	if err := target.DB().QueryRow(`SELECT COUNT(*) FROM guild_users JOIN accounts ON accounts.user_id=guild_users.user_id WHERE guild_users.guild_id='20'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatal("shared cross-provider guild membership not preserved")
	}
	if err := target.DB().QueryRow(`SELECT COUNT(*) FROM favorites WHERE kind='studio'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("AniList studio favorite not imported")
	}
	if err := target.DB().QueryRow(`SELECT COUNT(*) FROM users WHERE id='9223372036854775807'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("large Discord identifier lost precision")
	}
}

func TestImportIsIdempotentEvenAfterSourceIsRemoved(t *testing.T) {
	ctx := context.Background()
	source := fixture(t)
	target := destination(t)
	first, err := Import(ctx, source, target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	second, err := Import(ctx, source, target)
	if err != nil {
		t.Fatal(err)
	}
	if !second.AlreadyImported || second.Users != first.Users {
		t.Fatalf("unexpected repeated report: %+v", second)
	}
	summary, err := target.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Accounts != 2 || summary.Memberships != 4 {
		t.Fatal("retry changed imported data")
	}
}

func TestImportDoesNotReadOrCopyWebsiteTokens(t *testing.T) {
	ctx := context.Background()
	source := fixture(t)
	changeSource(t, source, `CREATE TABLE DiscordOAuthTokens(DiscordUserId INTEGER, AccessToken TEXT, RefreshToken TEXT);
		INSERT INTO DiscordOAuthTokens VALUES (1,'DO-NOT-COPY-ACCESS','DO-NOT-COPY-REFRESH')`)
	target := destination(t)
	report, err := Import(ctx, source, target)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "DO-NOT-COPY") {
		t.Fatal("report leaked a token")
	}
	var copied int
	if err := target.DB().QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE name='DiscordOAuthTokens'`).Scan(&copied); err != nil {
		t.Fatal(err)
	}
	if copied != 0 {
		t.Fatal("website token table copied")
	}
	var recorded string
	if err := target.DB().QueryRow(`SELECT report FROM imports`).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(recorded, "DO-NOT-COPY") {
		t.Fatal("recorded report leaked a token")
	}
}

func TestImportRejectsNonemptyDestination(t *testing.T) {
	target := destination(t)
	if _, err := target.DB().Exec(`INSERT INTO users(id) VALUES ('999')`); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(context.Background(), fixture(t), target); !errors.Is(err, ErrDestinationNotEmpty) {
		t.Fatalf("expected populated destination error, got %v", err)
	}
	summary, err := target.Summary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Users != 1 || summary.Accounts != 0 {
		t.Fatal("existing data was changed")
	}
}

func TestImportCannotOverwriteFromAnotherSource(t *testing.T) {
	target := destination(t)
	if _, err := Import(context.Background(), fixture(t), target); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(context.Background(), fixture(t), target); !errors.Is(err, ErrDifferentSource) {
		t.Fatalf("expected different source error, got %v", err)
	}
}

func TestInvalidSourceRollsBackAllInsertedData(t *testing.T) {
	for _, mutation := range []string{
		`UPDATE MalUsers SET LastUpdatedAnimeListTimestamp=-1`,
		`UPDATE AniListUsers SET Colors='not-json'`,
		`UPDATE MalUsers SET Colors='[{"ColorValue":-1,"UpdateType":2}]'`,
		`UPDATE AniListUsers SET DiscordUserId=999`,
		`UPDATE AniListFavourites SET FavouriteType=99`,
		`UPDATE DiscordUsers SET DiscordUserId=-1 WHERE DiscordUserId=1`,
		`UPDATE AniListUsers SET LastActivityTimestamp=-1`,
		`UPDATE MalUsers SET Features=-1`,
		`UPDATE MalUsers SET Username=''`,
	} {
		t.Run(mutation, func(t *testing.T) {
			source := fixture(t)
			changeSource(t, source, mutation)
			target := destination(t)
			if _, err := Import(context.Background(), source, target); err == nil {
				t.Fatal("invalid source accepted")
			}
			summary, err := target.Summary(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if summary != (store.Summary{}) {
				t.Fatalf("partial import persisted: %+v", summary)
			}
			var markers int
			if err := target.DB().QueryRow(`SELECT COUNT(*) FROM imports`).Scan(&markers); err != nil {
				t.Fatal(err)
			}
			if markers != 0 {
				t.Fatal("failed import was recorded as successful")
			}
		})
	}
}

func TestImportValidatesSourceSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unrelated.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE unrelated(value TEXT)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := Import(context.Background(), path, destination(t)); !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("expected invalid schema error, got %v", err)
	}
}

func TestImportDefaultsMissingOrNullColorColumns(t *testing.T) {
	for _, mutation := range []string{
		`UPDATE AniListUsers SET Colors=NULL; UPDATE MalUsers SET Colors=NULL`,
		`ALTER TABLE AniListUsers DROP COLUMN Colors; ALTER TABLE MalUsers DROP COLUMN Colors`,
	} {
		t.Run(mutation, func(t *testing.T) {
			source := fixture(t)
			changeSource(t, source, mutation)
			target := destination(t)
			if _, err := Import(context.Background(), source, target); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := target.DB().QueryRow(`SELECT COUNT(*) FROM accounts WHERE colors='[]'`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 2 {
				t.Fatal("missing colors did not default to an empty array")
			}
		})
	}
}

func TestImportReadsCommittedWALData(t *testing.T) {
	source := fixture(t)
	db, err := sql.Open("sqlite", source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; INSERT INTO DiscordUsers VALUES (3)`); err != nil {
		t.Fatal(err)
	}
	report, err := Import(context.Background(), source, destination(t))
	if err != nil {
		t.Fatal(err)
	}
	if report.Users != 4 {
		t.Fatal("committed WAL changes were ignored")
	}
}

func TestImportRejectsSourceDestinationAlias(t *testing.T) {
	target := destination(t)
	if _, err := Import(context.Background(), target.Path(), target); !errors.Is(err, ErrSameDatabase) {
		t.Fatalf("expected same-database error, got %v", err)
	}
}

func TestCanceledImportDoesNotPersistData(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	target := destination(t)
	if _, err := Import(ctx, fixture(t), target); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	summary, err := target.Summary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if summary != (store.Summary{}) {
		t.Fatal("canceled import persisted data")
	}
}

func TestImportRejectsTrailingColorJSONAndDuplicateOverrides(t *testing.T) {
	for _, colors := range []string{
		`[] {}`,
		`[{"ColorValue":1,"UpdateType":2},{"ColorValue":2,"UpdateType":2}]`,
	} {
		t.Run(colors, func(t *testing.T) {
			source := fixture(t)
			db, err := sql.Open("sqlite", source)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE AniListUsers SET Colors=?`, colors); err != nil {
				t.Fatal(err)
			}
			db.Close()
			if _, err := Import(context.Background(), source, destination(t)); err == nil {
				t.Fatal("accepted ambiguous color settings")
			}
		})
	}
}

func TestImportRejectsInvalidFavoriteYear(t *testing.T) {
	source := fixture(t)
	changeSource(t, source, `UPDATE MalFavorites SET StartYear=-1`)
	if _, err := Import(context.Background(), source, destination(t)); err == nil {
		t.Fatal("accepted a negative unsigned favorite year")
	}
}

func TestConcurrentImportsSerializeAndDoNotDuplicate(t *testing.T) {
	source := fixture(t)
	target := destination(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	results := make(chan error, 2)
	for range 2 {
		go func() { _, err := Import(ctx, source, target); results <- err }()
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	summary, err := target.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Accounts != 2 || summary.Users != 3 {
		t.Fatal("concurrent import duplicated records")
	}
}

func TestImportRetryAfterReopenDoesNotOverwriteLiveSettings(t *testing.T) {
	source := fixture(t)
	target := destination(t)
	if _, err := Import(context.Background(), source, target); err != nil {
		t.Fatal(err)
	}
	if _, err := target.DB().Exec(`UPDATE accounts SET features=0; INSERT INTO users(id) VALUES ('999')`); err != nil {
		t.Fatal(err)
	}
	path := target.Path()
	target.Close()
	reopened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	report, err := Import(context.Background(), source, reopened)
	if err != nil {
		t.Fatal(err)
	}
	if !report.AlreadyImported {
		t.Fatal("persistent marker not recognized")
	}
	var changed int
	if err := reopened.DB().QueryRow(`SELECT COUNT(*) FROM accounts WHERE features=0`).Scan(&changed); err != nil {
		t.Fatal(err)
	}
	if changed != 2 {
		t.Fatal("retry overwrote live account settings")
	}
	summary, err := reopened.Summary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Users != 4 {
		t.Fatal("retry overwrote live users")
	}
}

// A minimal independent fixture containing the columns the current Tantei
// models expose. Extra unrelated tables deliberately require no EF runtime.
func fixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy database.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`
		CREATE TABLE DiscordUsers(DiscordUserId INTEGER PRIMARY KEY);
		CREATE TABLE DiscordGuilds(DiscordGuildId INTEGER PRIMARY KEY, PostingChannelId INTEGER NOT NULL);
		CREATE TABLE DiscordGuildDiscordUser(GuildsDiscordGuildId INTEGER, UsersDiscordUserId INTEGER);
		CREATE TABLE AniListUsers(Id INTEGER PRIMARY KEY, DiscordUserId INTEGER, Features INTEGER, Colors TEXT, LastActivityTimestamp INTEGER, LastReviewTimestamp INTEGER);
		CREATE TABLE MalUsers(UserId INTEGER PRIMARY KEY, DiscordUserId INTEGER, Username TEXT, Features INTEGER, Colors TEXT, LastUpdatedAnimeListTimestamp INTEGER, LastUpdatedMangaListTimestamp INTEGER);
		CREATE TABLE AniListFavourites(Id INTEGER, FavouriteType INTEGER, UserId INTEGER);
		CREATE TABLE MalFavorites(Id INTEGER, FavoriteType INTEGER, UserId INTEGER, Name TEXT, NameUrl TEXT, ImageUrl TEXT, Type TEXT, StartYear INTEGER, FromTitleName TEXT);
		CREATE TABLE ShikiUsers(Id INTEGER PRIMARY KEY);
		INSERT INTO DiscordUsers VALUES (1),(2),(9223372036854775807);
		INSERT INTO DiscordGuilds VALUES (10,11),(20,21);
		INSERT INTO DiscordGuildDiscordUser VALUES (10,1),(10,2),(20,1),(10,9223372036854775807);
		INSERT INTO AniListUsers VALUES (101,1,127,'[]',1794000000,1793999000);
		INSERT INTO MalUsers VALUES (201,1,'friend',127,'[{"ColorValue":16711680,"UpdateType":2}]',1309279315048927562,1309278415339267676);
		INSERT INTO AniListFavourites VALUES (42,0,101),(43,4,101);
		INSERT INTO MalFavorites VALUES (52,1,201,'Anime','https://myanimelist.net/anime/52',NULL,'TV',2020,NULL),(53,3,201,'Character','https://myanimelist.net/character/53',NULL,NULL,NULL,'Example');
		INSERT INTO ShikiUsers VALUES (301);
	`)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func destination(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "kotantei.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func changeSource(t *testing.T, path, query string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(query); err != nil {
		t.Fatal(err)
	}
}

func digest(t *testing.T, path string) [32]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(data)
}
