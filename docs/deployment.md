# Deployment and migration

Run one Kotantei instance with its own SQLite database. The default image is `ghcr.io/theinternetuse7/kotantei:latest`; version tags and immutable manifest digests can also be used. Images are published by the [release workflow](releases.md).

## Credentials and configuration

- Create a Discord bot application and install it with the `bot` and `applications.commands` scopes. Grant **View Channel**, **Send Messages**, and **Embed Links** in the posting channel. No privileged intents are needed.
- Copy [config.example.jsonc](../config.example.jsonc) to private `config.jsonc`. Set `discord.token` and, when MAL is enabled, `mal.client_id`.
- Both providers default to enabled. Set `mal.enabled` to `false` for AniList-only operation. Optional Tenrai enrichment defaults to false.
- Release builds embed a matching-source download for `/about`. If you run modified code, publish its matching source and set `project.source_url`; see [licensing](licensing.md).

Keep `config.jsonc`, databases, and backups private. Compose reads the configuration as a read-only file secret, without duplicating credentials in environment variables. Local file secrets are bind mounts, not an encrypted secret store. Restrict host and Docker administrative access; never include credentials in build arguments or source releases. See [configuration](configuration.md) for all settings.

For a modified build, set `project.source_url` to its matching public source download. Leave it empty for an official image's embedded offer.

## Compose on a Linux VPS

Requires Docker Engine and the Docker Compose plugin. Copy `compose.yaml` and `config.example.jsonc` into a private deployment directory:

```sh
cp config.example.jsonc config.jsonc
```

Edit `config.jsonc` with the credentials and provider settings. Docker settings are documented directly in Compose; application settings are defined only in the JSONC file.

The process runs as UID/GID **65532**. For ordinary rootful Docker on Linux, make the configuration readable only by that identity:

```sh
sudo chown 65532:65532 config.jsonc
sudo chmod 0400 config.jsonc
```

Use a privileged editor for later changes. Local Compose secret `uid`, `gid`, and `mode` options do not remap file ownership. Rootless Docker, user namespaces, SELinux, and bind mounts may require corresponding access rules. Existing data volumes must also be writable by the container identity.

```sh
docker compose pull
docker compose up -d
docker compose logs --tail=100 -f kotantei
```

There are no listening ports to expose. Outbound HTTPS/WSS and DNS access are required for Discord, AniList, MAL, and any enabled enrichment. The pinned Distroless static runtime supplies CA certificates and has no shell or package manager.

Compose provides a read-only root filesystem, writable named volume at `/data`, a temporary filesystem, dropped capabilities, no-new-privileges, bounded logs, and a graceful stop interval. A new volume inherits the image's owned data directory; existing volumes or substituted bind mounts must have suitable ownership.

Configuration is loaded at startup. After changing settings, recreate the service with `docker compose up -d --force-recreate`, retaining the same data volume. Optional Docker resource controls are configured directly in Compose.

Once ready, a manager chooses the channel with `/sm set`; users link accounts with `/anilist user add` and `/mal user add`. See [commands and permissions](discord.md).

## Maintenance and shutdown

The image entrypoint is the executable. `compose run` replaces its default command, so include the mounted configuration path before maintenance commands:

```sh
docker compose run --rm --no-deps kotantei version
docker compose run --rm --no-deps kotantei --config /run/secrets/kotantei_config.jsonc doctor
docker compose run --rm --no-deps kotantei --config /run/secrets/kotantei_config.jsonc outbox
docker compose run --rm --no-deps kotantei --config /run/secrets/kotantei_config.jsonc health
```

For an existing running container, `compose exec` needs an explicit executable path; there is no shell:

```sh
docker compose exec kotantei /usr/local/bin/kotantei --config /run/secrets/kotantei_config.jsonc doctor
```

`version` reports compiled identity without reading configuration or storage. `doctor`, `outbox`, and `health` inspect existing storage read-only; they do not migrate or create it. **`health` is not a liveness probe**, so no Docker healthcheck is configured. Use `/status` and current structured runtime logs for provider outcomes. Permission failures require repair and `/sm resume` or `resume-channel --channel ID`; restarting alone does not requeue blocked channels.

`docker compose stop` sends SIGTERM and waits for joined shutdown. `restart:unless-stopped` also restarts the process after a successful `/admin restart`; explicitly stopping the service prevents automatic restart. Do not run two `run` processes against the database. Maintenance inspection can read a running database, but stop the bot before imports, restore operations, or upgrades.

## Legacy database migration

1. Stop the old bot and retain a consistent original backup for rollback. Do not run both bots while migrating.
2. Prefer a standalone SQLite backup made through SQLite's backup API. Copying only the main file of a WAL database can lose committed changes. If copying files from a fully stopped instance instead, keep the database and any `-wal`/`-shm` companions together.
3. Stop Kotantei if already running. Keep any existing Kotantei database backed up; a populated destination cannot be overwritten by the importer.
4. Put the consistent legacy snapshot in a private host directory and mount that directory read-only for the explicit import:

```sh
docker compose stop kotantei
docker compose run --rm --no-deps \
  --volume "$PWD/legacy-backup:/legacy:ro" \
  kotantei --config /run/secrets/kotantei_config.jsonc import --source /legacy/data.db
docker compose run --rm --no-deps kotantei --config /run/secrets/kotantei_config.jsonc doctor
docker compose up -d
```

The legacy snapshot and companions must be readable by UID/GID 65532. A complete standalone backup avoids the additional shared-memory requirements of a read-only WAL snapshot. Kotantei reads the source without migrating it and writes only its own `/data/kotantei.db`.

Alternatively, a conventional `/data/data.db` can be imported into eligible empty storage by `init` or first `run`. Set `storage.tantei_database` to `""` in `config.jsonc` to disable this lookup.

The importer preserves supported accounts, shared memberships, channels, settings, favorites, and checkpoints. Unsupported or inconsistent schemas fail atomically. Install the new bot in each retained server and grant access to the imported channels.

## Backups, upgrades, and rollback

The named `data` volume survives `compose down` without `--volumes`. **Do not use `down --volumes` for an upgrade.** Changing the Compose project name/directory can select a different named volume; keep that identity stable or manage the volume explicitly.

For a simple coherent filesystem backup, stop the service, keep its container, and copy all of `/data` to a fresh protected backup directory. For example, after creating that directory:

```sh
docker compose stop kotantei
container="$(docker compose ps --all --quiet kotantei)"
docker cp "$container:/data/." ./backup/
docker compose up -d
```

Do not copy only `kotantei.db` from a running WAL database. Use SQLite's online backup API for uninterrupted backups instead, with a suitable external SQLite tool. Store backups separately from releases and restrict their permissions: linked identities, settings, and possibly a retained legacy database are private operational data.

Before changing the Compose `image`, take a consistent backup and retain the previous image tag/digest. Pull the new version and recreate the service. `run`/`init` upgrade native schemas automatically; older binaries reject newer schemas. Rollback requires the previous image and its **pre-upgrade database snapshot**, with restored volume ownership. Never restore over a running bot or delete unfamiliar files from a live volume.

To return to the old bot, stop Kotantei and use the saved original database. Changes made only in Kotantei are not transferred back automatically. Pending retries can duplicate posts in the [ambiguous-send window](tracking.md).
