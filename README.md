# Kotantei

A Go bot that posts public AniList and MyAnimeList updates in Discord, designed for a low resource hosting server.

## Quick start

Container deployment requires Docker Engine and the Docker Compose plugin.

1. Create a Discord bot and install it in your server with the `bot` and `applications.commands` scopes. Give it **View Channel**, **Send Messages**, and **Embed Links** in the update channel.
2. Copy [compose.yaml](compose.yaml) and [config.example.jsonc](config.example.jsonc) into a dedicated deployment directory. Copy the template to `config.jsonc` and fill in `discord.token` and, when MAL is enabled, `mal.client_id`.
3. **Migrating from Tantei?** Stop it first and put a consistent backup beside `compose.yaml`, named `data.db`. For a fresh installation, leave that file out. No storage paths or Compose edits are required.
4. From that directory, set file access for normal rootful Docker on Linux:

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

5. Start the bot:

   ```sh
   docker compose pull
   docker compose up -d
   docker compose logs --tail=100 -f kotantei
   ```

6. For a migration, check for `legacy import completed` and its account counts in the logs. Imported users keep their links. For a fresh installation, choose the channel with `/sm set`; users link accounts with `/anilist user add` or `/mal user add`.

See [deployment and backups](docs/deployment.md) and the [Discord command guide](docs/discord.md).

## Configuration

Application settings and accepted values are documented in [config.example.jsonc](config.example.jsonc). The Docker image automatically reads `config.jsonc` and optional `data.db` from the deployment directory. Storage locations are managed internally; the new database stays in the persistent Docker volume.

For native runs, see [configuration](docs/configuration.md#native-runs):

```sh
go run ./cmd/kotantei --config ./config.jsonc run
```

See [configuration](docs/configuration.md) for details. Plain JSON configurations remain supported.

## Development

Requires **Go 1.26 or newer**.

```sh
go test ./...
go vet ./...
go build -trimpath -o kotantei ./cmd/kotantei
```

## Documentation

- [Deployment, migration, and backups](docs/deployment.md)
- [Configuration](docs/configuration.md)
- [Discord commands and permissions](docs/discord.md)
- [AniList](docs/anilist.md) and [MyAnimeList](docs/mal.md)
- [Maintenance commands](docs/maintenance.md)
- [Tracking and delivery](docs/tracking.md)
- [Building and publishing releases](docs/releases.md)
- [Licensing and corresponding source](docs/licensing.md)

## License

**AGPL-3.0-only**. See [LICENSE](LICENSE) and [NOTICE](NOTICE).

## Acknowledgment

Thanks to [Tantei](https://github.com/TanteiBot/Tantei) for inspiring this project.
