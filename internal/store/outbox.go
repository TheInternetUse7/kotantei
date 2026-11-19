package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"kotantei/internal/update"
)

var (
	ErrStalePoll       = errors.New("account snapshot changed while polling; reload before retrying")
	ErrBackpressure    = errors.New("pending delivery limit exceeded; poll was not committed")
	ErrPollBudget      = errors.New("poll collection budget exceeded; checkpoint was not advanced")
	ErrInvalidPoll     = errors.New("invalid poll result")
	ErrLeaseLost       = errors.New("delivery lease no longer belongs to this worker")
	ErrNeedsMigration  = errors.New("outbox schema requires an upgrade; run init first")
	ErrAccountNotFound = errors.New("tracked account no longer exists")
)

type Limits struct {
	MaxRecords      int
	MaxPayloadBytes int
	MaxBatchBytes   int
	MaxPending      int
	MaxPendingBytes int64
	MaxTargets      int
	MaxBoundaryIDs  int
	MaxFavorites    int
}

func DefaultLimits() Limits {
	return Limits{MaxRecords: 512, MaxPayloadBytes: 64 << 10, MaxBatchBytes: 4 << 20, MaxPending: 4096, MaxPendingBytes: 16 << 20, MaxTargets: 128, MaxBoundaryIDs: 4096, MaxFavorites: 5000}
}

type Favorite struct {
	Subject  update.Subject
	Name     string
	NameURL  string
	ImageURL string
	Extra    json.RawMessage
}
type Snapshot struct {
	AccountID  int64
	Provider   string
	ExternalID string
	UserID     string
	Username   string
	Features   int64
	Colors     []ColorOverride
	Revision   int64
	Cursors    map[update.Stream]update.Cursor
	Favorites  []Favorite
}
type Poll struct {
	AccountID        int64
	ExpectedRevision int64
	Events           []update.Event
	Observations     []update.Observation
	Favorites        *[]Favorite
	Username         *string
	Now              time.Time
}
type PollResult struct {
	Revision         int64
	Events           int
	Deliveries       int
	FailedDeliveries int
}
type Outbox struct {
	store  *Store
	limits Limits
}

// Limits returns the immutable collection bounds for provider integration.
func (q *Outbox) Limits() Limits { return q.limits }

func NewOutbox(s *Store, limits Limits) (*Outbox, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("outbox requires an open store")
	}
	if limits == (Limits{}) {
		limits = DefaultLimits()
	}
	if limits.MaxRecords < 1 || limits.MaxPayloadBytes < 1 || limits.MaxBatchBytes < 1 || limits.MaxPending < 1 || limits.MaxPendingBytes < 1 || limits.MaxTargets < 1 || limits.MaxBoundaryIDs < 1 || limits.MaxFavorites < 1 {
		return nil, errors.New("outbox limits must be positive")
	}
	return &Outbox{store: s, limits: limits}, nil
}

// Snapshot must be captured before fetching upstream pages. Record uses its
// revision for optimistic concurrency, and will not overwrite newer state.
func (q *Outbox) Snapshot(ctx context.Context, accountID int64) (Snapshot, error) {
	if accountID <= 0 {
		return Snapshot{}, ErrAccountNotFound
	}
	tx, err := q.store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Snapshot{}, err
	}
	defer tx.Rollback()
	return loadSnapshot(ctx, tx, accountID, q.limits)
}

