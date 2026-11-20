# Discord runtime and commands

Kotantei uses pinned Arikawa v3.6.0 for its REST/gateway protocol. It runs one process, one gateway shard, one independent loop per enabled provider, and one durable delivery worker.

## Setup and startup

Install a Discord bot with the `bot` and `applications.commands` scopes. The selected text/announcement channel needs **View Channel**, **Send Messages**, and **Embed Links** for the bot. Threads and categories are not supported posting targets. Do not enable privileged member, presence, or message-content intents for Kotantei: only the guild intent is requested.

```sh
kotantei --config config.jsonc run
```

Set `discord.token` and, when MAL is enabled, `mal.client_id` in your private `config.jsonc`. Set `mal.enabled` to `false` for an AniList-only runtime. Optional `mal.tenrai_enabled` defaults to `false`. See [configuration](configuration.md) for every setting.

Startup order is deliberate:

1. Validate local configuration/credential shape before creating storage.
2. Open/upgrade Kotantei SQLite and complete any eligible legacy auto-import. Stop the old bot first and keep a consistent backup. Empty native storage may import the configured source; an existing successful import is idempotent. A populated native destination with no import marker is not overwritten by a newly dropped source. Explicit `import`/`init` still report conflicting import requests.
3. Verify the token belongs to a bot and obtain its Discord application and actual application/team owner. Team membership alone does not grant owner authority.
4. Connect to the gateway and verify READY against that bot/application identity. Unsupported sharding or exhausted session-start limits fail startup clearly.
5. Bulk-register the supported global command set for this application and reconcile currently tracked server memberships.
6. Mark commands ready and start providers, durable delivery, and periodic reconciliation exactly once. Later READY/RESUMED events do not restart workers or rebaseline accounts.

The database/import is independent of the Discord application ID. A new bot can use imported subscriptions once installed in the relevant servers/channels; Kotantei does not require users to relink merely because application identity changed. Registration replaces this application's global command set, not another application's commands.

No guild/member/message/presence cache is kept. Instead, tracked users are checked individually through Discord REST before initial polling/delivery and about hourly afterward. Only a positive `404 / unknown member` removes a subscription, and only if its captured membership generation is still current. Unknown guilds, blocked requests, timeouts, and malformed responses preserve subscriptions. Departure detection is therefore not instantaneous. If the bot itself has left a server, an owner can explicitly remove its retained tracking configuration.

Gateway dispatch sequence zero is valid; missing/null/negative sequences are not. Data-free heartbeat ACK/reconnect controls may omit `d`. An explicit session-start `reset_after:0` is accepted; positive reset durations must be at least 1 ms and fit safely in a Go duration, with no seven-day cutoff. Zero retains only the advertised remaining IDENTIFY allowance and does not invent an automatic refill period or bypass the session-start budget. Missing/null reset values are rejected with `reason:missing_or_null` rather than misreported as zero. Validated discovery logs the numeric reset period and whether IDENTIFY refills are enabled. Invalid discovery identifies the rejected field and includes `reset_after_ms` for invalid numeric durations, without printing tokens, URLs, or payloads. A gateway failure after a successful import does not undo or require repeating that import; keep the destination database and retry startup after correcting the error.

## Startup failure diagnostics

Failed Discord startup requests log `discord request failed` before the final `command failed` entry:

- `stage` and `operation` distinguish `identity_user` (`GET /users/@me`), `identity_application` (`GET /oauth2/applications/@me`), `gateway_discovery`, `register_commands`, and `membership_user` (a tracked guild-member lookup).
- `reason` classifies DNS, connection, proxy, TLS certificate, timeout, HTTP status, and JSON decoding failures. `phase` identifies the last observed HTTP phase, such as `connect`, `tls_handshake`, `response_headers`, `response_body`, or `decode`.
- `http_status` and `discord_code` retain numeric response details. An HTTP status of `0` means no response status was observed.
- `elapsed_ms`, `timeout_ms`, `response_bytes`, `response_limit_bytes`, and `retry_after_ms` report timing and bounds. `timeout_ms` is the tighter configured SDK/HTTP-client deadline; a parent context can cancel earlier. A retry-after value of `0` means no cooldown was recorded.
- `error_types` contains only Go type names; `os_error`, when present, is a numeric OS error code. Raw error messages, URLs, proxy credentials, headers, certificates, and response bodies are never logged.

