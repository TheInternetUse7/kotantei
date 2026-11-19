# AniList provider

The AniList provider is implemented as Go APIs in `internal/anilist`. It uses public, unauthenticated POST requests to `https://graphql.anilist.co`; it does not request OAuth tokens or mutate an AniList account.

It records Discord-compatible messages in the transactional outbox. `kotantei run` wires tracking, linking, bounded public search, and Arikawa delivery. Users link through server slash commands; see [Discord runtime and commands](discord.md).

## Go entry points

| API                                                    | Responsibility                                                                                                                        |
| ------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------- |
| `anilist.NewClient(ClientOptions)`                     | Create one paced GraphQL client. Share it across tracking, linking, and searches.                                                     |
| `anilist.NewProvider(outbox, client, ProviderOptions)` | Attach bounded collection to an initialized native outbox; current runtime schema is version 3.                                       |
| `Provider.Link(ctx, discordUserID, guildID, username)` | Resolve a public username, silently baseline a new account, and return its stored account ID.                                         |
| `Provider.Check(ctx, accountID)`                       | Collect enabled streams, record them atomically, and return event/delivery counts plus `MetadataFailures`. It does not send messages. |
| `Client.Search(ctx, kind, title, format)`              | Search public anime/manga catalogue data with native format filtering; return at most five results.                                   |
| `Client.Close()`                                       | Release idle HTTP connections after all work has joined.                                                                              |

The guild must already be configured with `/sm set`. New links store AniList's stable numeric ID, not the username as an identity. Linking an already tracked Discord user only reasserts the shared guild membership, without refetching or resetting anything; the supplied username is not an account-switch operation. Switching accounts requires a confirmed global `/anilist user remove`, then a new quiet link.

New links capture complete favorite ID sets and quiet activity/review timestamp boundaries, including identities already present at that exact second. No historical messages are queued. Failure or cancellation before the storage transaction completes leaves no partially linked account.

## Tracked behavior

- **List activities:** poll enabled anime/manga activities, then fetch current list entries only for titles with unseen activities. The latest activity for each title produces one notification; coalesced activities still contribute to its checkpoint. A score-only list change does not trigger a query or notification by itself.
- **Preferences and embeds:** respect the user's title language, stylised titles when available, and all five score formats. Show activity progress, current list status/score, media totals, repeats, notes, and enabled advanced scoring/custom lists. List details describe the current entry, not a historical snapshot at activity creation time.
- **Optional enrichment:** descriptions, genres, tags with spoiler markers, studios, manga staff, directors, and Japanese voice actors are feature-controlled. Favorite names/images and related works are best effort. Missing, rejected, or oversized enrichment falls back to core fields or an ID-based favorite name/link.
- **Favorites:** compare complete snapshots of anime, manga, characters, staff, and studios. Preserve imported names/images/metadata when available. Additions and removals are separate events; changes that happen and reverse entirely between polls cannot be reconstructed.
- **Reviews:** optional new-review notifications use creation time and review ID, with a summary and link. Reviews are disabled by default; edits to an already consumed review do not create another notification.
- **Renames:** look up an existing account by numeric ID and commit its current username together with a successful poll. A username change alone does not create a notification.
- **Private/deleted data:** consume visible activity/review identities without posting when media or the public list entry is missing, an entry is private, or a review is a private draft. Private contents are not saved in the outbox. An unavailable account or failed required query instead leaves its entire poll uncommitted.

Activities, reviews, and each favorite category paginate independently. Activity queries use the documented `ID_DESC` sort, include the saved timestamp boundary, and apply an upper time fence. Reviews use descending creation time and retain all unseen identities at the saved boundary. Repeated/incomplete pagination is rejected rather than accepted as a complete collection.

AniList activity/review timestamps use Unix seconds and GraphQL's signed-32-bit integer bounds. Imported legacy checkpoints lack boundary ID sets, so the first inclusive poll may conservatively repeat updates exactly at an imported boundary. It does not rebaseline or replay the whole old list.

AniList does not provide a transactionally consistent snapshot across these requests. Concurrent remote edits/deletions and list details changing during pagination remain upstream limitations; the time fence cannot make separate public queries atomic.