func loadSnapshot(ctx context.Context, tx *sql.Tx, accountID int64, limits Limits) (Snapshot, error) {
	state := Snapshot{AccountID: accountID, Cursors: map[update.Stream]update.Cursor{}, Favorites: []Favorite{}}
	var colors string
	if err := tx.QueryRowContext(ctx, `SELECT provider,revision,external_id,user_id,username,features,colors FROM accounts WHERE id=?`, accountID).Scan(&state.Provider, &state.Revision, &state.ExternalID, &state.UserID, &state.Username, &state.Features, &colors); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Snapshot{}, ErrAccountNotFound
		}
		return Snapshot{}, err
	}
	if len(colors) > 64<<10 || json.Unmarshal([]byte(colors), &state.Colors) != nil || len(state.Colors) > 256 {
		return Snapshot{}, ErrInvalidPoll
	}
	seenColors := map[uint8]bool{}
	for _, color := range state.Colors {
		if color.ColorValue > 0xffffff || seenColors[color.UpdateType] {
			return Snapshot{}, ErrInvalidPoll
		}
		seenColors[color.UpdateType] = true
	}
	rows, err := tx.QueryContext(ctx, `SELECT kind,value FROM checkpoints WHERE account_id=?`, accountID)
	if err != nil {
		return Snapshot{}, err
	}
	for rows.Next() {
		var stream update.Stream
		var value int64
		if err := rows.Scan(&stream, &value); err != nil {
			rows.Close()
			return Snapshot{}, err
		}
		state.Cursors[stream] = update.Cursor{Value: value, SourceIDs: []string{}}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return Snapshot{}, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT kind,source_id FROM checkpoint_items WHERE account_id=? ORDER BY kind,source_id`, accountID)
	if err != nil {
		return Snapshot{}, err
	}
	for rows.Next() {
		var stream update.Stream
		var id string
		if err := rows.Scan(&stream, &id); err != nil {
			rows.Close()
			return Snapshot{}, err
		}
		cursor := state.Cursors[stream]
		if len(cursor.SourceIDs) >= limits.MaxBoundaryIDs {
			rows.Close()
			return Snapshot{}, update.ErrBoundaryLimit
		}
		cursor.SourceIDs = append(cursor.SourceIDs, id)
		state.Cursors[stream] = cursor
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return Snapshot{}, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT kind,external_id,name,name_url,image_url,extra FROM favorites WHERE account_id=? ORDER BY kind,external_id LIMIT ?`, accountID, limits.MaxFavorites+1)
	if err != nil {
		return Snapshot{}, err
	}
	defer rows.Close()
	bytesRead := 0
	for rows.Next() {
		var favorite Favorite
		var extra string
		if err := rows.Scan(&favorite.Subject.Kind, &favorite.Subject.ID, &favorite.Name, &favorite.NameURL, &favorite.ImageURL, &extra); err != nil {
			return Snapshot{}, err
		}
		favorite.Extra = json.RawMessage(extra)
		bytesRead += favoriteSize(favorite)
		if len(state.Favorites) >= limits.MaxFavorites || bytesRead > limits.MaxBatchBytes {
			return Snapshot{}, ErrPollBudget
		}
		state.Favorites = append(state.Favorites, favorite)
	}
	return state, rows.Err()
}

