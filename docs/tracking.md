# Tracking and delivery semantics

The shared tracking layer stays provider-independent and has no Discord SDK dependency. The [AniList](anilist.md) and [MAL](mal.md) providers supply complete observations and bounded Discord-compatible notification data. `kotantei run` composes independent provider schedules with a context-aware Arikawa sender. Recording a poll still does not itself send messages: delivery is a separate durable worker.

## Recording a poll

1. Capture `store.Outbox.Snapshot` before fetching. It includes the stable provider/Discord identities, username, feature/color settings, account revision, checkpoints, boundary identities, and favorite baseline.
2. Fetch all required pages against that captured state. `tracking.Collect` stages a generic bounded page stream; `tracking.Check` also captures the snapshot and records the result. AniList stages activity, review, and favorite streams independently; MAL stages enabled anime/manga streams and a verified complete profile-favorite snapshot when available.
3. Pass the complete batch to `store.Outbox.Record`, with its captured `ExpectedRevision` and observation time.
4. If account state changed meanwhile, recording returns `ErrStalePoll`. Fetch again from a new snapshot rather than forcing old data over newer settings/baselines.

No network operation occurs inside the recording transaction. Events, live subscription fan-out, checkpoints, any supplied complete favorite snapshot, and an optional recovered username commit together. A failed write or active-queue budget violation rolls everything back.

Commands that actually change an account's settings increment `accounts.revision` in the same transaction so a poll captured before that change is rejected. No-op settings writes do not increment it. Unlink/relink uses monotonically allocated durable account IDs, so an old in-flight poll cannot attach to a different newly linked account after deletion or restart.

Every upstream item whose position is consumed must be represented by either an event or a non-notifying `update.Observation`. Use observations for records that cannot produce an embed, such as deleted media, missing/private list entries, and private draft reviews. Do not supply only the highest position from a partially fetched page stream.

MAL public-list responses with HTTP 403 are treated as private/blocked streams: that stream's staged pages are discarded and its cursor is preserved, while other completed public streams may be recorded. Scraper failures preserve favorite baselines rather than generating removals; failed verified-identity recovery stops the account poll. Other required-page errors still discard the entire poll.

List notifications are consolidated to the latest update per media title **within the complete poll**. Reviews and favorite changes are not merged by title. All consumed list identities still contribute to the checkpoint, including coalesced notifications that were not posted separately.

`store.Outbox.Connect` atomically creates a new account with its complete fetched checkpoints/favorite baseline and configured server membership, without queuing historical notifications. Adding another shared server membership to an existing account preserves its settings, checkpoints, revision, and pending deliveries; it does not rebaseline the account. Conflicting provider identities are rejected rather than overwritten.

AniList and MAL payloads capture the collection's display settings. `notification.Payload` enforces individual and aggregate Discord UTF-16 text limits, filters unsafe URLs, and disables mentions except an explicitly selected Discord user. The Arikawa sender validates and preserves those captured payloads, including legal Unicode and black color overrides, rather than reformatting them with newer settings.

Membership is shared across providers. `removehere` disconnects both AniList and MAL in one server without changing account baselines or other servers. Provider `remove` instead deletes only that provider account globally. A positive member-departure check is applied only against its captured membership generation; a reasserted/relinked subscription invalidates an older check. See [command scopes and membership reconciliation](discord.md).

## Stable identities and timestamp boundaries

- AniList activity/review checkpoints use Unix seconds.
- MAL anime/manga checkpoints use UTC Unix microseconds.
- Each checkpoint retains the source identities consumed at its exact timestamp. Older positions are consumed; an equal-timestamp item is consumed only if its identity is already known.
- Provider queries should include the saved boundary, not blindly exclude every item whose timestamp equals it.

Use an upstream activity/review ID as `SourceID`. For MAL's mutable list entries, use an identity containing the media ID, update timestamp, and a deterministic revision/fingerprint of tracked list-status fields. Display-only metadata such as a renamed title must not create a new list revision.

The implemented MAL fingerprint covers status, score, progress including manga volumes, repeat state/count, normalized tag sets, comments, and start/finish dates. These list-status fields are requested regardless of display switches, so changing a feature does not change revision identity. Repeated snapshots are deduplicated; a changed score or progress at the exact saved timestamp remains observable.

Event keys are account-scoped and independent of display payloads. Favorites do not have an upstream event history: their keys include the captured account revision, and recording verifies additions/removals against the old and supplied new favorite ID sets. A favorite add/remove/add cycle is distinct, while repeating an unchanged snapshot is a no-op. Favorite notifications require a complete favorite snapshot in the same transaction.

Legacy checkpoints do not contain boundary identity sets. Imported timestamps are preserved, but an inclusive first poll may repeat updates exactly at an imported boundary. This conservative choice avoids silently skipping unseen same-timestamp updates; it does not replay the entire old list.

## Bounded collection and backpressure

Default outbox limits:

| Limit                                  | Default |
| -------------------------------------- | ------: |
| Raw events plus observations per poll  |     512 |
| Notification payload                   |  64 KiB |
| Retained batch data                    |   4 MiB |
| Pending channel deliveries             |   4,096 |
| Logical pending payload bytes          |  16 MiB |
| Target channels per account/poll       |     128 |
| Identities at one checkpoint timestamp |   4,096 |
| Favorite entries in a snapshot         |   5,000 |

