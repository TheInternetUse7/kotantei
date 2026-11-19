package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"kotantei/internal/update"
)

type Reason string

const (
	SendFailed         Reason = "send_failed"
	SendTimeout        Reason = "timeout"
	MissingPermissions Reason = "missing_permissions"
	UnknownChannel     Reason = "unknown_channel"
	InvalidMessage     Reason = "invalid_message"
	TargetRemoved      Reason = "target_removed"
)

type DeliveryRef struct {
	ID    int64
	Lease string
}
type Delivery struct {
	ID         int64
	Lease      string
	LeaseUntil time.Time
	ChannelID  string
	AccountID  int64
	UserID     string
	Provider   string
	Key        string
	Event      update.Event
	Attempts   int64
}

func (d Delivery) Ref() DeliveryRef { return DeliveryRef{ID: d.ID, Lease: d.Lease} }

type ChannelFailure struct {
	ChannelID string    `json:"channel_id"`
	Reason    Reason    `json:"reason"`
	Since     time.Time `json:"since"`
}
type DeliveryStatus struct {
	Pending         int              `json:"pending"`
	PendingBytes    int64            `json:"pending_bytes"`
	Leased          int              `json:"leased"`
	Sent            int              `json:"sent"`
	Failed          int              `json:"failed"`
	Canceled        int              `json:"canceled"`
	BlockedChannels int              `json:"blocked_channels"`
	Channels        []ChannelFailure `json:"channels"`
	MoreChannels    bool             `json:"more_channels,omitempty"`
}

