# Artist Release Tracker

A self-hosted household dashboard that watches MusicBrainz for new albums and
EPs and sends announcement and release-day notifications through
[Shoutrrr](https://containrrr.dev/shoutrrr/).

## Quick start

1. Copy `.env.example` to `.env` and set `SETUP_TOKEN`,
   `APP_ENCRYPTION_KEY`, and `SESSION_SECRET`. Each secret should be a random
   value of at least 32 characters.
2. Set `MUSICBRAINZ_CONTACT` to a real email address or project URL.
3. Start the application:

   ```console
   docker compose up --build -d
   ```

4. Open `http://localhost:8080/setup`, enter `SETUP_TOKEN`, and create the
   first administrator.

Application data is stored in the `artist-tracker-data` Docker volume. The app
supports a single running replica.

Release-group artwork is loaded from the Cover Art Archive and cached alongside
the database in the persistent data volume.

## Configuration

| Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `PUBLIC_URL` | yes | `http://localhost:8080` | External base URL; use HTTPS behind a reverse proxy. |
| `SETUP_TOKEN` | first run | — | Protects initial administrator creation. |
| `APP_ENCRYPTION_KEY` | yes | — | Encrypts notification credentials at rest. |
| `SESSION_SECRET` | yes | — | Adds server-side protection to session cookies. |
| `MUSICBRAINZ_CONTACT` | yes | — | Contact included in the required MusicBrainz User-Agent. |
| `POLL_INTERVAL` | no | `6h` | Catalog polling interval; values below one hour are rejected. |
| `SPOTIFY_CLIENT_ID` | no | — | Enables Spotify-first artist discovery. |
| `SPOTIFY_CLIENT_SECRET` | no | — | Spotify application secret. |
| `DATABASE_PATH` | no | `/data/artist-tracker.db` | SQLite database location. |
| `LISTEN_ADDR` | no | `:8080` | HTTP listen address. |

Every secret also supports Docker's `*_FILE` convention, for example
`APP_ENCRYPTION_KEY_FILE=/run/secrets/encryption_key`.

## Spotify discovery

Create an application in the [Spotify developer dashboard](https://developer.spotify.com/dashboard),
then set `SPOTIFY_CLIENT_ID` and `SPOTIFY_CLIENT_SECRET`. When configured,
Spotify supplies the primary artist search, images, and links. MusicBrainz still
provides the canonical artist identity and all release tracking. Selections that
cannot be identified while MusicBrainz is unavailable remain pending and retry
automatically.

Spotify Development Mode currently requires the application owner to have an
active Premium subscription and limits new applications to five authorized
users. No Spotify user login is required for this application's client-credentials
search flow.

## Notification destinations

Users can add guided Email, Discord, Telegram, ntfy, Gotify, and generic
webhook destinations. Any service supported by Shoutrrr can be added with its
raw service URL. Credentials are encrypted in SQLite and redacted from the UI
and logs. Use the **Send test** action after adding a destination.

## Reverse proxy and backups

Terminate TLS at Caddy, Traefik, nginx, or another reverse proxy and set
`PUBLIC_URL` to its HTTPS address. The app honors `X-Forwarded-For` only when
`TRUST_PROXY=true`.

For a consistent backup, stop the container and archive the Docker volume:

```console
docker compose stop app
docker run --rm -v artist-tracker-data:/data -v "$PWD":/backup alpine \
  tar czf /backup/artist-tracker-backup.tgz -C /data .
docker compose start app
```

Restore into an empty volume while the app is stopped. Embedded migrations run
automatically during upgrades.

## Development

The test suite runs in the pinned Go toolchain from the build image:

```console
docker build --target test .
```

No Node.js toolchain or external asset CDN is required.
