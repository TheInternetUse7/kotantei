# Deployment and migration

Run one Kotantei instance with its own SQLite database. The default image is `ghcr.io/theinternetuse7/kotantei:latest`; version tags and immutable manifest digests can also be used. Images are published by the [release workflow](releases.md).

## Credentials and configuration

- Create a Discord bot application and install it with the `bot` and `applications.commands` scopes. Grant **View Channel**, **Send Messages**, and **Embed Links** in the posting channel. No privileged intents are needed.
- Copy [config.example.jsonc](../config.example.jsonc) to private `config.jsonc`. Set `discord.token` and, when MAL is enabled, `mal.client_id`.
- Both providers default to enabled. Set `mal.enabled` to `false` for AniList-only operation. Optional Tenrai enrichment defaults to false.
- Release builds embed a matching-source download for `/about`. If you run modified code, publish its matching source and set `project.source_url`; see [licensing](licensing.md).

Keep `config.jsonc`, databases, and backups private. Compose mounts the deployment directory read-only, without duplicating credentials in environment variables. Use a dedicated directory containing only this bot's deployment files, not a source checkout or a shared secrets folder. Restrict host and Docker administrative access; never include credentials in build arguments or source releases. See [configuration](configuration.md) for all settings.

For a modified build, set `project.source_url` to its matching public source download. Leave it empty for an official image's embedded offer.

## Compose on a Linux VPS

Requires Docker Engine and the Docker Compose plugin. Copy `compose.yaml` and `config.example.jsonc` into a dedicated deployment directory, then create your private configuration:

```sh
cp config.example.jsonc config.jsonc
```

Edit `config.jsonc` with the credentials and provider settings. The image manages configuration and storage locations internally; do not add storage paths to the JSONC file or edit Compose for the default setup.

The expected layout is:

```text
kotantei/
├── compose.yaml
├── config.jsonc
└── data.db          # optional consistent legacy database backup
```

### Fresh installation

Leave out `data.db`. The missing optional input is skipped, and Kotantei initializes its own database in the persistent Docker volume. No Compose edits are needed.

### Migration before first startup

Stop Tantei and copy a consistent database backup into this directory as `data.db`. Keep the original for rollback. Prefer a standalone SQLite backup; if copying from a stopped WAL database, retain any `data.db-wal` and `data.db-shm` companions too. Do not copy only the main file while the old bot is running.

The input filename is fixed. If the original database has another name or lives elsewhere, place a consistent backup here named `data.db`. Kotantei reads that copy without changing it and writes its own database to the persistent Docker volume. Both fresh and migrating installations use the same Compose file and startup commands below.

### File access and startup

The process runs as UID/GID **65532**. For ordinary rootful Docker on Linux, allow that identity to traverse the deployment directory and read only the required private files:

```sh
chmod 0755 .
sudo chown 65532:65532 config.jsonc
sudo chmod 0400 config.jsonc
for file in data.db data.db-wal data.db-shm; do
  if [ -f "$file" ]; then
    sudo chown 65532:65532 "$file"
    sudo chmod 0400 "$file"
  fi
done
```

Use a privileged editor for later changes to `config.jsonc`. Bind mounts preserve host file ownership. Rootless Docker, user namespaces, and SELinux may require corresponding mapped ownership or access rules. Existing data volumes must also be writable by the container identity.

```sh
docker compose pull
docker compose up -d
docker compose logs --tail=100 -f kotantei
```

For a migration, look for `legacy import completed` and the expected account counts. Import happens before Discord/provider startup. If a later network or permission check fails, keep the new database and retry after fixing that failure; do not start a second bot or reset the data volume.

There are no listening ports to expose. Outbound HTTPS/WSS and DNS access are required for Discord, AniList, MAL, and any enabled enrichment. The pinned Distroless static runtime supplies CA certificates and has no shell or package manager.

Compose provides a read-only root filesystem, writable named volume at `/data`, a temporary filesystem, dropped capabilities, no-new-privileges, bounded logs, and a graceful stop interval. A new volume inherits the image's owned data directory; existing volumes or substituted bind mounts must have suitable ownership.

Configuration is loaded at startup. After changing settings, recreate the service with `docker compose up -d --force-recreate`, retaining the same data volume. Optional Docker resource controls are configured directly in Compose.

Once ready, a manager chooses the channel with `/sm set`; users link accounts with `/anilist user add` and `/mal user add`. See [commands and permissions](discord.md).

## Maintenance and shutdown

The image entrypoint selects your configuration automatically, so maintenance commands need no path arguments:

```sh
docker compose run --rm --no-deps kotantei version
docker compose run --rm --no-deps kotantei doctor
docker compose run --rm --no-deps kotantei outbox
docker compose run --rm --no-deps kotantei health
```

These inspection commands can be used while the bot is running; the image does not need a shell.

`version` reports compiled identity without reading configuration or storage. `doctor`, `outbox`, and `health` inspect existing storage read-only; they do not migrate or create it. **`health` is not a liveness probe**, so no Docker healthcheck is configured. Use `/status` and current structured runtime logs for provider outcomes. Permission failures require repair and `/sm resume` or `resume-channel --channel ID`; restarting alone does not requeue blocked channels.

`docker compose stop` sends SIGTERM and waits for joined shutdown. `restart:unless-stopped` also restarts the process after a successful `/admin restart`; explicitly stopping the service prevents automatic restart. Do not run two `run` processes against the database. Maintenance inspection can read a running database, but stop the bot before imports, restore operations, or upgrades.

## Legacy database migration

Put a consistent backup named `data.db` beside `compose.yaml` and `config.jsonc`, then use the normal startup commands above. The Docker image automatically imports it into eligible empty storage before starting Discord/provider work. No import path, wrapper, or Compose override is needed.

Imports require an empty destination and leave the source unchanged. A complete standalone backup avoids the additional shared-memory requirements of a read-only WAL snapshot. Unsupported or inconsistent input fails atomically.

Once the import is recorded, later startups do not inspect or reimport the legacy file. You can retain the copy for rollback or store it elsewhere; the running bot uses its own persisted database.

The importer preserves supported accounts, shared memberships, channels, settings, favorites, and checkpoints. Install the new bot in each retained server and grant access to the imported channels.

When updating an existing deployment, keep its directory/project name and `data` volume, update the image and Compose file, and retain your private configuration. Older JSONC `storage` entries remain accepted but are overridden by the Docker image; you can remove them. Completed imports do not require changing a legacy path or importing again.

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