// Record takes a complete, bounded collection, including observations that
// did not produce an embed. It commits cursors, favorite baselines, events and
// channel fan-out together. No network operation occurs inside the transaction.
func (q *Outbox) Record(ctx context.Context, input Poll) (PollResult, error) {
	if err := ctx.Err(); err != nil {
		return PollResult{}, err
	}
	if input.AccountID <= 0 || input.ExpectedRevision < 0 || input.Now.IsZero() || input.Now.Unix() < 0 || input.Now.Year() > 9999 {
		return PollResult{}, ErrInvalidPoll
	}
	if len(input.Events) > q.limits.MaxRecords || len(input.Observations) > q.limits.MaxRecords-len(input.Events) {
		return PollResult{}, ErrPollBudget
	}
	bytesRead := 0
	if input.Username != nil {
		if !validAccountName(*input.Username) {
			return PollResult{}, ErrInvalidPoll
		}
		bytesRead += len(*input.Username)
		if bytesRead > q.limits.MaxBatchBytes {
			return PollResult{}, ErrPollBudget
		}
	}
	for _, event := range input.Events {
		if len(event.Payload) > q.limits.MaxPayloadBytes {
			return PollResult{}, ErrPollBudget
		}
		bytesRead += len(event.Payload) + len(event.SourceID) + len(event.Subject.ID)
		if bytesRead > q.limits.MaxBatchBytes {
			return PollResult{}, ErrPollBudget
		}
		if err := event.Validate(); err != nil {
			return PollResult{}, err
		}
	}
	for _, observation := range input.Observations {
		if err := observation.Validate(); err != nil {
			return PollResult{}, err
		}
		bytesRead += len(observation.SourceID)
		if bytesRead > q.limits.MaxBatchBytes {
			return PollResult{}, ErrPollBudget
		}
	}
	var favorites []Favorite
	if input.Favorites != nil {
		if len(*input.Favorites) > q.limits.MaxFavorites {
			return PollResult{}, ErrPollBudget
		}
		var err error
		favorites, err = normalizeFavorites(*input.Favorites, q.limits.MaxPayloadBytes, q.limits.MaxBatchBytes-bytesRead)
		if err != nil {
			return PollResult{}, err
		}
		for _, favorite := range favorites {
			bytesRead += favoriteSize(favorite)
			if bytesRead > q.limits.MaxBatchBytes {
				return PollResult{}, ErrPollBudget
			}
		}
	}
	tx, err := q.store.db.BeginTx(ctx, nil)
	if err != nil {
		return PollResult{}, err
	}
	defer tx.Rollback()
	state, err := loadSnapshot(ctx, tx, input.AccountID, q.limits)
	if err != nil {
		return PollResult{}, err
	}
	if state.Revision != input.ExpectedRevision {
		return PollResult{}, ErrStalePoll
	}
	observations := map[update.Stream][]update.Observation{}
	fresh := make([]update.Event, 0, len(input.Events))
	oldFavorites := favoriteSet(state.Favorites)
	newFavorites := favoriteSet(favorites)
	for _, event := range input.Events {
		if !providerStream(state.Provider, event.Stream) {
			return PollResult{}, ErrInvalidPoll
		}
		if event.Stream == update.Favorites {
			if input.Favorites == nil {
				return PollResult{}, fmt.Errorf("%w: favorite events require a complete favorite snapshot", ErrInvalidPoll)
			}
			before, after := oldFavorites[event.Subject], newFavorites[event.Subject]
			if (event.Kind == update.FavoriteAdded && !before && after) || (event.Kind == update.FavoriteRemoved && before && !after) {
				fresh = append(fresh, event)
			}
			continue
		}
		observation, _ := event.Observation()
		observations[observation.Stream] = append(observations[observation.Stream], observation)
		if !state.Cursors[observation.Stream].Contains(observation) {
			fresh = append(fresh, event)
		}
	}
	for _, observation := range input.Observations {
		if !providerStream(state.Provider, observation.Stream) {
			return PollResult{}, ErrInvalidPoll
		}
		observations[observation.Stream] = append(observations[observation.Stream], observation)
	}
	cursors := map[update.Stream]update.Cursor{}
	for stream, values := range observations {
		previous := state.Cursors[stream]
		next, err := previous.Advance(values, q.limits.MaxBoundaryIDs)
		if err != nil {
			return PollResult{}, err
		}
		if next.Value != previous.Value || !slices.Equal(next.SourceIDs, previous.SourceIDs) {
			cursors[stream] = next
		}
	}
	normalized, err := update.Normalize(fresh)
	if err != nil {
		return PollResult{}, err
	}
	targets := []deliveryTarget{}
	if len(normalized) > 0 {
		targets, err = loadTargets(ctx, tx, input.AccountID, q.limits.MaxTargets)
		if err != nil {
			return PollResult{}, err
		}
	}
	readyTargets := 0
	for _, target := range targets {
		if target.blockedReason == "" {
			readyTargets++
		}
	}
	type candidate struct {
		event update.Event
		key   string
	}
	candidates := make([]candidate, 0, len(normalized))
	var addedBytes int64
	for _, event := range normalized {
		key, err := event.Key(input.AccountID, state.Revision)
		if err != nil {
			return PollResult{}, err
		}
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE event_key=?`, key).Scan(&exists); err != nil {
			return PollResult{}, err
		}
		if exists != 0 {
			continue
		}
		candidates = append(candidates, candidate{event: event, key: key})
		addedBytes += int64(len(event.Payload)) * int64(readyTargets)
	}
	addedPending := int64(len(candidates)) * int64(readyTargets)
	if err := q.checkCapacity(ctx, tx, addedPending, addedBytes); err != nil {
		return PollResult{}, err
	}
	favoritesChanged := input.Favorites != nil && !equalFavorites(state.Favorites, favorites)
	username := state.Username
	if input.Username != nil {
		username = *input.Username
	}
	usernameChanged := username != state.Username
	result := PollResult{Revision: state.Revision}
	if len(candidates) == 0 && len(cursors) == 0 && !favoritesChanged && !usernameChanged {
		return result, nil
	}
	if state.Revision == math.MaxInt64 {
		return PollResult{}, fmt.Errorf("%w: account revision exhausted", ErrInvalidPoll)
	}
	now := input.Now.UTC().UnixMicro()
	for _, item := range candidates {
		event := item.event
		inserted, err := tx.ExecContext(ctx, `INSERT INTO events(account_id,event_key,stream,kind,source_id,subject_kind,subject_id,occurred_at,payload,created_at) VALUES (?,?,?,?,?,?,?,?,?,?)`, input.AccountID, item.key, event.Stream, event.Kind, event.SourceID, event.Subject.Kind, event.Subject.ID, event.OccurredAt.UnixMicro(), string(event.Payload), now)
		if err != nil {
			return PollResult{}, err
		}
		id, err := inserted.LastInsertId()
		if err != nil {
			return PollResult{}, err
		}
		result.Events++
		for _, target := range targets {
			status := "pending"
			if target.blockedReason != "" {
				status = "failed"
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO deliveries(event_id,channel_id,state,available_at,last_error_code,created_at) VALUES (?,?,?,?,?,?)`, id, target.id, status, now, target.blockedReason, now); err != nil {
				return PollResult{}, err
			}
			if status == "pending" {
				result.Deliveries++
			} else {
				result.FailedDeliveries++
			}
		}
	}
	for stream, cursor := range cursors {
		if _, err := tx.ExecContext(ctx, `INSERT INTO checkpoints(account_id,kind,value) VALUES (?,?,?) ON CONFLICT(account_id,kind) DO UPDATE SET value=excluded.value`, input.AccountID, stream, cursor.Value); err != nil {
			return PollResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM checkpoint_items WHERE account_id=? AND kind=?`, input.AccountID, stream); err != nil {
			return PollResult{}, err
		}
		for _, id := range cursor.SourceIDs {
			if _, err := tx.ExecContext(ctx, `INSERT INTO checkpoint_items(account_id,kind,source_id) VALUES (?,?,?)`, input.AccountID, stream, id); err != nil {
				return PollResult{}, err
			}
		}
	}
	if favoritesChanged {
		if _, err := tx.ExecContext(ctx, `DELETE FROM favorites WHERE account_id=?`, input.AccountID); err != nil {
			return PollResult{}, err
		}
		for _, favorite := range favorites {
			if _, err := tx.ExecContext(ctx, `INSERT INTO favorites(account_id,kind,external_id,name,name_url,image_url,extra) VALUES (?,?,?,?,?,?,?)`, input.AccountID, favorite.Subject.Kind, favorite.Subject.ID, favorite.Name, favorite.NameURL, favorite.ImageURL, string(favorite.Extra)); err != nil {
				return PollResult{}, err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE accounts SET revision=revision+1,username=? WHERE id=?`, username, input.AccountID); err != nil {
		return PollResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return PollResult{}, err
	}
	result.Revision++
	if result.Deliveries > 0 {
		q.signal()
	}
	return result, nil
}

