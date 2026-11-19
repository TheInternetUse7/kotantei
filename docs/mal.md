# MyAnimeList provider

The MAL provider is implemented as Go APIs in `internal/mal`. Public lists use the official API at `https://api.myanimelist.net/v2` with an application client ID in `X-MAL-CLIENT-ID`. No user OAuth token, `@me` request, cookie login, or account mutation is needed.

Stable account IDs, current avatar URLs, and favorites come from public profile HTML. Optional catalogue enrichment uses [Tenrai](https://api.tenrai.org), an unofficial service following the Jikan v4 schema. Scraping and enrichment do not have to succeed for a previously linked account's ordinary public-list polling to continue.

`kotantei run` composes these APIs with slash commands, independent polling, and durable Arikawa delivery. Maintenance commands remain credential-free. See [Discord runtime and commands](discord.md).

## Go entry points

| API                                                    | Responsibility                                                                                                                       |
| ------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------ |
| `mal.NewClient(ClientOptions)`                         | Configure the MAL client ID, bounded official/profile requests, and shared pacing.                                                   |
| `mal.NewEnricher(EnricherOptions)`                     | Optionally create an unauthenticated Tenrai client with a bounded in-process cache.                                                  |
| `mal.NewProvider(outbox, client, ProviderOptions)`     | Attach polling to an initialized native outbox; current runtime schema is version 3. `ProviderOptions.Enricher` is nil by default.   |
| `Provider.Link(ctx, discordUserID, guildID, username)` | Link a public profile and quietly commit complete baselines. Return the stored account ID.                                           |
| `Provider.Check(ctx, accountID)`                       | Stage enabled streams and atomically record events/cursors/favorites/username. Return counts and warnings, without sending messages. |
| `Client.Search(ctx, kind, query, type)`                | Fetch one public official catalogue page (at most 25 candidates), filter locally, and return at most five results.                   |
| `Client.Close()` / `Enricher.Close()`                  | Release idle connections after all work has joined.                                                                                  |

`CheckResult` includes normal outbox event/delivery counts plus `ProfileFailures`, `FavoriteFailures`, `MetadataFailures`, `PrivateLists`, and `Renamed`. `PrivateLists` means an HTTP-403 private **or blocked** stream, not proof of the site's exact reason for denying access. Warning counts permit simpler core updates to succeed without hiding enrichment or scraper degradation.

The runtime shares one MAL client across tracking, linking, and search, and one enricher when opted in. It passes the configured client ID and request interval (default three seconds) and waits the cycle delay (default 50 seconds) after a completed provider cycle. Other collection/cache limits remain Go API options.

## Linking and identity recovery

The guild must already be configured. New links resolve the canonical public profile name and stable numeric ID, require a complete favorite baseline, and query both public lists only as far as the quiet linking boundary. Existing revisions between the start of the linking second and the precise time fence—including fractional timestamps—are consumed without historical notifications. Private/blocked lists may be quietly linked without reading their private contents.

Adding another shared server membership to an existing account does not refetch it or reset settings, checkpoints, favorites, revision, or deliveries. Account conflicts are rejected rather than overwritten. Linking is not an implicit account-switch operation.

When a profile is missing, its numeric ID disagrees with the saved identity, or a list endpoint returns 404, recovery uses `comments.php?id=STORED_ID`. Only the owner's header profile link is used—not commenter links. The candidate profile must then expose the saved numeric ID before lists are read under that candidate name. An API-triggered recovery discards previously staged list pages and performs one bounded recollection.

Recovered usernames commit together with a successful poll. Unverifiable, deleted, or reused identities return `ErrRenameRecovery`, preserving storage and providing a retry/profile-access/relink action rather than silently switching users.

If the website is temporarily blocked, times out, or has unrecognized HTML, an existing account's lists use its last verified username and an ID-based avatar fallback; favorites remain unchanged. Public list endpoints are username-based, so ownership cannot be independently revalidated during that outage. A rename followed by name reuse while identity pages are unavailable remains a limitation; positive identity mismatches always require verified recovery.

## Lists, favorites, and embeds

Anime and manga paginate independently using `sort=list_updated_at`, with NSFW entries included so filtering does not hide saved list changes. Pagination URLs must keep the configured origin, user/list path, and query scope and move forward by the requested page size. Requests are reconstructed locally rather than blindly following an upstream URL.

Polling includes the saved timestamp boundary, checks descending timestamp order, and stops only after passing into older data or reaching the end. Future positions beyond the collection's time fence are deferred. Missing required state or malformed pagination is an explicit error, not a silently consumed update.

Checkpoints use UTC Unix microseconds, preserving imported precision. Revision identities include media ID, timestamp, and a deterministic fingerprint of status, score, progress, repeat state/count, tags, comments, and dates. Display titles, cover images, and catalogue enrichment are excluded. Tags are treated as a sorted unique set. Repeated snapshots do not replay updates; changed state at the exact saved timestamp remains eligible.

Only the latest observed list state per title is posted in a complete collection. Other consumed revisions still contribute observations to the checkpoint. MAL is a mutable snapshot, not an activity history: intermediate edits, removals from lists, and changes that reverse entirely between polls cannot always be reconstructed. Public queries are not an atomic snapshot across pages.

Embeds show scores except zero, anime episode or manga chapter/volume progress, repeat counts, media format/status, and imported colors. Feature switches control genres, synopsis, studios, authors, tags, comments, dates, themes, demographic, and Japanese voice actors. Dates can retain partial year/month precision. Notes and text are bounded by the shared Discord UTF-16 and aggregate embed limits; only an explicitly selected Discord user is allowlisted for mentions.

Favorites cover anime, manga, characters, people, and companies/studios. Profile headings/counts must agree with parsed IDs, and the expected section/document structure must be intact. A missing, truncated, challenged, or limited preview is **not** an empty snapshot: `FavoriteFailures` increases and the old baseline is preserved. New linking instead rejects an incomplete favorite baseline. Favorite polling resumes when a complete recognized profile is available.

Imported favorite names/images and supplemental metadata are retained when current optional display data is missing. Add/remove/add cycles remain distinct. A deleted/unavailable title with an identifiable list revision is consumed without an embed; an unidentifiable malformed row cannot safely advance a cursor. HTTP 403 preserves that list's cursor and permits completed other public streams to continue. Authentication, server, malformed-data, budget, or other required failures discard the account's staged poll.

New accounts use features `127`, matching the familiar anime/manga/favorites/user-reference/footer/format/status defaults. Imported feature values are preserved. Color types `0`–`5` cover anime current/planning/completed/dropped/on-hold/repeating; `6`–`11` cover manga in the same order; `12`/`13` cover favorite additions/removals.

## Optional enrichment

Tenrai is opt-in through `mal.tenrai_enabled:true` in your JSONC configuration, or `ProviderOptions.Enricher` through Go APIs. It can supply favorite media details, themes/demographics, Japanese voice actors, character/person descriptions and related works, and a company's best-known catalogue work. The associated display features must also be enabled for those fields to appear. See [configuration](configuration.md).

The enricher sends no MAL client ID, OAuth token, cookie login, or server-key header. Requests target public catalogue IDs, not Tenrai user endpoints. Catalogue identity mismatches and malformed/oversized metadata fall back rather than replacing core data.

Core message capacity is reserved before enrichment. A separate short metadata context and retained-data allowance prevent an optional deadline or oversized field from discarding otherwise valid core notifications. Upstream cooldowns stop further display-only lookups rather than being waited out during a collection. Parent cancellation still prevents the whole poll from committing.

The cache is in-process, bounded by both entry count and logical encoded bytes, with least-recently-used eviction, success/failure TTLs, and detached returned values. Concurrent misses for a shared key are serialized. An account's collection-budget failure is not cached for other accounts.

## Limits and failure handling

Zero-valued options select these defaults; other numeric/duration values must be positive:

| Limit                                                |                       Default |
| ---------------------------------------------------- | ----------------------------: |
| Shared MAL request interval                          |                     3 seconds |
| HTTP deadline after pacing                           |                    15 seconds |
| Decoded HTTP response body                           |                         1 MiB |
| Profile/comments HTML hard limit                     |                         1 MiB |
| List items per page                                  | 100; allowed range `1`–`1000` |
| HTTP attempts per account collection                 |                            64 |
| Eligible list records plus favorite changes          |                           512 |
| Favorites across all categories                      |                         5,000 |
| Required retained data/core payload capacity         |                         4 MiB |
| Optional retained metadata allowance                 |                 At most 1 MiB |
| Whole link/check deadline                            |                     5 minutes |
| Profile/recovery-operation deadline, including waits |                     5 seconds |
| Collection-wide metadata deadline                    |                    10 seconds |
| Tenrai interval / lookup deadline                    |          1 second / 5 seconds |
| Tenrai cache entries / logical bytes                 |                    64 / 1 MiB |
| Tenrai cached value size                             |                        64 KiB |
| Tenrai success / failure TTL                         |            6 hours / 1 minute |

The request budget includes actual scraping/API/enrichment attempts and canonical profile redirects; cache hits use no HTTP attempt. Eligible fetched list records, including repeated boundary rows before deduplication, count toward record limits. Record, favorite, and required-byte limits are capped by the attached outbox; its default payload and per-favorite metadata cap remains 64 KiB.

Official and website requests share pacing but have independent cooldowns, so website failures do not impose their backoff on the official API. Server/transport failures back off from five to at most 90 seconds. A longer `Retry-After` is not shortened. Profile redirects are same-origin and limited to three; official/enrichment redirects are not followed. There are no application-level automatic HTTP retries beyond bounded canonical redirects and verified rename recollection.

Responses, required fields, UTC timestamps, and continuation scope are validated before recording. Raw upstream error text is not returned or stored. Failed required writes, cancellation, expired account deadlines, stale revisions, and active-outbox backpressure leave the account's baselines unchanged. Reload after `store.ErrStalePoll`; do not force old pages over newer settings.

Large catch-up feeds require appropriate collection/outbox limits; updates are not silently dropped to fit a budget. See [tracking and delivery](tracking.md) for retained delivery history.

## References

[MAL API v2](https://myanimelist.net/apiconfig/references/api/v2) and [Tenrai's OpenAPI schema](https://api.tenrai.org/openapi.json).