`tracking.Collect` defaults to at most 64 pages, 512 records, 64 KiB per payload, and 4 MiB retained data. Repeated continuation tokens are rejected. A fetch error, cancellation, or budget violation discards the staged collection without committing a checkpoint. Adapters must also bound HTTP response bodies and parse pages correctly; the collector cannot limit allocations performed inside an adapter's fetch function.

AniList also bounds individual HTTP responses and the whole account deadline. Its request budget includes profile, detail, and enrichment queries, not just pagination. Core payload capacity is reserved before optional metadata, which has a separate bounded allowance and falls back when it cannot fit. See [AniList limits](anilist.md#limits-and-rate-handling).

MAL also reserves core payload capacity before optional metadata. Its collection budget counts actual HTTP attempts, including scraping, canonical redirects, rename recovery, and uncached enrichment. Tenrai uses a separately bounded cache, short deadlines, and fail-open display enrichment; an account-local request-budget error is not cached as a shared upstream failure. See [MAL limits](mal.md#limits-and-failure-handling).

Favorite metadata is checked against the payload and remaining batch budgets before decoding and again after encoding, since JSON escaping can increase its size.

Limits can be supplied through the shared Go APIs. A budget error is explicit: data is not silently truncated and checkpoints do not advance beyond unqueued updates. Large catch-up feeds require appropriate collection limits or a provider-specific bounded catch-up strategy.

Pending payload bytes are counted per channel delivery, including leased work. Sent, canceled, and permanently failed delivery history is retained for audit/deduplication/recovery and is not included in the active queue limit. Keep this retained history in mind when planning storage and backups.

## Dispatch, retries, and leases

`delivery.Worker` accepts a sender implementing:

```go
Send(context.Context, store.Delivery) (messageID string, err error)
```

The sender must respect context cancellation and return a valid Discord message ID on confirmed success. It may classify failures using `delivery.Failure`, with a `store.Reason` and optional `RetryAfter`.

Defaults are one worker, a 30-second lease, a 15-second send timeout, and a 5-second storage cleanup deadline. The lease must exceed the combined send/storage deadlines. Worker pools are capped at 16; cooperating workers serialize unexpired channel leases. Senders must honor timeouts: lease expiry alone cannot prove a remote request has stopped. The application is intended to run as one instance against a local SQLite database.

Workers load one payload at a time. Idle workers use a bounded wake-up hint plus a one-second timer for retry/lease recovery. A retry at a channel's head holds later messages in that channel, but other channels can continue. Dispatch is ordered by event time; ties put numeric source IDs in numeric order before opaque IDs in lexical order. Already-posted messages cannot be retrospectively reordered if older data arrives later.

Transient failures use exponential backoff starting at five seconds, capped at five minutes, with up to ±20% jitter. An upstream retry-after is never shortened to that cap. Raw transport error bodies are neither stored nor logged; failure codes are used instead.

Lease ownership is checked when recording outcomes. A worker holding an obsolete lease cannot acknowledge or change a replacement worker's claim. Process crashes leave durable pending work that becomes claimable after lease expiry. Clean cancellation releases interrupted sends without waiting for expiry; a received success is acknowledged under a short cancellation-independent storage deadline.

## Permanent failures and recovery

- `missing_permissions` and `unknown_channel` block that channel and retain its unconfirmed deliveries as failed. New events still reach healthy channels; failed records for the blocked channel are retained rather than repeatedly hitting Discord.
- `invalid_message` fails only the affected delivery, not the whole channel.
- Pending targets whose account no longer has a live subscription to the channel are canceled before dispatch. Removing a subscription cannot retract an in-flight request; its active lease is kept until an outcome is recorded or it expires. Account deletion cascades through its events and deliveries.
- A channel change does not replay acknowledged history or retarget pending messages. `/sm update` cancels obsolete unleased/expired targets, preserves active leases, and routes newly recorded events to the new channel.

Inspect failures with:

```sh
kotantei outbox
kotantei health
```

The status includes counts and up to 100 blocked channel IDs/reasons. `more_channels` indicates that the list was bounded. `leased` counts persisted unacknowledged claims, including expired leases awaiting recovery. No message payload, account username, or lease token is exposed.

After repairing a channel, explicitly resume it:

```sh
kotantei resume-channel --channel CHANNEL_ID
```

This requeues only that channel's permission/availability failures. Sent deliveries and individual invalid-message failures remain untouched. If resuming would exceed pending budgets, nothing changes; choose an appropriate recovery strategy rather than dropping records. Resuming does not send anything unless the runtime is running and ready. `/sm resume` is the permission-checked Discord equivalent for the current configured channel.

## The unavoidable ambiguous-send window

SQLite and Discord cannot commit in one shared transaction. If Discord accepts a message and then the request times out, the process dies, the returned receipt is invalid, or the local acknowledgment cannot be saved, the delivery remains unconfirmed and may be retried. An occasional duplicate is therefore possible.

Confirmed channel deliveries are not replayed during another channel's retry or explicit recovery. This is at-least-once processing for transient/unconfirmed sends, with explicit terminal failures/cancellations—not an exactly-once guarantee or a promise that inaccessible channels will eventually receive messages without operator action.
