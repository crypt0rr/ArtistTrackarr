# Artist Trackarr

A self-hosted household dashboard that watches MusicBrainz and, when configured,
Spotify for new albums and EPs and sends announcement and release-day notifications through
[Shoutrrr](https://containrrr.dev/shoutrrr/).

## Quick start

1. Copy `.env.example` to `.env` and set `SETUP_TOKEN`,
   `APP_ENCRYPTION_KEY`, and `SESSION_SECRET`. Each secret should be a random
   value of at least 32 characters.
2. Set `MUSICBRAINZ_CONTACT` to a real email address or project URL.
3. Start the application:

   ```console
   docker compose pull
   docker compose up -d
   ```

4. Open `http://localhost:8080/setup`, enter `SETUP_TOKEN`, and create the
   first administrator.

Application data is stored in the legacy-named `artist-tracker-data` Docker
volume so existing installations can upgrade without moving their data. The
app supports a single running replica. Docker Compose names that container
`artist-trackarr` for predictable logs and administration commands.

Release-group artwork is loaded from the Cover Art Archive and cached alongside
the database in the persistent data volume.

The interface follows the operating-system color theme by default. Use the
header theme control to select and remember an explicit light or dark mode.
The running application version and project repository are available in the
footer.

## Container images

GitHub Actions builds and publishes the Docker image to
`ghcr.io/crypt0rr/artist-trackarr` for `linux/amd64` and `linux/arm64`.

- `latest` and `main` follow the current `main` branch.
- `sha-<commit>` identifies an exact source revision.
- Pushing a tag such as `v0.1.7` publishes `0.1.7`, `0.1`, and `latest`.

Pin a deployment to a release by setting the Compose image before starting:

```console
ARTIST_TRACKARR_IMAGE=ghcr.io/crypt0rr/artist-trackarr:0.1.7 docker compose up -d
```

The first workflow run creates the package. If anonymous Docker pulls are
required, set the package visibility to **Public** once in the package settings
on GitHub. The workflow uses the repository-scoped `GITHUB_TOKEN`; no registry
password or personal access token is required.

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
| `SPOTIFY_MARKET` | no | `US` | Two-letter market used when retrieving Spotify releases. |
| `DATABASE_PATH` | no | `/data/artist-tracker.db` | SQLite database location. |
| `LISTEN_ADDR` | no | `:8080` | HTTP listen address. |

Every secret also supports Docker's `*_FILE` convention, for example
`APP_ENCRYPTION_KEY_FILE=/run/secrets/encryption_key`.

## Spotify discovery and release observation

Create an application in the [Spotify developer dashboard](https://developer.spotify.com/dashboard),
then set `SPOTIFY_CLIENT_ID` and `SPOTIFY_CLIENT_SECRET`. When configured,
Spotify supplies the primary artist search, images, links, and an independent
album/EP observation feed. MusicBrainz remains the canonical artist and release
identity whenever a release-group match is available. Spotify-only releases are
stored under a stable provider identity and may generate notifications; they are
promoted to the MusicBrainz release group later when a conservative title, type,
and date match is found.

Set `SPOTIFY_MARKET` to the country whose catalogue should be checked, for
example `NL`. Existing followed artists are silently baselined the first time
Spotify release polling runs after an upgrade, preventing back-catalogue
notification floods. New releases observed after that baseline can notify
independently of MusicBrainz. Spotify entries classified as albums are tracked;
items classified as singles are excluded, while multi-track releases with at
least four tracks are treated as EPs.

Selections that cannot be identified while MusicBrainz is unavailable remain
pending and retry automatically.

Spotify Development Mode currently requires the application owner to have an
active Premium subscription and limits new applications to five authorized
users. No Spotify user login is required for this application's
client-credentials search and release-observation flow.

## Artist management

The **Add artists** page combines individual search, multi-select following,
and watchlist export. Exports contain canonical MusicBrainz URLs and IDs plus
optional Spotify identifiers for backups and external processing. Bulk import
is not currently available.

## Notification destinations

Users can add guided Email, Discord, Telegram, ntfy, Gotify, and generic
webhook destinations. Any service supported by Shoutrrr can be added with its
raw service URL. Credentials are encrypted in SQLite and redacted from the UI
and logs. Use the **Send test** action after adding a destination.

## Household administration

Administrators can review every household account, its role, reminder settings,
follow count, and notification-destination count. They can permanently delete
another user and all of that user's private data. Administrators cannot delete
their own account or leave the household without an administrator.

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

To build and run the current checkout instead of the published image:

```console
docker build -t artist-trackarr:local .
ARTIST_TRACKARR_IMAGE=artist-trackarr:local docker compose up -d
```

No Node.js toolchain or external asset CDN is required.