type deliveryTarget struct {
	id            string
	blockedReason string
}

func loadTargets(ctx context.Context, tx *sql.Tx, accountID int64, limit int) ([]deliveryTarget, error) {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT g.channel_id,COALESCE(c.blocked_reason,'') FROM accounts a
		JOIN guild_users m ON m.user_id=a.user_id JOIN guilds g ON g.id=m.guild_id
		LEFT JOIN delivery_channels c ON c.channel_id=g.channel_id WHERE a.id=? ORDER BY g.channel_id LIMIT ?`, accountID, limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []deliveryTarget{}
	for rows.Next() {
		var target deliveryTarget
		if err := rows.Scan(&target.id, &target.blockedReason); err != nil {
			return nil, err
		}
		if len(result) >= limit {
			return nil, ErrPollBudget
		}
		result = append(result, target)
	}
	return result, rows.Err()
}

func providerStream(provider string, stream update.Stream) bool {
	if stream == update.Favorites {
		return provider == "anilist" || provider == "mal"
	}
	if provider == "anilist" {
		return stream == update.Activity || stream == update.ReviewStream
	}
	return provider == "mal" && (stream == update.Anime || stream == update.Manga)
}

func (q *Outbox) checkCapacity(ctx context.Context, tx *sql.Tx, count, bytesAdded int64) error {
	if count == 0 && bytesAdded == 0 {
		return nil
	}
	var pending, bytesPending int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(e.payload_bytes),0) FROM deliveries d JOIN events e ON e.id=d.event_id WHERE d.state='pending'`).Scan(&pending, &bytesPending); err != nil {
		return err
	}
	if count > int64(q.limits.MaxPending)-pending || bytesAdded > q.limits.MaxPendingBytes-bytesPending {
		return ErrBackpressure
	}
	return nil
}