Non-fatal membership failures log `tracked membership check failed` at WARN with the same safe diagnostics plus numeric `guild_id` and `user_id`. A `404` with Discord code `10004` means an unknown or inaccessible guild, not a confirmed user departure; verify that the running bot is installed in the imported server. Failed checks preserve subscriptions, while only a confirmed unknown member (`10007`) can remove a current membership.

For a timeout in `response_headers`, check connectivity, proxy/firewall settings, and Discord availability: the request was written, but response headers did not arrive before the deadline. TLS certificate failures require checking the trust/proxy setup, not disabling certificate verification. Share the structured diagnostic entry when troubleshooting; do not share the bot token or private configuration.

## Commands and scopes

All replies are private and mention-disabled. Options below are shown as slash-command fields, not CLI arguments. Both provider roots remain registered when a provider is disabled: existing settings/removal operations still work, while new linking/search/polling for the disabled provider does not.

### Provider commands

Replace `PROVIDER` with `anilist` or `mal`.

| Command                                                | Scope and behavior                                                                                                                                      |
| ------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `/PROVIDER user add [username]`                        | Link your public account quietly, or reconnect an existing link here without refetching/resetting it. A username is required for a new link.            |
| `/PROVIDER user remove confirm:true`                   | **Globally unlink only that provider**, in every server, including its queued events/deliveries. Other provider accounts and shared memberships remain. |
| `/PROVIDER user removehere confirm:true`               | **Stop both AniList and MAL here**, not just the named provider. Other servers and account baselines remain.                                            |
| `/PROVIDER user list [page]`                           | List a bounded page of tracked accounts in this server, privately.                                                                                      |
| `/PROVIDER features list` / `enabled`                  | List supported features or your enabled flags.                                                                                                          |
| `/PROVIDER features enable feature:CHOICE` / `disable` | Change one of your own feature bits. Unknown imported bits remain preserved.                                                                            |
| `/PROVIDER colors list`                                | Show your color overrides.                                                                                                                              |
| `/PROVIDER colors set updatetype:CHOICE color:#RRGGBB` | Set one of your own colors; `000000` is valid black.                                                                                                    |
| `/PROVIDER colors remove updatetype:CHOICE`            | Restore that type's default without deleting other imported overrides.                                                                                  |

Settings changes preserve checkpoints and previously queued payloads. Actual changes increment the account revision; no-op writes do not. Switching provider identities requires a confirmed global unlink followed by a new quiet link. A deleted account ID is never reused for a later link, including after restart.

AniList reviews are optional and off under default features `127`. AniList's `4096` bit means reviews; MAL's same bit means comments. Color choices cover AniList types `0`–`14` and MAL `0`–`13`; unknown imported types are retained, not editable as supported choices.

Public catalogue search returns up to five links/metadata summaries, not an interactive selection session:

- `/anilist search anime title:TEXT [format]`
- `/anilist search manga title:TEXT [format]`
- `/mal search anime query:TEXT [type]`
- `/mal search manga query:TEXT [type]`

Search uses the same client's pacing/cooldown as linking and tracking. AniList filters format natively; MAL filters one bounded official candidate page locally, so restrictive filters may return fewer results even when additional catalogue pages contain matches.

### Server management

These require **Manage Server**, **Administrator**, or the verified bot owner. Non-owner manager authority is revalidated through REST guild/member roles; incomplete or failed permission reads fail closed. Discord's command visibility/default permissions are not the authorization boundary. An application owner without the default server permissions may need a server administrator to allow the commands under Discord's integration settings.

| Command                                          | Behavior                                                                                                                                         |
| ------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------ |
| `/sm set [channel]`                              | Create the first posting-channel configuration; default is the invoking channel.                                                                 |
| `/sm update [channel]`                           | Change the posting channel. Old queued posts are not moved or replayed. Obsolete unleased/expired targets are canceled; active sends may finish. |
| `/sm status`                                     | Show this server's channel/pending/failed state and current provider-cycle aggregates.                                                           |
| `/sm resume`                                     | Revalidate a repaired channel and requeue its permission/availability failures; acknowledged posts stay sent.                                    |
| `/sm removeserver confirm:true`                  | Remove this server's tracking configuration/memberships, not global accounts in other servers.                                                   |
| `/sm forceremoveuser user:USER confirm:true`     | Stop both providers for that user in this server.                                                                                                |
| `/sm forceremoveuserbyid userid:ID confirm:true` | The same operation using a full, unrounded Discord ID string.                                                                                    |
| `/say channel:CHANNEL text:TEXT`                 | Post one mention-disabled text embed after checking the target channel.                                                                          |

### Owner and information commands

