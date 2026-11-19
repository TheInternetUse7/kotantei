package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"time"
)

var (
	ErrGuildAlreadyConfigured = errors.New("server already has an update channel; use update instead")
	ErrUnchangedChannel       = errors.New("that channel is already configured")
	ErrMembershipNotFound     = errors.New("user is not subscribed in this server")
)

type AccountSummary struct {
	AccountID  int64
	Provider   string
	ExternalID string
	UserID     string
	Username   string
	Features   int64
	Colors     []ColorOverride
	Revision   int64
}
type Membership struct {
	GuildID, UserID string
	Generation      int64
}
type GuildStatus struct {
	ChannelID       string
	Pending, Failed int
	BlockedReason   Reason
}

func (q *Outbox) ConfigureGuild(ctx context.Context, guildID, channelID string, replace bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validNumericID(guildID) || !validNumericID(channelID) {
		return ErrInvalidPoll
	}
	tx, err := q.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current string
	err = tx.QueryRowContext(ctx, `SELECT channel_id FROM guilds WHERE id=?`, guildID).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if replace {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrGuildNotConfigured
		}
		if current == channelID {
			return ErrUnchangedChannel
		}
		_, err = tx.ExecContext(ctx, `UPDATE guilds SET channel_id=? WHERE id=?`, channelID, guildID)
	} else {
		if err == nil {
			return ErrGuildAlreadyConfigured
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO guilds(id,channel_id) VALUES (?,?)`, guildID, channelID)
	}
	if err != nil {
		return err
	}
	if err := cancelUnsubscribed(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	q.signal()
	return nil
}

func (q *Outbox) GuildStatus(ctx context.Context, guildID string) (GuildStatus, error) {
	if !validNumericID(guildID) {
		return GuildStatus{}, ErrInvalidPoll
	}
	status := GuildStatus{}
	err := q.store.db.QueryRowContext(ctx, `SELECT g.channel_id,COALESCE(c.blocked_reason,''),
		(SELECT COUNT(*) FROM deliveries d WHERE d.channel_id=g.channel_id AND d.state='pending'),
		(SELECT COUNT(*) FROM deliveries d WHERE d.channel_id=g.channel_id AND d.state='failed')
		FROM guilds g LEFT JOIN delivery_channels c ON c.channel_id=g.channel_id WHERE g.id=?`, guildID).Scan(&status.ChannelID, &status.BlockedReason, &status.Pending, &status.Failed)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrGuildNotConfigured
	}
	return status, err
}

func (q *Outbox) RemoveGuild(ctx context.Context, guildID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validNumericID(guildID) {
		return ErrInvalidPoll
	}
	tx, err := q.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `DELETE FROM guilds WHERE id=?`, guildID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrGuildNotConfigured
	}
	if err := cancelUnsubscribed(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	q.signal()
	return nil
}

func (q *Outbox) RemoveAccount(ctx context.Context, userID, provider string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validUserProvider(userID, provider) {
		return ErrInvalidPoll
	}
	result, err := q.store.db.ExecContext(ctx, `DELETE FROM accounts WHERE user_id=? AND provider=?`, userID, provider)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrAccountNotFound
	}
	q.signal()
	return nil
}

// DisconnectUser removes the shared Discord server membership, not an account.
// Both providers stop here, while other servers and account baselines survive.
func (q *Outbox) DisconnectUser(ctx context.Context, userID, guildID string) error {
	_, err := q.disconnect(ctx, Membership{UserID: userID, GuildID: guildID}, false)
	return err
}

// DisconnectIfCurrent applies a positive departure check only to the exact
// membership that was checked. A link/reassertion invalidates older checks.
func (q *Outbox) DisconnectIfCurrent(ctx context.Context, member Membership) (bool, error) {
	return q.disconnect(ctx, member, true)
}
func (q *Outbox) disconnect(ctx context.Context, member Membership, conditional bool) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !validNumericID(member.UserID) || !validNumericID(member.GuildID) || member.Generation < 0 {
		return false, ErrInvalidPoll
	}
	tx, err := q.store.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	query := `DELETE FROM guild_users WHERE user_id=? AND guild_id=?`
	args := []any{member.UserID, member.GuildID}
	if conditional {
		query += ` AND generation=?`
		args = append(args, member.Generation)
	}
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		if conditional {
			return false, nil
		}
		return false, ErrMembershipNotFound
	}
	if err := cancelUnsubscribed(ctx, tx); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	q.signal()
	return true, nil
}

func cancelUnsubscribed(ctx context.Context, tx *sql.Tx) error {
	// A subscription cannot retract a request already in flight. Its lease is
	// retained until outcome/expiry; only unleased or expired work is canceled.
	_, err := tx.ExecContext(ctx, `UPDATE deliveries SET state='canceled',last_error_code='target_removed',lease_token='',leased_until=0
		WHERE state IN ('pending','failed') AND (lease_token='' OR leased_until<=?) AND NOT EXISTS (
		SELECT 1 FROM events e JOIN accounts a ON a.id=e.account_id JOIN guild_users m ON m.user_id=a.user_id JOIN guilds g ON g.id=m.guild_id
		WHERE e.id=deliveries.event_id AND g.channel_id=deliveries.channel_id)`, time.Now().UTC().UnixMicro())
	return err
}

func validProvider(provider string) bool { return provider == "anilist" || provider == "mal" }
func validUserProvider(userID, provider string) bool {
	return validNumericID(userID) && validProvider(provider)
}

const accountColumns = `a.id,a.provider,a.external_id,a.user_id,a.username,a.features,a.colors,a.revision`

func scanSettings(row interface{ Scan(...any) error }) (AccountSummary, error) {
	state := AccountSummary{}
	var colors string
	err := row.Scan(&state.AccountID, &state.Provider, &state.ExternalID, &state.UserID, &state.Username, &state.Features, &colors, &state.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return state, ErrAccountNotFound
	}
	if err != nil {
		return state, err
	}
	if len(colors) > 64<<10 || json.Unmarshal([]byte(colors), &state.Colors) != nil || len(state.Colors) > 256 {
		return AccountSummary{}, ErrInvalidPoll
	}
	seen := map[uint8]bool{}
	for _, color := range state.Colors {
		if color.ColorValue > 0xffffff || seen[color.UpdateType] {
			return AccountSummary{}, ErrInvalidPoll
		}
		seen[color.UpdateType] = true
	}
	return state, nil
}

func (q *Outbox) Settings(ctx context.Context, userID, provider string) (AccountSummary, error) {
	if !validUserProvider(userID, provider) {
		return AccountSummary{}, ErrInvalidPoll
	}
	return scanSettings(q.store.db.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM accounts a WHERE a.user_id=? AND a.provider=?`, userID, provider))
}

