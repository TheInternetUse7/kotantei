# Configuration

Copy [config.example.jsonc](../config.example.jsonc) to a private `config.jsonc`. The template includes every application setting, its default, and accepted values.

Compose mounts `config.jsonc` read-only; application settings are not duplicated in environment variables. Docker-specific settings are kept in [compose.yaml](../compose.yaml). Apply the [file permissions](deployment.md#compose-on-a-linux-vps), then recreate the service after changing configuration:

```sh
docker compose up -d --force-recreate
```

For a native run:

```sh
go run ./cmd/kotantei --config ./config.jsonc run
```

Configuration is loaded at startup. Settings are applied in this order: built-in defaults, optional JSONC/JSON file, then any explicitly supplied environment overrides. Compose uses only the file.

## Settings and accepted values

| Field                         | Default            | Accepted values                                                          |
| ----------------------------- | ------------------ | ------------------------------------------------------------------------ |
| `discord.token`               | Empty              | Discord bot token; required for `run`                                    |
| `anilist.enabled`             | `true`             | Boolean: `true` or `false`                                               |
| `anilist.cycle_delay`         | `90s`              | Positive Go duration, such as `90s`, `2m`, or `1h`                       |
| `anilist.requests_per_minute` | `29`               | Integer from `1` through `29`                                            |
| `mal.enabled`                 | `true`             | Boolean: `true` or `false`                                               |
| `mal.client_id`               | Empty              | MAL application client ID; required when MAL is enabled                  |
| `mal.tenrai_enabled`          | `false`            | Boolean: `true` or `false`; optional catalogue enrichment                |
| `mal.cycle_delay`             | `50s`              | Positive Go duration, such as `50s` or `2m`                              |
| `mal.request_interval`        | `3s`               | Positive Go duration, such as `3s` or `5s`                               |
| `storage.database`            | `data/kotantei.db` | Nonempty SQLite destination path                                         |
| `storage.tantei_database`     | `data/data.db`     | Legacy SQLite input path, or an empty string to disable automatic import |
| `project.source_url`          | Empty              | Public HTTPS download of the running build's matching source             |

Relative paths use the process working directory. The supplied container starts in `/`, so the default `data/` paths resolve inside its persistent `/data` volume. Native runs resolve them from the directory where you start the bot. Source and destination paths must differ; imports read the source without changing it.

At least one provider must be enabled for `run`. To use only AniList, set `mal.enabled` to `false`; its client ID can remain empty. Unknown configuration fields, null values, invalid durations, and files larger than 1 MiB are rejected.

## Optional environment overrides

These remain available for native runs and custom deployments; the supplied Compose file does not require them.

| Environment variable                   | Field                         |
| -------------------------------------- | ----------------------------- |
| `KOTANTEI_DISCORD_TOKEN`               | `discord.token`               |
| `KOTANTEI_ANILIST_ENABLED`             | `anilist.enabled`             |
| `KOTANTEI_ANILIST_CYCLE_DELAY`         | `anilist.cycle_delay`         |
| `KOTANTEI_ANILIST_REQUESTS_PER_MINUTE` | `anilist.requests_per_minute` |
| `KOTANTEI_MAL_ENABLED`                 | `mal.enabled`                 |
| `KOTANTEI_MAL_CLIENT_ID`               | `mal.client_id`               |
| `KOTANTEI_MAL_TENRAI_ENABLED`          | `mal.tenrai_enabled`          |
| `KOTANTEI_MAL_CYCLE_DELAY`             | `mal.cycle_delay`             |
| `KOTANTEI_MAL_REQUEST_INTERVAL`        | `mal.request_interval`        |
| `KOTANTEI_DATABASE`                    | `storage.database`            |
| `KOTANTEI_LEGACY_DATABASE`             | `storage.tantei_database`     |
| `KOTANTEI_SOURCE_URL`                  | `project.source_url`          |

Boolean environment values accept case-insensitive `true` or `false`; surrounding whitespace is trimmed. An explicitly empty boolean, duration, or request limit is invalid. `KOTANTEI_TANTEI_DATABASE` remains a compatibility alias; `KOTANTEI_LEGACY_DATABASE` takes precedence, including an explicitly empty value.

## Credentials and source links

Official images embed their matching source download. Modified deployments can set their own matching source download; see [licensing](licensing.md).

See [deployment](deployment.md) for Docker settings and data ownership, and the [AniList](anilist.md) and [MAL](mal.md) guides for provider behavior.
