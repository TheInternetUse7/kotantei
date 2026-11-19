package tantei

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"kotantei/internal/store"
)

var (
	ErrDestinationNotEmpty = errors.New("Tantei import requires an empty destination")
	ErrDifferentSource     = errors.New("destination already imported a different Tantei database")
	ErrInvalidSource       = errors.New("source is not a supported Tantei database")
	ErrSameDatabase        = errors.New("source and destination must be different database files")
)

type Report struct {
	Users                    int  `json:"users"`
	Guilds                   int  `json:"guilds"`
	Memberships              int  `json:"memberships"`
	AniListAccounts          int  `json:"anilist_accounts"`
	MALAccounts              int  `json:"mal_accounts"`
	AniListFavorites         int  `json:"anilist_favorites"`
	MALFavorites             int  `json:"mal_favorites"`
	SkippedShikimoriAccounts int  `json:"skipped_shikimori_accounts"`
	AlreadyImported          bool `json:"already_imported"`
}

// Import reads a coherent legacy snapshot into an empty native store. The
// successful marker and all imported records commit in the same transaction.
func Import(ctx context.Context, sourcePath string, target *store.Store) (Report, error) {
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	if strings.TrimSpace(sourcePath) == "" {
		return Report{}, ErrInvalidSource
	}
	sourcePath, err := filepath.Abs(sourcePath)
	if err != nil {
		return Report{}, err
	}
	if sameFile(sourcePath, target.Path()) {
		return Report{}, ErrSameDatabase
	}
	tx, err := target.DB().BeginTx(ctx, nil)
	if err != nil {
		return Report{}, err
	}
	defer tx.Rollback()
	var previousPath, previousReport string
	err = tx.QueryRowContext(ctx, `SELECT source_path,report FROM imports LIMIT 1`).Scan(&previousPath, &previousReport)
	if err == nil {
		if !sameFile(previousPath, sourcePath) {
			return Report{}, ErrDifferentSource
		}
		var report Report
		if err := json.Unmarshal([]byte(previousReport), &report); err != nil {
			return Report{}, errors.New("invalid recorded import report")
		}
		report.AlreadyImported = true
		return report, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Report{}, err
	}
	var existing int64
	if err := tx.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM users)+(SELECT COUNT(*) FROM guilds)+
		(SELECT COUNT(*) FROM guild_users)+(SELECT COUNT(*) FROM accounts)+
		(SELECT COUNT(*) FROM checkpoints)+(SELECT COUNT(*) FROM favorites)`).Scan(&existing); err != nil {
		return Report{}, err
	}
	if existing != 0 {
		return Report{}, ErrDestinationNotEmpty
	}
	source, err := store.OpenReadOnly(ctx, sourcePath)
	if err != nil {
		return Report{}, err
	}
	defer source.Close()
	read, err := source.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Report{}, err
	}
	defer read.Rollback()
	schema, err := inspectLegacy(ctx, read)
	if err != nil {
		return Report{}, err
	}
	var integrity string
	if err := read.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&integrity); err != nil {
		return Report{}, fmt.Errorf("check legacy integrity: %w", err)
	}
	if integrity != "ok" {
		return Report{}, fmt.Errorf("%w: integrity check failed", ErrInvalidSource)
	}
	importer := legacyImport{ctx: ctx, read: read, write: tx, schema: schema}
	if err := importer.copy(); err != nil {
		return Report{}, err
	}
	encoded, err := json.Marshal(importer.report)
	if err != nil {
		return Report{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO imports(source_path,report,imported_at) VALUES (?,?,?)`, sourcePath, string(encoded), time.Now().UTC().Unix()); err != nil {
		return Report{}, err
	}
	if err := tx.Commit(); err != nil {
		return Report{}, err
	}
	return importer.report, nil
}

func sameFile(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if a == b || (runtime.GOOS == "windows" && strings.EqualFold(a, b)) {
		return true
	}
	first, err := os.Stat(a)
	if err != nil {
		return false
	}
	second, err := os.Stat(b)
	return err == nil && os.SameFile(first, second)
}

type legacySchema map[string]map[string]bool