func (q *Outbox) SetFeature(ctx context.Context, userID, provider string, flag uint64, enabled bool) error {
	maximum := uint64(1 << 15)
	if provider == "mal" {
		maximum = 1 << 16
	}
	if !validUserProvider(userID, provider) || flag == 0 || flag&(flag-1) != 0 || flag > maximum {
		return ErrInvalidPoll
	}
	return q.editSettings(ctx, userID, provider, func(state *AccountSummary) {
		if enabled {
			state.Features |= int64(flag)
		} else {
			state.Features &^= int64(flag)
		}
	})
}

// A nil color removes one override without discarding unrelated imported types.
func (q *Outbox) SetColor(ctx context.Context, userID, provider string, kind uint8, value *uint32) error {
	maximum := uint8(14)
	if provider == "mal" {
		maximum = 13
	}
	if !validUserProvider(userID, provider) || kind > maximum || (value != nil && *value > 0xffffff) {
		return ErrInvalidPoll
	}
	return q.editSettings(ctx, userID, provider, func(state *AccountSummary) {
		colors := make([]ColorOverride, 0, len(state.Colors)+1)
		for _, color := range state.Colors {
			if color.UpdateType != kind {
				colors = append(colors, color)
			}
		}
		if value != nil {
			colors = append(colors, ColorOverride{UpdateType: kind, ColorValue: *value})
		}
		slices.SortFunc(colors, func(a, b ColorOverride) int { return int(a.UpdateType) - int(b.UpdateType) })
		state.Colors = colors
	})
}

