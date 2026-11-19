# Maintenance commands

Use the running bot's configuration and database. Global flags precede the command. Inspection commands return JSON and can inspect an existing running database.

| Command                                        | Purpose                                                                   |
| ---------------------------------------------- | ------------------------------------------------------------------------- |
| `kotantei init`                                | Initialize storage and import a conventional legacy input if present      |
| `kotantei import --source PATH`                | Import a legacy SQLite database into an empty destination                 |
| `kotantei doctor`                              | Inspect schema and storage                                                |
| `kotantei outbox`                              | Inspect pending/failed deliveries and blocked channels                    |
| `kotantei health`                              | Inspect saved provider checkpoints, configuration, and storage aggregates |
| `kotantei version`                             | Show compiled version, revision, source offer, and license                |
| `kotantei resume-channel --channel CHANNEL_ID` | Requeue a repaired channel's failed deliveries                            |

For a native configuration:

```sh
kotantei --config config.jsonc doctor
kotantei --config config.jsonc outbox
kotantei --config config.jsonc health
```

For Compose:

```sh
docker compose run --rm --no-deps kotantei --config /run/secrets/kotantei_config.jsonc doctor
docker compose run --rm --no-deps kotantei --config /run/secrets/kotantei_config.jsonc outbox
docker compose run --rm --no-deps kotantei --config /run/secrets/kotantei_config.jsonc health
```

`doctor`, `outbox`, and `health` are read-only: they do not create or migrate storage. Their output omits account names, message payloads, lease tokens, and credentials. `health` describes saved state, not current process liveness; use Discord `/status` and runtime logs for current cycle outcomes.

`run` and `init` upgrade older native schemas. Stop the bot and retain a consistent backup before importing, restoring, or upgrading data. Imports preserve supported accounts, settings, memberships, favorites, and checkpoints; a populated destination is not overwritten.

After repairing Discord permissions or channel availability, `/sm resume` revalidates the channel before requeuing failed deliveries. `resume-channel` provides CLI recovery and requires an existing database. Neither command requeues acknowledged posts. See [tracking and delivery](tracking.md) for retry semantics.

See [deployment](deployment.md) for backup, migration, and shutdown procedures.
