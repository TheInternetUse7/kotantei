package store

// Discord and provider identifiers use TEXT to preserve integer precision
// without depending on SQLite's signed 64-bit INTEGER range.
const schemaV1 = `
PRAGMA application_id=1263490126;
CREATE TABLE schema_migrations (
    version INTEGER PRIMARY KEY,
    applied_at INTEGER NOT NULL
);
CREATE TABLE users (
    id TEXT PRIMARY KEY NOT NULL
);
CREATE TABLE guilds (
    id TEXT PRIMARY KEY NOT NULL,
    channel_id TEXT NOT NULL
);
CREATE TABLE guild_users (
    guild_id TEXT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    PRIMARY KEY(guild_id,user_id)
);
CREATE INDEX guild_users_by_user ON guild_users(user_id);
CREATE TABLE accounts (
    id INTEGER PRIMARY KEY,
    provider TEXT NOT NULL CHECK(provider IN ('anilist','mal')),
    external_id TEXT NOT NULL,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    username TEXT NOT NULL DEFAULT '',
    features INTEGER NOT NULL DEFAULT 127 CHECK(features >= 0),
    colors TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(colors) AND json_type(colors)='array'),
    UNIQUE(provider,external_id),
    UNIQUE(provider,user_id)
);
CREATE INDEX accounts_by_user ON accounts(user_id);
CREATE TABLE checkpoints (
    account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK(kind IN ('activity','review','anime','manga')),
    value INTEGER NOT NULL,
    PRIMARY KEY(account_id,kind)
);
CREATE TABLE favorites (
    account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK(kind IN ('anime','manga','character','staff','studio','person','company')),
    external_id TEXT NOT NULL,
    name TEXT NOT NULL DEFAULT '',
    name_url TEXT NOT NULL DEFAULT '',
    image_url TEXT NOT NULL DEFAULT '',
    extra TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(extra) AND json_type(extra)='object'),
    PRIMARY KEY(account_id,kind,external_id)
);
CREATE TABLE imports (
    source_path TEXT PRIMARY KEY NOT NULL,
    report TEXT NOT NULL CHECK(json_valid(report)),
    imported_at INTEGER NOT NULL
);
`

// Cursors keep the source identities at their exact high-water timestamp.
// Delivery leases and acknowledgments are durable and independent per channel.
const schemaV2 = `
ALTER TABLE accounts ADD COLUMN revision INTEGER NOT NULL DEFAULT 0 CHECK(revision >= 0);
CREATE TABLE checkpoint_items (
    account_id INTEGER NOT NULL,
    kind TEXT NOT NULL,
    source_id TEXT NOT NULL,
    PRIMARY KEY(account_id,kind,source_id),
    FOREIGN KEY(account_id,kind) REFERENCES checkpoints(account_id,kind) ON DELETE CASCADE
);
CREATE TABLE events (
    id INTEGER PRIMARY KEY,
    account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    event_key TEXT NOT NULL UNIQUE CHECK(length(event_key)=64),
    stream TEXT NOT NULL CHECK(stream IN ('activity','review','anime','manga','favorites')),
    kind TEXT NOT NULL CHECK(kind IN ('list','review','favorite_added','favorite_removed')),
    source_id TEXT NOT NULL,
    subject_kind TEXT NOT NULL CHECK(subject_kind IN ('anime','manga','character','staff','studio','person','company')),
    subject_id TEXT NOT NULL,
    occurred_at INTEGER NOT NULL,
    payload TEXT NOT NULL CHECK(json_valid(payload) AND json_type(payload)='object'),
    payload_bytes INTEGER GENERATED ALWAYS AS (length(CAST(payload AS BLOB))) STORED,
    created_at INTEGER NOT NULL
);
CREATE INDEX events_by_account ON events(account_id,occurred_at,id);
CREATE TABLE deliveries (
    id INTEGER PRIMARY KEY,
    event_id INTEGER NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    channel_id TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','sent','failed','canceled')),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK(attempts >= 0),
    available_at INTEGER NOT NULL,
    lease_token TEXT NOT NULL DEFAULT '',
    leased_until INTEGER NOT NULL DEFAULT 0,
    last_error_code TEXT NOT NULL DEFAULT '' CHECK(last_error_code IN ('','send_failed','timeout','missing_permissions','unknown_channel','invalid_message','target_removed')),
    message_id TEXT NOT NULL DEFAULT '',
    sent_at INTEGER,
    created_at INTEGER NOT NULL,
    UNIQUE(event_id,channel_id),
    CHECK((lease_token='' AND leased_until=0) OR (length(lease_token)=32 AND leased_until>0)),
    CHECK(state='pending' OR lease_token=''),
    CHECK((state='sent' AND message_id<>'' AND sent_at IS NOT NULL) OR (state<>'sent' AND message_id='' AND sent_at IS NULL))
);
CREATE INDEX deliveries_due ON deliveries(state,available_at,leased_until,id);
CREATE INDEX deliveries_by_channel ON deliveries(channel_id,state,event_id);
CREATE TABLE delivery_channels (
    channel_id TEXT PRIMARY KEY NOT NULL,
    blocked_reason TEXT NOT NULL CHECK(blocked_reason IN ('missing_permissions','unknown_channel')),
    blocked_at INTEGER NOT NULL
);
`

// Durable generations prevent deleted/relinked rows from looking like an old
// in-flight account poll or an old membership departure check after restart.
const schemaV3 = `
CREATE TABLE runtime_counters (
    name TEXT PRIMARY KEY NOT NULL CHECK(name IN ('account','membership')),
    value INTEGER NOT NULL CHECK(value>=0)
);
INSERT INTO runtime_counters(name,value) SELECT 'account',COALESCE(MAX(id),0) FROM accounts;
INSERT INTO runtime_counters(name,value) VALUES ('membership',0);
ALTER TABLE guild_users ADD COLUMN generation INTEGER NOT NULL DEFAULT 0 CHECK(generation>=0);
CREATE TRIGGER account_id_highwater AFTER INSERT ON accounts BEGIN
    UPDATE runtime_counters SET value=MAX(value,NEW.id) WHERE name='account';
END;
`