func inspectLegacy(ctx context.Context, tx *sql.Tx) (legacySchema, error) {
	required := map[string][]string{
		"DiscordUsers":            {"DiscordUserId"},
		"DiscordGuilds":           {"DiscordGuildId", "PostingChannelId"},
		"DiscordGuildDiscordUser": {"GuildsDiscordGuildId", "UsersDiscordUserId"},
		"AniListUsers":            {"Id", "DiscordUserId", "Features", "LastActivityTimestamp", "LastReviewTimestamp"},
		"MalUsers":                {"UserId", "DiscordUserId", "Username", "Features", "LastUpdatedAnimeListTimestamp", "LastUpdatedMangaListTimestamp"},
		"AniListFavourites":       {"Id", "FavouriteType", "UserId"},
		"MalFavorites":            {"Id", "FavoriteType", "UserId", "Name", "NameUrl", "ImageUrl"},
	}
	schema := legacySchema{}
	for table, columns := range required {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_schema WHERE type='table' AND name=?`, table).Scan(&exists); err != nil {
			return nil, err
		}
		if exists != 1 {
			return nil, fmt.Errorf("%w: missing table %s", ErrInvalidSource, table)
		}
		rows, err := tx.QueryContext(ctx, `PRAGMA table_info("`+table+`")`)
		if err != nil {
			return nil, err
		}
		present := map[string]bool{}
		for rows.Next() {
			var cid, notnull, primaryKey int
			var name, kind string
			var defaultValue any
			if err := rows.Scan(&cid, &name, &kind, &notnull, &defaultValue, &primaryKey); err != nil {
				rows.Close()
				return nil, err
			}
			present[name] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		for _, column := range columns {
			if !present[column] {
				return nil, fmt.Errorf("%w: missing %s.%s", ErrInvalidSource, table, column)
			}
		}
		schema[table] = present
	}
	return schema, nil
}

type legacyImport struct {
	ctx    context.Context
	read   *sql.Tx
	write  *sql.Tx
	schema legacySchema
	report Report
}

func (i *legacyImport) rows(table, query string, width int, apply func([]any) error) error {
	rows, err := i.read.QueryContext(i.ctx, query)
	if err != nil {
		return fmt.Errorf("read %s: %w", table, err)
	}
	defer rows.Close()
	values := make([]any, width)
	addresses := make([]any, width)
	for n := range values {
		addresses[n] = &values[n]
	}
	for rows.Next() {
		if err := rows.Scan(addresses...); err != nil {
			return fmt.Errorf("read %s: %w", table, err)
		}
		if err := apply(values); err != nil {
			return fmt.Errorf("import %s: %w", table, err)
		}
	}
	return rows.Err()
}

func (i *legacyImport) copy() error {
	if err := i.rows("DiscordUsers", `SELECT DiscordUserId FROM DiscordUsers`, 1, func(v []any) error {
		id, err := identifier(v[0])
		if err != nil {
			return err
		}
		if _, err := i.write.ExecContext(i.ctx, `INSERT INTO users(id) VALUES (?)`, id); err != nil {
			return err
		}
		i.report.Users++
		return nil
	}); err != nil {
		return err
	}
	if err := i.rows("DiscordGuilds", `SELECT DiscordGuildId,PostingChannelId FROM DiscordGuilds`, 2, func(v []any) error {
		guild, err := identifier(v[0])
		if err != nil {
			return err
		}
		channel, err := identifier(v[1])
		if err != nil {
			return err
		}
		if _, err := i.write.ExecContext(i.ctx, `INSERT INTO guilds(id,channel_id) VALUES (?,?)`, guild, channel); err != nil {
			return err
		}
		i.report.Guilds++
		return nil
	}); err != nil {
		return err
	}
	if err := i.rows("DiscordGuildDiscordUser", `SELECT GuildsDiscordGuildId,UsersDiscordUserId FROM DiscordGuildDiscordUser`, 2, func(v []any) error {
		guild, err := identifier(v[0])
		if err != nil {
			return err
		}
		user, err := identifier(v[1])
		if err != nil {
			return err
		}
		if _, err := i.write.ExecContext(i.ctx, `INSERT INTO guild_users(guild_id,user_id) VALUES (?,?)`, guild, user); err != nil {
			return err
		}
		i.report.Memberships++
		return nil
	}); err != nil {
		return err
	}
	if err := i.accounts("anilist"); err != nil {
		return err
	}
	if err := i.accounts("mal"); err != nil {
		return err
	}
	if err := i.favorites("anilist"); err != nil {
		return err
	}
	if err := i.favorites("mal"); err != nil {
		return err
	}
	var shikiExists int
	if err := i.read.QueryRowContext(i.ctx, `SELECT COUNT(*) FROM sqlite_schema WHERE type='table' AND name='ShikiUsers'`).Scan(&shikiExists); err != nil {
		return err
	}
	if shikiExists != 0 {
		if err := i.read.QueryRowContext(i.ctx, `SELECT COUNT(*) FROM ShikiUsers`).Scan(&i.report.SkippedShikimoriAccounts); err != nil {
			return err
		}
	}
	return nil
}

func (i *legacyImport) column(table, column string) string {
	if i.schema[table][column] {
		return `"` + column + `"`
	}
	return "NULL"
}

func (i *legacyImport) accounts(provider string) error {
	table := "AniListUsers"
	query := `SELECT Id,DiscordUserId,Features,` + i.column(table, "Colors") + `,LastActivityTimestamp,LastReviewTimestamp,'' FROM AniListUsers`
	if provider == "mal" {
		table = "MalUsers"
		query = `SELECT UserId,DiscordUserId,Features,` + i.column(table, "Colors") + `,LastUpdatedAnimeListTimestamp,LastUpdatedMangaListTimestamp,Username FROM MalUsers`
	}
	return i.rows(table, query, 7, func(v []any) error {
		externalID, err := identifier(v[0])
		if err != nil {
			return err
		}
		user, err := identifier(v[1])
		if err != nil {
			return err
		}
		features, err := integer(v[2])
		if err != nil || features < 0 {
			return errors.New("invalid Features")
		}
		colors, err := normalizeColors(v[3])
		if err != nil {
			return err
		}
		username, err := text(v[6], false)
		if err != nil {
			return err
		}
		if provider == "mal" && strings.TrimSpace(username) == "" {
			return errors.New("MAL Username must not be empty")
		}
		first, err := integer(v[4])
		if err != nil {
			return errors.New("invalid first update checkpoint")
		}
		second, err := integer(v[5])
		if err != nil {
			return errors.New("invalid second update checkpoint")
		}
		kinds := []string{"activity", "review"}
		if provider == "mal" {
			kinds = []string{"anime", "manga"}
			date, err := DecodeDateTimeOffsetBinary(first)
			if err != nil {
				return err
			}
			first = date.UnixMicro()
			date, err = DecodeDateTimeOffsetBinary(second)
			if err != nil {
				return err
			}
			second = date.UnixMicro()
		} else if first < 0 || second < 0 {
			return errors.New("AniList checkpoints must not be negative")
		}
		result, err := i.write.ExecContext(i.ctx, `INSERT INTO accounts(provider,external_id,user_id,username,features,colors) VALUES (?,?,?,?,?,?)`, provider, externalID, user, username, features, colors)
		if err != nil {
			return err
		}
		accountID, err := result.LastInsertId()
		if err != nil {
			return err
		}
		if _, err := i.write.ExecContext(i.ctx, `INSERT INTO checkpoints(account_id,kind,value) VALUES (?,?,?),(?,?,?)`, accountID, kinds[0], first, accountID, kinds[1], second); err != nil {
			return err
		}
		if provider == "anilist" {
			i.report.AniListAccounts++
		} else {
			i.report.MALAccounts++
		}
		return nil
	})
}

func (i *legacyImport) favorites(provider string) error {
	table := "AniListFavourites"
	query := `SELECT Id,FavouriteType,UserId,'','','',NULL,NULL,NULL FROM AniListFavourites`
	kinds := []string{"anime", "manga", "character", "staff", "studio"}
	if provider == "mal" {
		table = "MalFavorites"
		query = `SELECT Id,FavoriteType,UserId,Name,NameUrl,ImageUrl,` + i.column(table, "Type") + `,` + i.column(table, "StartYear") + `,` + i.column(table, "FromTitleName") + ` FROM MalFavorites`
		kinds = []string{"", "anime", "manga", "character", "person", "company"}
	}
	return i.rows(table, query, 9, func(v []any) error {
		externalID, err := identifier(v[0])
		if err != nil {
			return err
		}
		kind, err := integer(v[1])
		if err != nil || kind < 0 || kind >= int64(len(kinds)) || kinds[kind] == "" {
			return errors.New("invalid favorite kind")
		}
		owner, err := identifier(v[2])
		if err != nil {
			return err
		}
		var accountID int64
		if err := i.write.QueryRowContext(i.ctx, `SELECT id FROM accounts WHERE provider=? AND external_id=?`, provider, owner).Scan(&accountID); err != nil {
			return errors.New("favorite refers to an unknown account")
		}
		name, err := text(v[3], true)
		if err != nil {
			return err
		}
		nameURL, err := text(v[4], true)
		if err != nil {
			return err
		}
		imageURL, err := text(v[5], true)
		if err != nil {
			return err
		}
		extra := map[string]any{}
		for field, index := range map[string]int{"type": 6, "from_title_name": 8} {
			if v[index] != nil {
				value, err := text(v[index], false)
				if err != nil {
					return err
				}
				extra[field] = value
			}
		}
		if v[7] != nil {
			year, err := integer(v[7])
			if err != nil || year < 0 || year > 65535 {
				return errors.New("invalid favorite StartYear")
			}
			extra["start_year"] = year
		}
		encoded, err := json.Marshal(extra)
		if err != nil {
			return err
		}
		if _, err := i.write.ExecContext(i.ctx, `INSERT INTO favorites(account_id,kind,external_id,name,name_url,image_url,extra) VALUES (?,?,?,?,?,?,?)`, accountID, kinds[kind], externalID, name, nameURL, imageURL, string(encoded)); err != nil {
			return err
		}
		if provider == "anilist" {
			i.report.AniListFavorites++
		} else {
			i.report.MALFavorites++
		}
		return nil
	})
}

func identifier(value any) (string, error) {
	var raw string
	switch value := value.(type) {
	case int64:
		if value <= 0 {
			return "", errors.New("identifier must be a positive integer")
		}
		raw = strconv.FormatInt(value, 10)
	case string:
		raw = value
	case []byte:
		raw = string(value)
	default:
		return "", errors.New("identifier must be stored as an integer or decimal text, not a floating-point value")
	}
	parsed, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || parsed == 0 {
		return "", errors.New("invalid positive numeric identifier")
	}
	return strconv.FormatUint(parsed, 10), nil
}

func integer(value any) (int64, error) {
	switch value := value.(type) {
	case int64:
		return value, nil
	case string:
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err == nil {
			return parsed, nil
		}
	case []byte:
		return integer(string(value))
	}
	return 0, errors.New("expected an integer column")
}

func text(value any, nullable bool) (string, error) {
	switch value := value.(type) {
	case string:
		return value, nil
	case []byte:
		return string(value), nil
	case nil:
		if nullable {
			return "", nil
		}
	}
	return "", errors.New("expected a text column")
}

func normalizeColors(value any) (string, error) {
	if value == nil {
		return "[]", nil
	}
	raw, err := text(value, false)
	if err != nil {
		return "", errors.New("invalid Colors")
	}
	type color struct {
		ColorValue *int64 `json:"ColorValue"`
		UpdateType *int64 `json:"UpdateType"`
	}
	var colors []color
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&colors); err != nil || colors == nil {
		return "", errors.New("Colors must be an array of color overrides")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return "", errors.New("Colors must contain a single JSON array")
	}
	seen := map[int64]bool{}
	for _, override := range colors {
		if override.ColorValue == nil || override.UpdateType == nil || *override.ColorValue < 0 || *override.ColorValue > 0xffffff || *override.UpdateType < 0 || *override.UpdateType > 255 {
			return "", errors.New("invalid color override")
		}
		if seen[*override.UpdateType] {
			return "", errors.New("duplicate color override for an update type")
		}
		seen[*override.UpdateType] = true
	}
	encoded, err := json.Marshal(colors)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}
