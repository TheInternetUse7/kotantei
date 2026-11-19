package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"unicode/utf8"

	"kotantei/internal/update"
)

var ErrAccountConflict = errors.New("provider account is already linked; unlink it before switching accounts")
var ErrGuildNotConfigured = errors.New("server is not configured for tracking")

type ColorOverride struct {
	ColorValue uint32 `json:"ColorValue"`
	UpdateType uint8  `json:"UpdateType"`
}

type AccountLink struct {
	Provider   string
	ExternalID string
	UserID     string
	GuildID    string
	Username   string
	Cursors    map[update.Stream]update.Cursor
	Favorites  []Favorite
}
type LinkResult struct {
	AccountID int64
	Created   bool
}

func (q *Outbox) ForUser(ctx context.Context, userID, provider string) (Snapshot, error) {
	if !validNumericID(userID) || (provider != "anilist" && provider != "mal") {
		return Snapshot{}, ErrInvalidPoll
	}
	var id int64
	if err := q.store.db.QueryRowContext(ctx, `SELECT id FROM accounts WHERE user_id=? AND provider=?`, userID, provider).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Snapshot{}, ErrAccountNotFound
		}
		return Snapshot{}, err
	}
	return q.Snapshot(ctx, id)
}

// Connect writes a fetched baseline only for a new account. Adding another
// shared server membership never resets checkpoints, settings or delivery state.
// Callers must finish network requests before entering this transaction.
func (q *Outbox) Connect(ctx context.Context, input AccountLink) (LinkResult, error) {
	if !validNumericID(input.ExternalID) || !validNumericID(input.UserID) || !validNumericID(input.GuildID) || (input.Provider != "anilist" && input.Provider != "mal") {
		return LinkResult{}, ErrInvalidPoll
	}
	tx, err := q.store.db.BeginTx(ctx, nil)
	if err != nil {
		return LinkResult{}, err
	}
	defer tx.Rollback()
	var guild int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM guilds WHERE id=?`, input.GuildID).Scan(&guild); err != nil {
		return LinkResult{}, err
	}
	if guild != 1 {
		return LinkResult{}, ErrGuildNotConfigured
	}
	var id int64
	var external string
	err = tx.QueryRowContext(ctx, `SELECT id,external_id FROM accounts WHERE provider=? AND user_id=?`, input.Provider, input.UserID).Scan(&id, &external)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return LinkResult{}, err
	}
	created := errors.Is(err, sql.ErrNoRows)
	if !created && external != input.ExternalID {
		return LinkResult{}, ErrAccountConflict
	}
	if created {
		var taken int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE provider=? AND external_id=?`, input.Provider, input.ExternalID).Scan(&taken); err != nil {
			return LinkResult{}, err
		}
		if taken != 0 {
			return LinkResult{}, ErrAccountConflict
		}
		if !validAccountName(input.Username) || len(input.Favorites) > q.limits.MaxFavorites {
			return LinkResult{}, ErrInvalidPoll
		}
		streams := []update.Stream{update.Activity, update.ReviewStream}
		if input.Provider == "mal" {
			streams = []update.Stream{update.Anime, update.Manga}
		}
		if len(input.Cursors) != len(streams) {
			return LinkResult{}, ErrInvalidPoll
		}
		bytesLeft := q.limits.MaxBatchBytes
		cursors := map[update.Stream]update.Cursor{}
		for _, stream := range streams {
			cursor, ok := input.Cursors[stream]
			if !ok || cursor.Value < 0 {
				return LinkResult{}, ErrInvalidPoll
			}
			if len(cursor.SourceIDs) > q.limits.MaxBoundaryIDs {
				return LinkResult{}, update.ErrBoundaryLimit
			}
			ids := slices.Clone(cursor.SourceIDs)
			slices.Sort(ids)
			for n, source := range ids {
				if err := (update.Observation{Stream: stream, Value: cursor.Value, SourceID: source}).Validate(); err != nil {
					return LinkResult{}, err
				}
				if n > 0 && source == ids[n-1] {
					return LinkResult{}, ErrInvalidPoll
				}
				bytesLeft -= len(source)
				if bytesLeft < 0 {
					return LinkResult{}, ErrPollBudget
				}
			}
			cursor.SourceIDs = ids
			cursors[stream] = cursor
		}
		favorites, err := normalizeFavorites(input.Favorites, q.limits.MaxPayloadBytes, bytesLeft)
		if err != nil {
			return LinkResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO users(id) VALUES (?) ON CONFLICT(id) DO NOTHING`, input.UserID); err != nil {
			return LinkResult{}, err
		}
		id, err = nextCounter(ctx, tx, "account")
		if err != nil {
			return LinkResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO accounts(id,provider,external_id,user_id,username) VALUES (?,?,?,?,?)`, id, input.Provider, input.ExternalID, input.UserID, input.Username); err != nil {
			return LinkResult{}, err
		}
		for stream, cursor := range cursors {
			if _, err := tx.ExecContext(ctx, `INSERT INTO checkpoints(account_id,kind,value) VALUES (?,?,?)`, id, stream, cursor.Value); err != nil {
				return LinkResult{}, err
			}
			for _, source := range cursor.SourceIDs {
				if _, err := tx.ExecContext(ctx, `INSERT INTO checkpoint_items(account_id,kind,source_id) VALUES (?,?,?)`, id, stream, source); err != nil {
					return LinkResult{}, err
				}
			}
		}
		for _, favorite := range favorites {
			if _, err := tx.ExecContext(ctx, `INSERT INTO favorites(account_id,kind,external_id,name,name_url,image_url,extra) VALUES (?,?,?,?,?,?,?)`, id, favorite.Subject.Kind, favorite.Subject.ID, favorite.Name, favorite.NameURL, favorite.ImageURL, string(favorite.Extra)); err != nil {
				return LinkResult{}, err
			}
		}
	}
	generation, err := nextCounter(ctx, tx, "membership")
	if err != nil {
		return LinkResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO guild_users(guild_id,user_id,generation) VALUES (?,?,?) ON CONFLICT(guild_id,user_id) DO UPDATE SET generation=excluded.generation`, input.GuildID, input.UserID, generation); err != nil {
		return LinkResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return LinkResult{}, err
	}
	return LinkResult{AccountID: id, Created: created}, nil
}

func validAccountName(name string) bool {
	return strings.TrimSpace(name) != "" && name == strings.TrimSpace(name) && utf8.ValidString(name) && len(name) <= 256
}