// Claim acquires one expiring lease. A pending retry at a channel's head holds
// later messages there, but not messages in other channels. The first statement
// is a write, so concurrent claimers cannot race a deferred read transaction.
func (q *Outbox) Claim(ctx context.Context, now time.Time, lease time.Duration) (*Delivery, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validDeliveryTime(now) || lease < time.Microsecond || lease > 24*time.Hour {
		return nil, errors.New("invalid delivery claim time/lease duration")
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(random[:])
	tx, err := q.store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// A subscription change cannot retract a request already in flight. Keep
	// its active lease so another worker cannot send to that channel concurrently.
	_, err = tx.ExecContext(ctx, `UPDATE deliveries SET state='canceled',lease_token='',leased_until=0,last_error_code='target_removed'
		WHERE id IN (SELECT d.id FROM deliveries d JOIN events e ON e.id=d.event_id JOIN accounts a ON a.id=e.account_id
		WHERE d.state='pending' AND (d.lease_token='' OR d.leased_until<=?) AND NOT EXISTS (
			SELECT 1 FROM guild_users m JOIN guilds g ON g.id=m.guild_id WHERE m.user_id=a.user_id AND g.channel_id=d.channel_id
		) ORDER BY d.id LIMIT ?)`, now.UnixMicro(), q.limits.MaxRecords)
	if err != nil {
		return nil, err
	}
	result := &Delivery{Lease: token, LeaseUntil: now.Add(lease).UTC()}
	var eventID int64
	err = tx.QueryRowContext(ctx, `UPDATE deliveries SET lease_token=?,leased_until=?,attempts=attempts+1 WHERE id=(
		SELECT d.id FROM deliveries d JOIN events e ON e.id=d.event_id JOIN accounts a ON a.id=e.account_id
		WHERE d.state='pending' AND d.available_at<=? AND (d.lease_token='' OR d.leased_until<=?)
		AND NOT EXISTS (SELECT 1 FROM delivery_channels c WHERE c.channel_id=d.channel_id)
		AND EXISTS (SELECT 1 FROM guild_users m JOIN guilds g ON g.id=m.guild_id WHERE m.user_id=a.user_id AND g.channel_id=d.channel_id)
		AND NOT EXISTS (SELECT 1 FROM deliveries active WHERE active.channel_id=d.channel_id AND active.state='pending' AND active.lease_token<>'' AND active.leased_until>?)
		AND NOT EXISTS (SELECT 1 FROM deliveries earlier JOIN events older ON older.id=earlier.event_id
			WHERE earlier.channel_id=d.channel_id AND earlier.state='pending'
			AND (older.occurred_at<e.occurred_at OR (older.occurred_at=e.occurred_at AND older.id<e.id)))
		ORDER BY e.occurred_at,e.id,d.id LIMIT 1
	) RETURNING id,event_id,channel_id,attempts`, token, result.LeaseUntil.UnixMicro(), now.UnixMicro(), now.UnixMicro(), now.UnixMicro()).Scan(&result.ID, &eventID, &result.ChannelID, &result.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		// Commit any bounded cancellation cleanup even if nothing is ready.
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var payload string
	var occurredAt int64
	err = tx.QueryRowContext(ctx, `SELECT e.account_id,a.user_id,a.provider,e.event_key,e.stream,e.kind,e.source_id,e.subject_kind,e.subject_id,e.occurred_at,e.payload
		FROM events e JOIN accounts a ON a.id=e.account_id WHERE e.id=?`, eventID).Scan(&result.AccountID, &result.UserID, &result.Provider, &result.Key, &result.Event.Stream, &result.Event.Kind, &result.Event.SourceID, &result.Event.Subject.Kind, &result.Event.Subject.ID, &occurredAt, &payload)
	if err != nil {
		return nil, err
	}
	result.Event.Payload = json.RawMessage(payload)
	result.Event.OccurredAt = time.UnixMicro(occurredAt).UTC()
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

func (q *Outbox) Acknowledge(ctx context.Context, ref DeliveryRef, messageID string, now time.Time) error {
	if !validNumericID(messageID) || !validDeliveryTime(now) {
		return errors.New("invalid delivery send receipt/time")
	}
	return q.finish(ctx, ref, `UPDATE deliveries SET state='sent',message_id=?,sent_at=?,last_error_code='',lease_token='',leased_until=0 WHERE id=? AND lease_token=? AND state='pending'`, messageID, now.UTC().UnixMicro())
}

func (q *Outbox) Retry(ctx context.Context, ref DeliveryRef, next time.Time, reason Reason) error {
	if (reason != SendFailed && reason != SendTimeout) || !validDeliveryTime(next) {
		return errors.New("invalid delivery retry code/time")
	}
	return q.finish(ctx, ref, `UPDATE deliveries SET available_at=?,last_error_code=?,lease_token='',leased_until=0 WHERE id=? AND lease_token=? AND state='pending'`, next.UTC().UnixMicro(), reason)
}

func (q *Outbox) Release(ctx context.Context, ref DeliveryRef) error {
	return q.finish(ctx, ref, `UPDATE deliveries SET lease_token='',leased_until=0 WHERE id=? AND lease_token=? AND state='pending'`)
}

func (q *Outbox) Fail(ctx context.Context, ref DeliveryRef, reason Reason) error {
	if reason != InvalidMessage {
		return errors.New("invalid permanent delivery failure code")
	}
	return q.finish(ctx, ref, `UPDATE deliveries SET state='failed',last_error_code=?,lease_token='',leased_until=0 WHERE id=? AND lease_token=? AND state='pending'`, reason)
}

func (q *Outbox) finish(ctx context.Context, ref DeliveryRef, query string, args ...any) error {
	if ref.ID <= 0 || ref.Lease == "" {
		return ErrLeaseLost
	}
	args = append(args, ref.ID, ref.Lease)
	result, err := q.store.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrLeaseLost
	}
	q.signal()
	return nil
}

// BlockChannel retains failures for explicit operator recovery instead of
// repeatedly hammering a deleted/inaccessible target or replaying healthy ones.
func (q *Outbox) BlockChannel(ctx context.Context, ref DeliveryRef, reason Reason, now time.Time) error {
	if (reason != MissingPermissions && reason != UnknownChannel) || !validDeliveryTime(now) {
		return errors.New("invalid channel failure code/time")
	}
	if ref.ID <= 0 || ref.Lease == "" {
		return ErrLeaseLost
	}
	tx, err := q.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var channel string
	if err := tx.QueryRowContext(ctx, `SELECT channel_id FROM deliveries WHERE id=? AND lease_token=? AND state='pending'`, ref.ID, ref.Lease).Scan(&channel); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLeaseLost
		}
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO delivery_channels(channel_id,blocked_reason,blocked_at) VALUES (?,?,?) ON CONFLICT(channel_id) DO UPDATE SET blocked_reason=excluded.blocked_reason,blocked_at=excluded.blocked_at`, channel, reason, now.UTC().UnixMicro()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE deliveries SET state='failed',last_error_code=?,lease_token='',leased_until=0 WHERE channel_id=? AND state='pending'`, reason, channel); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	q.signal()
	return nil
}