## Features and color compatibility

New accounts use feature value `127`: anime, manga, favorites, user-reference formatting, website footer, media format, and media status are enabled. Reviews use bit `4096` (`1 << 12`) and are off by default. Additional metadata and custom-list flags are off by default. Imported feature settings are preserved, including an imported review opt-in.

Imported color update types remain compatible:

| Types    | Updates, in order                                               |
| -------- | --------------------------------------------------------------- |
| `0`–`5`  | Anime: current, planning, completed, dropped, paused, repeating |
| `6`–`11` | Manga: current, planning, completed, dropped, paused, repeating |
| `12`     | Favorite added                                                  |
| `13`     | Favorite removed                                                |
| `14`     | New review                                                      |

Payloads contain one bounded embed. Text is clipped using Discord's UTF-16 field and 6,000-unit aggregate limits. Only HTTPS URLs without embedded credentials are accepted. User-reference formatting adds a `By` field and allowlists only that Discord user; role/everyone mentions and reply pings remain disabled.

## Limits and rate handling

Zero-valued options select these defaults; other values must be positive:

| Limit                                                   |       Default |
| ------------------------------------------------------- | ------------: |
| Shared client request ceiling                           |     29/minute |
| HTTP request deadline, after pacing                     |    15 seconds |
| Decoded HTTP response body                              |         1 MiB |
| Items per page                                          |            25 |
| GraphQL requests per account collection                 |            64 |
| Activity/review records plus favorite changes           |           512 |
| Favorites across all categories                         |         5,000 |
| Required retained data and reserved core payloads       |         4 MiB |
| Optional retained metadata allowance                    | At most 1 MiB |
| Whole link/check deadline, including queue/pacing waits |     3 minutes |

`ProviderOptions.MaxPages` counts **all GraphQL requests**, including the profile, list details, favorite details, and enrichment—not just paginated pages. Records include fetched activity/review duplicates before deduplication; favorites have a separate snapshot-entry cap. Page size is restricted to `1`–`25`. Record, favorite, and required-byte options are capped by the attached outbox's limits; its default per-payload/per-favorite metadata limit is 64 KiB.

Most limits are Go API options. The runtime passes `anilist.requests_per_minute` (`1`–`29`) to its single shared client and waits the configured cycle delay (default 90 seconds) after each provider cycle. Linking/search requests use the same pacing/cooldown as tracking. Other applications sharing the upstream quota remain outside this client's control. See [configuration](configuration.md).

AniList's [rate-limit documentation](https://docs.anilist.co/guide/rate-limiting) reports a temporary degraded limit of 30 requests/minute. Kotantei retains 29 rather than increasing automatically to the normal advertised 90. Lower response limits reduce pacing; `X-RateLimit-Remaining`, `X-RateLimit-Reset`, and `Retry-After` can impose longer cooldowns, including HTTP-200 GraphQL rate errors.

Requests are serialized, cancellable, and never automatically retried after failure. Failed connections still consume pacing. Server/transport failures back off from five seconds to at most 90 seconds; an existing longer rate cooldown is not shortened. An optional lookup that triggers a server/rate cooldown stops the remaining display-only requests in that collection, allowing already collected core updates to be recorded without waiting through the outage.

GraphQL errors discard partial data. Raw upstream error text is not returned or stored. Failed required pages, malformed data, cancellation, deadline expiry, stale account revisions, and active-outbox backpressure do not advance checkpoints or replace favorite baselines. Callers must reload after `store.ErrStalePoll` rather than force a stale collection through.

Core payload space is reserved before enrichment. Required budget errors remain explicit; updates are not silently skipped to fit a large catch-up feed. See [tracking and delivery](tracking.md) for collection limits and retained delivery history.

## References

[Query](https://docs.anilist.co/reference/query), [page](https://docs.anilist.co/reference/object/page), [list activity](https://docs.anilist.co/reference/object/listactivity), [media list](https://docs.anilist.co/reference/object/medialist), and [review](https://docs.anilist.co/reference/object/review).
