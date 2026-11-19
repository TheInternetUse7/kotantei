# Kotantei

A Go bot that posts public AniList and MyAnimeList updates in Discord, designed for a low resource hosting server.

## Quick start

Container deployment requires Docker Engine and the Docker Compose plugin.

1. Create a Discord bot and install it in your server with the `bot` and `applications.commands` scopes. Give it **View Channel**, **Send Messages**, and **Embed Links** in the update channel.
2. Copy [compose.yaml](compose.yaml) and [config.example.jsonc](config.example.jsonc) into a private deployment directory. Copy the template to `config.jsonc`, fill in the Discord token and MAL client ID, and apply the [configuration-file permissions](docs/deployment.md#compose-on-a-linux-vps).
3. Start the bot:

   ```sh
   docker compose up -d
   docker compose logs -f kotantei
   ```

4. Choose the channel with `/sm set`. Users link their accounts with `/anilist user add` or `/mal user add`.

See [deployment and backups](docs/deployment.md) and the [Discord command guide](docs/discord.md).

## Configuration

Application settings, defaults, and accepted values are documented in [config.example.jsonc](config.example.jsonc). Compose mounts your private `config.jsonc` read-only; Docker settings stay in [compose.yaml](compose.yaml).

Native runs use the same configuration:

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