func (q *Outbox) ResumeChannel(ctx context.Context, channelID string, now time.Time) (int, error) {
	if !validNumericID(channelID) || !validDeliveryTime(now) {
		return 0, errors.New("invalid channel recovery identity/time")
	}
	tx, err := q.store.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var count, bytesToResume int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(e.payload_bytes),0) FROM deliveries d JOIN events e ON e.id=d.event_id
		WHERE d.channel_id=? AND d.state='failed' AND d.last_error_code IN ('missing_permissions','unknown_channel')`, channelID).Scan(&count, &bytesToResume); err != nil {
		return 0, err
	}
	if err := q.checkCapacity(ctx, tx, count, bytesToResume); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE deliveries SET state='pending',available_at=?,last_error_code='',lease_token='',leased_until=0
		WHERE channel_id=? AND state='failed' AND last_error_code IN ('missing_permissions','unknown_channel')`, now.UTC().UnixMicro(), channelID); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM delivery_channels WHERE channel_id=?`, channelID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	q.signal()
	return int(count), nil
}

// Leased counts persisted unacknowledged claims, including expired ones that
// will be recovered by Claim. Status deliberately excludes payloads and tokens.
func (q *Outbox) Status(ctx context.Context) (DeliveryStatus, error) {
	return deliveryStatus(ctx, q.store.db)
}

func deliveryStatus(ctx context.Context, db *sql.DB) (DeliveryStatus, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return DeliveryStatus{}, err
	}
	defer tx.Rollback()
	return readDeliveryStatus(ctx, tx)
}

func readDeliveryStatus(ctx context.Context, tx sqlReader) (DeliveryStatus, error) {
	status := DeliveryStatus{Channels: []ChannelFailure{}}
	err := tx.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(CASE WHEN d.state='pending' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN d.state='pending' THEN e.payload_bytes ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN d.state='pending' AND d.lease_token<>'' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN d.state='sent' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN d.state='failed' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN d.state='canceled' THEN 1 ELSE 0 END),0),
		(SELECT COUNT(*) FROM delivery_channels)
		FROM deliveries d JOIN events e ON e.id=d.event_id`).Scan(&status.Pending, &status.PendingBytes, &status.Leased, &status.Sent, &status.Failed, &status.Canceled, &status.BlockedChannels)
	if err != nil {
		return DeliveryStatus{}, err
	}
	const maxChannels = 100
	rows, err := tx.QueryContext(ctx, `SELECT channel_id,blocked_reason,blocked_at FROM delivery_channels ORDER BY channel_id LIMIT ?`, maxChannels)
	if err != nil {
		return DeliveryStatus{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var failure ChannelFailure
		var since int64
		if err := rows.Scan(&failure.ChannelID, &failure.Reason, &since); err != nil {
			return DeliveryStatus{}, err
		}
		failure.Since = time.UnixMicro(since).UTC()
		status.Channels = append(status.Channels, failure)
	}
	status.MoreChannels = status.BlockedChannels > len(status.Channels)
	return status, rows.Err()
}

func validNumericID(id string) bool {
	parsed, err := strconv.ParseUint(id, 10, 64)
	return err == nil && parsed > 0 && strconv.FormatUint(parsed, 10) == id
}

func validDeliveryTime(t time.Time) bool { return !t.IsZero() && t.Unix() >= 0 && t.Year() <= 9999 }

func InspectOutbox(ctx context.Context, path string) (DeliveryStatus, error) {
	db, err := OpenReadOnly(ctx, path)
	if err != nil {
		return DeliveryStatus{}, err
	}
	defer db.Close()
	version, err := inspectSchema(ctx, db)
	if err != nil {
		return DeliveryStatus{}, err
	}
	if version == 0 {
		return DeliveryStatus{}, ErrForeignDatabase
	}
	if version < 2 {
		return DeliveryStatus{}, ErrNeedsMigration
	}
	return deliveryStatus(ctx, db)
}