func (q *Outbox) editSettings(ctx context.Context, userID, provider string, edit func(*AccountSummary)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tx, err := q.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	before, err := scanSettings(tx.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM accounts a WHERE a.user_id=? AND a.provider=?`, userID, provider))
	if err != nil {
		return err
	}
	state := before
	state.Colors = slices.Clone(before.Colors)
	edit(&state)
	if before.Features == state.Features && slices.Equal(before.Colors, state.Colors) {
		return nil
	}
	if state.Revision == math.MaxInt64 {
		return ErrInvalidPoll
	}
	encoded, err := json.Marshal(state.Colors)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE accounts SET features=?,colors=?,revision=revision+1 WHERE id=? AND revision=?`, state.Features, string(encoded), state.AccountID, state.Revision)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return ErrStalePoll
	}
	return tx.Commit()
}

func (q *Outbox) ListAccounts(ctx context.Context, guildID, provider string, offset, limit int) ([]AccountSummary, bool, error) {
	if !validNumericID(guildID) || !validProvider(provider) || offset < 0 || offset > 100000 || limit < 1 || limit > 100 {
		return nil, false, ErrInvalidPoll
	}
	rows, err := q.store.db.QueryContext(ctx, `SELECT `+accountColumns+` FROM accounts a JOIN guild_users m ON m.user_id=a.user_id WHERE m.guild_id=? AND a.provider=? ORDER BY a.id LIMIT ? OFFSET ?`, guildID, provider, limit+1, offset)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	result := make([]AccountSummary, 0, limit)
	more := false
	for rows.Next() {
		state, err := scanSettings(rows)
		if err != nil {
			return nil, false, err
		}
		if len(result) == limit {
			more = true
			break
		}
		result = append(result, state)
	}
	return result, more, rows.Err()
}

func (q *Outbox) AccountFence(ctx context.Context) (int64, error) {
	var value int64
	err := q.store.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM accounts`).Scan(&value)
	return value, err
}
func (q *Outbox) ActiveAccountIDs(ctx context.Context, provider string, after, until int64, limit int) ([]int64, error) {
	if !validProvider(provider) || after < 0 || until < 0 || limit < 1 || limit > 1000 {
		return nil, ErrInvalidPoll
	}
	rows, err := q.store.db.QueryContext(ctx, `SELECT a.id FROM accounts a WHERE a.provider=? AND a.id>? AND a.id<=? AND EXISTS(SELECT 1 FROM guild_users m WHERE m.user_id=a.user_id) ORDER BY a.id LIMIT ?`, provider, after, until, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]int64, 0, limit)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	return result, rows.Err()
}

func (q *Outbox) Memberships(ctx context.Context, afterGuild, afterUser string, limit int) ([]Membership, error) {
	if (afterGuild != "" || afterUser != "") && (!validNumericID(afterGuild) || !validNumericID(afterUser)) {
		return nil, ErrInvalidPoll
	}
	if limit < 1 || limit > 1000 {
		return nil, ErrInvalidPoll
	}
	rows, err := q.store.db.QueryContext(ctx, `SELECT m.guild_id,m.user_id,m.generation FROM guild_users m WHERE (m.guild_id>? OR (m.guild_id=? AND m.user_id>?)) AND EXISTS(SELECT 1 FROM accounts a WHERE a.user_id=m.user_id) ORDER BY m.guild_id,m.user_id LIMIT ?`, afterGuild, afterGuild, afterUser, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Membership, 0, limit)
	for rows.Next() {
		var member Membership
		if err := rows.Scan(&member.GuildID, &member.UserID, &member.Generation); err != nil {
			return nil, err
		}
		result = append(result, member)
	}
	return result, rows.Err()
}

func (q *Outbox) Cleanup(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 1000 {
		return 0, ErrInvalidPoll
	}
	result, err := q.store.db.ExecContext(ctx, `DELETE FROM users WHERE id IN (SELECT u.id FROM users u WHERE NOT EXISTS(SELECT 1 FROM guild_users m WHERE m.user_id=u.id) ORDER BY u.id LIMIT ?)`, limit)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	if n > 0 {
		q.signal()
	}
	return int(n), err
}

func nextCounter(ctx context.Context, tx *sql.Tx, name string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `UPDATE runtime_counters SET value=value+1 WHERE name=? AND value<? RETURNING value`, name, int64(math.MaxInt64)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrInvalidPoll
	}
	return id, err
}
