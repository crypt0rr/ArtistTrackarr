# ArtistTrackarr

A self-hosted household dashboard that watches Spotify for new albums and EPs,
using MusicBrainz for stable artist identity and optional enrichment, and sends announcement and release-day notifications through
[Shoutrrr](https://containrrr.dev/shoutrrr/).

## Example dashboard

The following screenshot shows how ArtistTrackarr can look in a production
deployment:

![ArtistTrackarr production dashboard](docs/images/artisttrackarr-production.png)

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

Use the moon/sun button in the header to switch between light and dark mode;
your choice is remembered in the browser.
The running application version and project repository are available in the
footer.

## Container images

GitHub Actions builds and publishes the Docker image to
`ghcr.io/crypt0rr/artist-trackarr` for `linux/amd64` and `linux/arm64`.

- `latest` and `main` follow the current `main` branch.
- `sha-<commit>` identifies an exact source revision.
- Pushing a tag such as `v0.9.0` publishes `0.9.0`, `0.9`, and `latest`.

Pin a deployment to a release by setting the Compose image before starting:

```console
ARTIST_TRACKARR_IMAGE=ghcr.io/crypt0rr/artist-trackarr:0.9.0 docker compose up -d
```

## Configuration

| Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `PUBLIC_URL` | yes | `http://localhost:8080` | External base URL; use HTTPS behind a reverse proxy. |
| `SETUP_TOKEN` | first run | — | Protects initial administrator creation. |
| `APP_ENCRYPTION_KEY` | yes | — | Encrypts notification credentials at rest. |
| `SESSION_SECRET` | yes | — | Adds server-side protection to session cookies. |
| `MUSICBRAINZ_CONTACT` | yes | — | Contact included in the required MusicBrainz User-Agent. |
| `POLL_INTERVAL` | no | `6h` | Catalog polling interval; values below one hour are rejected. |
| `SPOTIFY_POLL_INTERVAL` | no | `24h` | Independent Spotify observation interval; values below one hour are rejected. |
| `SPOTIFY_CLIENT_ID` | no | — | Enables Spotify-first artist discovery. |
| `SPOTIFY_CLIENT_SECRET` | no | — | Spotify application secret. |
| `SPOTIFY_MARKET` | no | `US` | Two-letter market used when retrieving Spotify releases. |
| `DATABASE_PATH` | no | `/data/artist-tracker.db` | SQLite database location. |
| `LISTEN_ADDR` | no | `:8080` | HTTP listen address. |
| `LOG_LEVEL` | no | `info` | JSON log threshold: `debug`, `info`, `warn`, or `error`. |
| `TZ` | no | `UTC` | Container/system timezone for runtime logs and local process time, e.g. `Europe/Amsterdam`. |

Every secret also supports Docker's `*_FILE` convention, for example
`APP_ENCRYPTION_KEY_FILE=/run/secrets/encryption_key`.

## Spotify discovery and release observation

Create an application in the [Spotify developer dashboard](https://developer.spotify.com/dashboard),
then set `SPOTIFY_CLIENT_ID` and `SPOTIFY_CLIENT_SECRET`. When configured,
Spotify supplies the primary artist search, images, links, and the authoritative
album/EP observation feed. MusicBrainz remains the stable artist identity and
optional enrichment source. Spotify-only releases are stored under their stable
Spotify identity and can generate notifications immediately; they are promoted
to a MusicBrainz release group later when a conservative title, type, and date
match is found.

Set `SPOTIFY_MARKET` to the country whose catalogue should be checked, for
example `NL`. Existing followed artists are silently baselined the first time
Spotify release polling runs after an upgrade, preventing back-catalogue
notification floods. New releases observed after that baseline can notify
independently of MusicBrainz. Albums, EPs, and singles are all eligible release
types; multi-track Spotify releases with at least four tracks are treated as
EPs.

To keep Spotify Development Mode usage low, release observation normally reads
the newest Spotify artist-albums page and only walks older pages when the stored
Spotify release history has not yet been reached. Known release IDs, dates, and
provider observations are retained locally, and successful responses are cached
for 24 hours. Artists are assigned stable polling offsets so a large watch list
is spread across the day instead of queried in one burst. MusicBrainz release
polling is used as a fallback when Spotify is unavailable or not mapped; it does
not override successful Spotify data.

Successful Spotify checks also adapt per artist. A catalog change returns the
artist to the configured `SPOTIFY_POLL_INTERVAL`; unchanged artists back off
progressively up to seven days, while artists with upcoming releases stay on the
baseline interval. The backoff state is stored in SQLite and survives restarts.

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

Users can choose whether albums, EPs, singles, announcements, and release-day
reminders should be delivered. Followed artists show their last and next
synchronization times, and **Sync now** queues a rate-limited refresh. Release
details expose the stored provider observations and source history.

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

## License

- Licensed under the [MIT License](LICENSE).
