# Configuration

Copy [config.example.jsonc](../config.example.jsonc) to a private `config.jsonc`. The template documents credentials, provider settings, and accepted values. Docker storage paths are not user configuration.

Keep `compose.yaml`, `config.jsonc`, and optional legacy `data.db` in one dedicated deployment directory. The image automatically selects the configuration and database locations. No path settings, generated deployment files, or manual Compose edits are required. Apply the [file permissions](deployment.md#file-access-and-startup), then recreate the service after changing configuration:

```sh
docker compose up -d --force-recreate
```

## Native runs

For a native run:

```sh
go run ./cmd/kotantei --config ./config.jsonc run
```

Native built-in storage defaults are `data/kotantei.db` and optional legacy `data/data.db`, relative to the working directory. Native runs can optionally override these local paths in a `storage` section, for example:

```jsonc
"storage": {
  "database": "data/kotantei.db",
  "tantei_database": "data.db"
}
```

Set the native legacy path to `""` if you do not want automatic import. These storage fields are for native runs; the Docker image overrides them with its fixed internal locations, including when reading older configurations.

Configuration is loaded at startup. Settings are applied in this order: built-in defaults, optional JSONC/JSON file, then environment overrides. Docker supplies only its internal storage locations; credentials and provider settings come from your file.

## Settings and accepted values

| Field                         | Template value     | Accepted values                                                          |
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
| `project.source_url`          | Empty              | Public HTTPS download of the running build's matching source             |

Docker imports a consistent `data.db` backup beside Compose before first startup when the destination is eligible and empty. Missing input is skipped. Once an import is recorded, later startups do not inspect the legacy input again. The source stays read-only and the native database stays in the persistent volume.

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
| `KOTANTEI_SOURCE_URL`                  | `project.source_url`          |

Boolean environment values accept case-insensitive `true` or `false`; surrounding whitespace is trimmed. An explicitly empty boolean, duration, or request limit is invalid.

For native storage overrides, `KOTANTEI_DATABASE` selects the destination and `KOTANTEI_LEGACY_DATABASE` selects the optional input. `KOTANTEI_TANTEI_DATABASE` remains a compatibility alias; `KOTANTEI_LEGACY_DATABASE` takes precedence, including an explicitly empty value. Normal Docker deployment does not require these settings.

## Credentials and source links

Official images embed their matching source download. Modified deployments can set their own matching source download; see [licensing](licensing.md).

See [deployment](deployment.md) for Docker settings and data ownership, and the [AniList](anilist.md) and [MAL](mal.md) guides for provider behavior.