| Command                                       | Behavior                                                                                                                      |
| --------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------- |
| `/admin check name:anilist` or `mal`          | Queue one coalesced cycle wake-up. It does not overlap a running cycle or execute a long poll inline.                         |
| `/admin cleanup confirm:true`                 | Remove up to 100 users with no server memberships, cascading their orphaned provider data. Repeat if more remain.             |
| `/admin forcetoleave guildid:ID confirm:true` | Remove that server's tracking configuration. The bot remains a Discord member.                                                |
| `/admin restart confirm:true`                 | Reply first, then exit gracefully. An external supervisor may restart the executable; no supervisor is installed by Kotantei. |
| `/about`                                      | Brief description, AGPLv3/repository link, build version, and a source-download hint when configured.                         |
| `/status`                                     | Current provider-cycle aggregates and this server's delivery state.                                                           |

Server administrators are not automatically bot owners. Destructive commands require explicit `confirm:true`; command replies and `/say` disable all mentions. Provider notification payloads may allowlist only their captured tracked Discord user; roles/everyone and reply pings remain disabled.

## Health, delivery, and shutdown

Runtime logs are structured JSON on stderr and use safe failure codes, never raw upstream bodies or interaction tokens. `/status` shows current cycle counts/warnings and safe provider errors. Credential-free `kotantei health` reads a coherent storage snapshot, configured enablement/delays, saved checkpoints, and outbox failures. It labels runtime liveness `unobserved`; a saved checkpoint is not proof the process or gateway is alive. Use the running bot's same configuration/environment for that CLI report.

Provider cycles page actively subscribed account IDs against a captured high-water fence, with small bounded pages. New higher account IDs wait for the next cycle. AniList and MAL loops are independent and wait their configured delays after a cycle; linking/search share the corresponding provider's rate budget. There is no hard real-time update guarantee.

Discord transport errors classify missing permissions, unknown channels, invalid messages, and transient/rate failures. `/sm resume` or CLI `resume-channel` is explicit recovery after repair. See [durable delivery and its ambiguous-send window](tracking.md): delivery is at least once, not exactly once.

SIGTERM/interrupt or a successful owner shutdown cancels work and closes command admission. The runtime joins gateway, provider/reconciliation/delivery work, interaction replies, and logging before releasing clients/storage. Known Discord receipts still get a short cancellation-independent SQLite acknowledgment deadline; interrupted sends release their leases. An already in-flight send cannot be retracted by removing a subscription.

## Default runtime bounds

| Bound                                         |                                           Default |
| --------------------------------------------- | ------------------------------------------------: |
| Gateway READY wait                            |                                        60 seconds |
| Websocket dial / graceful close               |                            10 seconds / 2 seconds |
| Uncompressed gateway JSON packet              |                                             4 MiB |
| Discord HTTP deadline / decoded body          |                                15 seconds / 1 MiB |
| Discord REST route-rate cache                 |             256 entries; 15-minute inactivity TTL |
| Shared REST admission pace                    |          40 requests/second, plus upstream limits |
| Active commands / overload responders         |                                             4 / 2 |
| Interaction dedupe cache                      | 256 entries / 15 minutes; active work is retained |
| Deferred acknowledgment / final edit          |                             2 seconds / 5 seconds |
| Command operation budget after successful ACK |                                         6 minutes |
| Provider/membership page                      |                               64 accounts/members |
| Membership check / spacing / repeat interval  |                    10 seconds / 1 second / 1 hour |
| Delivery workers / lease / send / cleanup     |           1 / 30 seconds / 15 seconds / 5 seconds |

Acknowledgment, command work, and final replies have separate deadlines. The command budget begins after a successful deferred acknowledgment; all three still respect runtime shutdown cancellation.

Route-rate entries with active requests or future cooldowns are not evicted. If the cache cannot safely admit another route, the request fails/retries rather than growing memory or forgetting an upstream limit. Fractional retry-after and body-reported global Discord limits are honored conservatively. SDK retries are disabled; the durable outbox owns send retries. Websocket compression and raw gateway logging are not enabled.

Release builds compile their version tag and matching-source URL. `project.source_url` or `KOTANTEI_SOURCE_URL` lets operators offer the source of their own modified running version through `/about`; the full source-download URL is retained as the embed's title link, not clipped into a field. Credential-free `kotantei version` reports compiled identity without reading configuration. See [licensing/source obligations](licensing.md).

Most bounds are Go API options. See [configuration](configuration.md) for runtime settings and [deployment](deployment.md) for container operation.
