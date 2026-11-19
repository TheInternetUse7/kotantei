package store

import (
	"context"
	"database/sql"
)

type ProviderHealth struct {
	Name                 string `json:"name"`
	Accounts             int    `json:"accounts"`
	SubscribedAccounts   int    `json:"subscribed_accounts"`
	LatestCheckpointUnix int64  `json:"latest_checkpoint_unix,omitempty"`
}
type Health struct {
	SchemaVersion int              `json:"schema_version"`
	Summary       Summary          `json:"summary"`
	Outbox        DeliveryStatus   `json:"outbox"`
	Providers     []ProviderHealth `json:"providers"`
}

type sqlReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// InspectHealth reads a coherent, aggregate-only storage snapshot. Saved
// checkpoints are not evidence that a provider or Discord process is alive.
func InspectHealth(ctx context.Context, path string) (Health, error) {
	db, err := OpenReadOnly(ctx, path)
	if err != nil {
		return Health{}, err
	}
	defer db.Close()
	version, err := inspectSchema(ctx, db)
	if err != nil {
		return Health{}, err
	}
	if version == 0 {
		return Health{}, ErrForeignDatabase
	}
	if version < 2 {
		return Health{}, ErrNeedsMigration
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Health{}, err
	}
	defer tx.Rollback()
	health := Health{SchemaVersion: version, Providers: []ProviderHealth{}}
	health.Summary, err = summarize(ctx, tx)
	if err != nil {
		return Health{}, err
	}
	health.Outbox, err = readDeliveryStatus(ctx, tx)
	if err != nil {
		return Health{}, err
	}
	rows, err := tx.QueryContext(ctx, `WITH providers(name) AS (VALUES ('anilist'),('mal'))
		SELECT p.name,COUNT(DISTINCT a.id),COUNT(DISTINCT CASE WHEN EXISTS(SELECT 1 FROM guild_users m WHERE m.user_id=a.user_id) THEN a.id END),
		COALESCE(MAX(CASE WHEN p.name='mal' THEN c.value/1000000 ELSE c.value END),0)
		FROM providers p LEFT JOIN accounts a ON a.provider=p.name LEFT JOIN checkpoints c ON c.account_id=a.id GROUP BY p.name ORDER BY p.name`)
	if err != nil {
		return Health{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var provider ProviderHealth
		if err := rows.Scan(&provider.Name, &provider.Accounts, &provider.SubscribedAccounts, &provider.LatestCheckpointUnix); err != nil {
			return Health{}, err
		}
		health.Providers = append(health.Providers, provider)
	}
	return health, rows.Err()
}