func normalizeFavorites(favorites []Favorite, maxPayload, remainingBytes int) ([]Favorite, error) {
	result := make([]Favorite, 0, len(favorites))
	seen := map[update.Subject]bool{}
	for _, favorite := range favorites {
		if err := favorite.Subject.Validate(); err != nil {
			return nil, err
		}
		if seen[favorite.Subject] {
			return nil, fmt.Errorf("%w: duplicate favorite identity", ErrInvalidPoll)
		}
		seen[favorite.Subject] = true
		if len(favorite.Extra) == 0 {
			favorite.Extra = json.RawMessage(`{}`)
		}
		if size := favoriteSize(favorite); size > maxPayload || size > remainingBytes {
			return nil, ErrPollBudget
		}
		if !utf8.ValidString(favorite.Name) || !utf8.ValidString(favorite.NameURL) || !utf8.ValidString(favorite.ImageURL) || !utf8.Valid(favorite.Extra) {
			return nil, ErrInvalidPoll
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(favorite.Extra, &object) != nil || object == nil {
			return nil, fmt.Errorf("%w: favorite metadata must be a JSON object", ErrInvalidPoll)
		}
		encoded, err := json.Marshal(object)
		if err != nil {
			return nil, err
		}
		favorite.Extra = encoded
		// JSON encoding can expand metadata through escaping. Enforce the
		// encoded size too, before retaining another snapshot entry.
		size := favoriteSize(favorite)
		if size > maxPayload || size > remainingBytes {
			return nil, ErrPollBudget
		}
		remainingBytes -= size
		result = append(result, favorite)
	}
	slices.SortFunc(result, func(a, b Favorite) int {
		if order := strings.Compare(string(a.Subject.Kind), string(b.Subject.Kind)); order != 0 {
			return order
		}
		return strings.Compare(a.Subject.ID, b.Subject.ID)
	})
	return result, nil
}

func favoriteSize(favorite Favorite) int {
	return len(favorite.Subject.ID) + len(favorite.Name) + len(favorite.NameURL) + len(favorite.ImageURL) + len(favorite.Extra)
}

func favoriteSet(favorites []Favorite) map[update.Subject]bool {
	set := make(map[update.Subject]bool, len(favorites))
	for _, favorite := range favorites {
		set[favorite.Subject] = true
	}
	return set
}

func equalFavorites(a, b []Favorite) bool {
	if len(a) != len(b) {
		return false
	}
	for n := range a {
		if a[n].Subject != b[n].Subject || a[n].Name != b[n].Name || a[n].NameURL != b[n].NameURL || a[n].ImageURL != b[n].ImageURL || !bytes.Equal(a[n].Extra, b[n].Extra) {
			return false
		}
	}
	return true
}

// Changed is a bounded wake-up hint, not the queue itself; SQLite remains
// authoritative, and workers also check for expired leases and scheduled retries.
func (q *Outbox) Changed() <-chan struct{} { return q.store.wake }
func (q *Outbox) signal() {
	select {
	case q.store.wake <- struct{}{}:
	default:
	}
}
