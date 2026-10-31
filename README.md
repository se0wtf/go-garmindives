# go-garmindives

`go-garmindives` exports every Garmin Connect diving activity to YAML, SQLite,
or PostgreSQL. It supports Garmin's official Personal API through an API key,
and the legacy Connect/Dive APIs through an account login. Each dive preserves
the available JSON documents, original FIT files, and all decoded FIT messages,
including depth-over-time samples. The local API also manages photos and videos
attached to each activity, with originals on disk and media metadata in SQL.

The app is unofficial and is not affiliated with Garmin.

## Install

```sh
go install github.com/se0wtf/go-garmindives@latest
```

## Usage

Create a key in [Garmin Connect's Personal API settings](https://connect.garmin.com/app/settings/personalApiToken)
and supply it through the environment:

```sh
export GARMIN_PERSONAL_API_KEY='your-personal-api-key'
go-garmindives
```

The key selects the official Personal API and takes precedence over cached
legacy sessions and email/password credentials. The app exchanges it for a
bearer token each run; the client automatically renews expiring tokens and
retries a rejected token once. A rejected key returns an error. No email,
password, MFA, or browser session is needed. The key is never written to disk
or application logs.

Personal API bearer tokens are saved as `go-garmindives/personal-tokens.json`
under the operating system's user configuration directory, with owner-only
permissions. Legacy tokens use `go-garmindives/tokens.json`. `-token-file`
overrides either path. An unexpired Personal API cache can be resumed without
the key by specifying its path; renewal requires `GARMIN_PERSONAL_API_KEY`.

The Personal API returns the full activity document under `dive_details`,
including `diveInfo` (weight, suit, gases, equipment, and settings), `summaryDTO`
(depths in meters, duration and bottom time in seconds, temperatures in Celsius),
and split summaries when recorded. SQL and REST summary fields use these values.
Missing measurements remain absent, and recorded zeroes remain zero.

The app also downloads the original FIT archive for each dive through
Garmin's [Personal API](https://developer.garmin.com/gc-developer-program/personal/swagger-docs/).
Depth-over-time profiles require only the API key. FIT downloads also work with
legacy sessions. ZIP archives, multiple FIT files, and chained FIT sequences
are decoded with CRC validation.

The `fit` document in YAML and REST dive details contains every decoded message:
records, sessions, laps, dive summaries, gases, settings, tank updates, device
information, events, and any other messages present in the file. Fields retain
their numbers, types, units, raw values, and values after applying FIT scales and
offsets. Developer fields retain their developer index and available descriptions.
Unknown fields and messages are preserved by number. Each FIT file also includes
`original_base64`, which preserves its exact bytes for future decoding. Invalid
measurements become `null`; recorded zeroes stay zero.

SQL and the samples endpoint index timestamps, depth, temperature, heart rate,
decompression stops, NDL, CNS/N2 load, gas consumption, PO2, and ascent rate when
recorded. Dive alerts and gas/tank events are indexed as alerts. All other data,
including tank pressure updates, remains available in the full `fit` document.
FIT timestamps are converted to Unix milliseconds; depth is in meters, times
are in seconds, temperature is in Celsius, gas consumption is in bar/min or
L/min, and FIT ascent rate is in m/s. Field-level units are retained in `fit`.
The Personal API still has `activity_details: null` because its graph JSON
endpoint is unavailable; its profiles are decoded from FIT instead.

Without `GARMIN_PERSONAL_API_KEY`, the app reuses a cached legacy session or
prompts for Garmin credentials and MFA. Credentials can also be supplied for
non-interactive legacy use:

```sh
GARMIN_EMAIL='you@example.com' GARMIN_PASSWORD='...' go-garmindives
```

The password is never stored. Legacy API tokens and the authenticated Garmin
browser session needed by the Dive endpoint are cached under the operating
system's user configuration directory with owner-only permissions. Later runs
reuse the cached session and automatically refresh expiring API tokens. If Garmin expires
either part of the session, the app asks for credentials and MFA again.

SQLite is the default output. Running `go-garmindives` creates or updates
`dives.db` in the current directory, with owner-only permissions. Set another
database path with:

```sh
go-garmindives -output-dsn /path/to/dives.db
```

The database path can also be configured through the environment:

```sh
GARMIN_OUTPUT_DSN=/path/to/dives.db go-garmindives
```

Select YAML explicitly to create or update `dives.yaml`, or choose another
archive and token cache path:

```sh
go-garmindives -output-type yaml
go-garmindives -output-type yaml -output /path/to/dives.yaml -token-file /path/to/tokens.json
```

`GARMIN_OUTPUT_TYPE=yaml` also selects YAML. The `-output` flag applies only to
YAML; SQLite and PostgreSQL use `-output-dsn` or `GARMIN_OUTPUT_DSN`.

For PostgreSQL, provide a standard connection URL:

```sh
GARMIN_OUTPUT_TYPE=postgres \
GARMIN_OUTPUT_DSN='postgres://user:password@localhost:5432/garmindives?sslmode=require' \
go-garmindives
```

The DSN is never written to application logs. The same settings are available
as `-output-type` and `-output-dsn` flags; flags override environment values.

Dives without FIT data are automatically backfilled on the next sync, including
previously imported summaries and legacy dives. Backfilling keeps their original
JSON documents and fills missing SQL summary values from FIT. When FIT contains
samples, those become the indexed profile. Once FIT has been imported, the dive
is skipped on later runs, even if its file has no profile samples.

Downloads are sequential. YAML is replaced atomically only after every download
and decode succeeds; SQL batches, including FIT backfills, commit in one
transaction. Corrupt or unavailable original files fail the sync without saving
partial changes. YAML, SQLite, and the token file use mode `0600` where applicable.

## Garmin request pacing

Garmin requests are throttled by default: the app waits at least **2 seconds**
between HTTP requests, with no initial burst. One limiter covers authentication,
token renewal, every activity-list page, JSON details, FIT downloads, and HTTP
redirects. The first request can start immediately. This allows at most about
30 requests per minute per running sync; it is an app default, not a documented
Garmin quota.

For slower imports, choose a positive Go duration through the environment:

```sh
GARMIN_REQUEST_INTERVAL=5s go-garmindives
```

On HTTP 429, the app honors `Retry-After` in either seconds or HTTP-date format.
Without a usable header it backs off for 30, 60, then 120 seconds. Reads and the
empty-body Personal API token exchange receive at most three automatic retries;
login/MFA POSTs are not replayed. The cooldown is shared with subsequent
requests even when retries are exhausted. Other HTTP errors are returned without
automatic retries. Rate-limit messages log the cooldown, never credentials.

Ctrl+C cancels pacing and cooldown waits immediately. The 30-second network
timeout applies to each attempt after waiting, so a long cooldown does not
exhaust it. Multiple app processes have independent limiters. The local REST
server does not make Garmin requests and is unaffected by this setting.

## SQL schema and migrations

SQL writes are transactional. The schema is designed for a future frontend:

- `dives` stores queryable summary, location, profile, environment, and heart
  rate fields, plus the available source JSON documents so no returned Garmin
  data is lost. `fit_json` retains the complete decoded FIT document and original
  file bytes. Migration 3 adds this column to existing databases.
- `dive_fit_files` stores each original FIT file as a SQLite `BLOB` or PostgreSQL
  `BYTEA`, keyed by `(dive_id, file_index)`. Rows retain the filename, byte count,
  SHA-256 checksum, and storage timestamp. Deleting a dive cascades to its files.
  Migration 4 creates the table and copies original bytes from existing
  `fit_json` documents, entirely offline. New downloads and FIT backfills save
  these rows in the same transaction as the dive and samples; repeated saves
  keep the cached originals.
- `dive_samples` stores one row per timestamp with indexed `dive_id` and
  `timestamp_ms`, elapsed time, depth, temperature, heart rate, decompression,
  gas-consumption, PO2, and ascent-rate columns. It also retains the original
  sample JSON for forward compatibility.
- `dive_record_metadata` preserves Garmin's position-to-metric mapping for legacy
  records, or a sorted list of recorded metric keys for FIT profiles. FIT sample
  raw JSON retains the full message with field numbers and units.
- `dive_alerts` stores timestamped dive-computer alerts.

The app runs forward-only migrations automatically when it opens a SQL
database. Applied versions are recorded in `schema_migrations`, and each new
migration runs in a transaction. A database newer than the app is rejected
instead of being modified. Future schema changes are added as a new numbered
migration; existing migrations are never rewritten.

Future features can load exact original bytes through `sqlStore.FITFiles`, or
re-decode all cached files with `sqlStore.DecodeStoredFIT`, without a Garmin
session or a network request. Reads verify byte counts and checksums. The
existing JSON/REST and YAML representations retain `original_base64` for
compatibility. A stored binary file also marks its dive as cached for sync,
even if the decoded JSON is subsequently cleared for reprocessing.

## REST API

The API reads directly from SQLite or PostgreSQL and does not contact Garmin or
require Garmin credentials. Start it with the same SQL configuration used for
synchronization:

```sh
go-garmindives serve -output-type sqlite -output-dsn /path/to/dives.db
```

It listens on `127.0.0.1:8080` by default. Change this with `-listen` or
`GARMIN_API_ADDR`. There is intentionally no authentication yet, so only bind it
to a trusted network. Browser access from a separately hosted JavaScript
frontend is enabled through CORS.

Browse the interactive **Swagger UI** at
[`http://127.0.0.1:8080/swagger/`](http://127.0.0.1:8080/swagger/) (also available
through `/docs`). It covers every dive and media endpoint, including multipart
uploads, pagination, file ranges, error responses, and 360-degree projection metadata.
Use **Try it out** to call the running local API.

The OpenAPI 3.0.3 specification is available at `/api/v1/openapi.json` and
`/api/v1/openapi.yaml`. Use either URL with a frontend client/type generator.
[`docs/openapi.yaml`](docs/openapi.yaml) is the editable source; the JSON is derived
from it automatically. The specification and Swagger UI assets are embedded in
the Go binary and work offline, without a CDN or external validation service.

The versioned endpoints are:

- `GET /api/v1/health`
- `GET /api/v1/dives`
- `GET /api/v1/dives/{id}`
- `PATCH /api/v1/dives/{id}`
- `GET /api/v1/dives/{id}/samples`

Dive lists are newest-first and accept `limit` (default 50, maximum 100), an
opaque `cursor`, inclusive local-date filters `from` and `to` in `YYYY-MM-DD`
format, and a case-insensitive `location` substring. For example:

```text
/api/v1/dives?from=2026-01-01&to=2026-08-31&location=noumea&limit=20
```

When more results exist, use the returned `next_cursor` unchanged in the next
request. Sample lists use the same scheme with a default of 5,000 and maximum
of 10,000 rows per page. The dive detail endpoint includes the available original
Garmin JSON documents, the complete `fit` document, record metadata, and alerts; the samples endpoint
provides graph-ready elapsed time, depth, heart rate, temperature, gas, and
decompression values. Dive details also include a `media` array with attachment
metadata, original URLs, and available thumbnail URLs.

Select a cover photo using the ID returned by the media API:

```sh
curl -X PATCH -H 'Content-Type: application/json' \
  -d '{"cover_media_id":"MEDIA_ID"}' \
  http://127.0.0.1:8080/api/v1/dives/123456789
```

`GET /api/v1/dives` includes `cover_media_id` and a lightweight `cover_photo`
object in each item, and the dive detail's `dive` summary contains the same fields:

```json
{
  "cover_media_id": "MEDIA_ID",
  "cover_photo": {
    "id": "MEDIA_ID",
    "filename": "Turtle.jpeg",
    "url": "/assets/123456789/Turtle.jpeg",
    "thumbnail_url": "/api/v1/dives/123456789/media/MEDIA_ID/thumbnail?v=VERSION"
  }
}
```

The frontend can use `cover_photo.thumbnail_url`, falling back to `cover_photo.url`
when thumbnail generation failed. Both cover fields are `null` when unset. Send
`{"cover_media_id":null}` to clear the selection. Only a photo attached to that
dive can be selected; invalid selections leave the previous cover unchanged.
Garmin synchronization and media rescans preserve the choice. Deleting the photo
through the API, or removing it from disk and rescanning, clears the cover.

## Photos and videos

Start the server with a media directory:

```sh
go-garmindives serve -output-dsn /path/to/dives.db -assets-dir /path/to/assets
```

`-assets-dir` defaults to `assets` in the working directory. It can also be set
with `GARMIN_ASSETS_DIR`. Keep the same assets directory with its database.
Install **FFmpeg**, including `ffmpeg` and `ffprobe`, on `PATH` for metadata and
thumbnail generation (`brew install ffmpeg` on macOS, or your Linux package
manager's FFmpeg package). Thumbnail processing currently targets Linux/macOS.
The [FFprobe documentation](https://ffmpeg.org/ffprobe.html) describes the stream
and format metadata retained in each media item's `probe` document.

Copy originals directly into a directory named with the **activity ID** returned
as `dive.id` by the API (the SQL `dives.id`, not `garmin_dive_id` or dive number):

```text
assets/
  123456789/
    Turtle original.jpeg
    Reef 360.MP4
```

Original filenames, capitalization, and bytes are preserved. Filenames must be
at most 255 bytes and cannot contain path separators, control characters, or
start with a dot. Activity directories contain files directly; nested directories
and symlinks are skipped. Supported extensions include JPEG/JPS, PNG, GIF, WebP,
HEIC/HEIF, AVIF, TIFF, BMP, MP4/M4V, MOV, MKV, WebM, AVI, MTS/M2TS, 3GP, INSV, and .360.
Actual metadata/thumbnail support depends on the installed FFmpeg build.

Every `serve` startup scans the assets directory **before listening**. It adds
new files, updates changed files, rebuilds missing/failed thumbnails, and removes
index entries and cached thumbnails for originals deleted from disk. Unchanged
files are skipped using size and modification time. Folders whose activities
are not yet in the database are logged and left untouched for a later scan.
Each changed file and a final added/updated/unchanged/removed/skipped/failed count
are logged. Individual processing failures are logged without stopping the server.
The `sync` command continues to synchronize Garmin data; it does not scan media.

For files copied while the server runs, call the rescan endpoint below or restart
`serve`. There is no background filesystem watcher. Copy through a hidden temporary
filename, then rename into place once finished to avoid scanning incomplete files.
Use `?force=true` to hash/reprocess every file, including changes made by tools
that preserve both size and modification time.

The media APIs have no authentication, as with the existing dive API:

| Method and path | Purpose |
| --- | --- |
| `POST /api/v1/dives/{id}/media` | Upload one multipart file, field name `file` |
| `GET /api/v1/dives/{id}/media` | List media, sorted by filename; `limit` defaults to 50 (maximum 100), with an opaque `cursor` |
| `GET /api/v1/dives/{id}/media/{mediaID}` | Read attachment metadata |
| `PATCH /api/v1/dives/{id}/media/{mediaID}` | Update caption or 360-degree projection settings |
| `DELETE /api/v1/dives/{id}/media/{mediaID}` | Delete the original, thumbnail, and metadata |
| `GET /api/v1/dives/{id}/media/{mediaID}/thumbnail` | Serve the JPEG thumbnail |
| `GET /assets/{activityID}/{original filename}` | Serve the original; URL-encode the filename |
| `POST /api/v1/media/scan` | Scan/reconcile files; optional `?force=true` |
| `GET /api/v1/media/status` | Read totals, thumbnail status counts, and upload limits |

For example:

```sh
curl -F 'file=@/path/to/Turtle original.jpeg' \
  http://127.0.0.1:8080/api/v1/dives/123456789/media

curl -X POST http://127.0.0.1:8080/api/v1/media/scan

curl -X PATCH -H 'Content-Type: application/json' \
  -d '{"caption":"Reef wall panorama","projection":"360"}' \
  http://127.0.0.1:8080/api/v1/dives/123456789/media/MEDIA_ID
```

Uploads stream to disk and publish only after the complete multipart request is
validated. They return `201` with the media object and a `Location` header.
An existing filename within the activity returns `409`; uploads never overwrite
it, even if the existing file has not been indexed yet. The default per-file
upload limit is **16 GiB**, configurable with `-media-max-upload-bytes` or
`GARMIN_MEDIA_MAX_UPLOAD_BYTES`; oversized uploads return `413`. Direct filesystem
imports have no upload-size limit. Uploads have a one-hour request read deadline.

Media objects include `id`, `dive_id`, `filename`, `kind`, `content_type`,
`size_bytes`, `sha256`, `width`, `height`, `duration_seconds`, `caption`,
`projection`, `effective_projection`, optional `projection_format`, timestamps, `probe`, `url`,
and `thumbnail_status` (`ready` or `error`). A successful thumbnail adds
`thumbnail_url`; a failed inspection or thumbnail adds `thumbnail_error` instead.
Originals are still accepted and served when tools/codecs are missing; install
the tools and rescan to retry. Thumbnails are JPEG images with a maximum dimension
of 640 pixels, stored separately under `assets/.thumbnails`.

Original and thumbnail endpoints support `HEAD`, HTTP byte ranges for video
seeking, ETags, and conditional requests. Directory browsing is disabled. If an
original changed on disk since indexing, serving returns `409 media_changed`
until a rescan refreshes its metadata and validators.

**360-degree videos** retain their complete original container and spherical
metadata. New media uses `projection: "auto"`: explicit spherical mapping metadata
identifies full equirectangular/cubemap 360 footage. A wide aspect ratio or filename
alone does not mark a file as 360. `effective_projection` resolves to `flat`, `180`,
or `360` so the frontend can choose its player; `projection_format` identifies
the source texture format when known.

For a stitched equirectangular export without spherical metadata, set
`projection: "360"` through PATCH, as shown above. `flat` forces normal playback;
`auto` returns to metadata detection. Changing projection regenerates the
thumbnail, and rescans preserve explicit overrides. Existing media previously
stored with `flat` keeps that setting until updated through PATCH.

Equirectangular 360 media gets a **640x360 perspective thumbnail**, using FFmpeg's
[v360 filter](https://ffmpeg.org/ffmpeg-filters.html#v360), for a normal preview in
the frontend. Other projection formats keep a scaled decoded frame. Stereo layout
fields remain optional for stereoscopic footage; ordinary 360 video needs no stereo
setting. Originals are served with byte ranges for the frontend's 360 player.
The server does not stitch raw camera footage or convert playback formats.
For reliable panoramic previews and browser playback, use a stitched equirectangular
MP4/MOV export from your camera software. Raw `.insv`/`.360` files are retained and
served, with preview availability depending on FFmpeg's support for that format.

The `dive_media` table stores metadata only. Back up **both the database and assets
directory** to retain your originals. Removing an attachment through the API deletes
its files; deleting an entire dive from SQL cascades its media metadata but leaves
the originals available for a later reimport.

## Development

```sh
go test ./...
go test -race ./...
go vet ./...
```

Tests are offline by default, including Personal API authentication, incremental
sync, FIT decoding, developer/unknown fields, invalid values, compressed
timestamps, CRC errors, transactional backfills, YAML/SQLite output, REST
responses, and missing versus zero measurements.
Documentation tests check that API routes and response fields match the OpenAPI
contract, that its references resolve, and that the bundled UI/spec routes work.
CI also validates the specification with `openapi-spec-validator`.
Original FIT storage tests cover binary round trips, offline re-decoding,
migration from existing JSON, rollback, checksums, and cascading deletion.
Request-pacing tests use a fake clock and cover concurrent callers, HTTP 429,
`Retry-After`, retry limits, token renewal, and cancellation without contacting
Garmin or waiting through real cooldowns.
To validate the official API against one real dive with temporary YAML and
SQLite output, set `GARMIN_PERSONAL_API_KEY` and run:

```sh
go test -run '^TestGarminPersonalDiveIntegration$' -count=1 -v
```

The test checks the real FIT depth profile as well as persistence and incremental
sync. It skips when the key is absent and never prints the key or dive documents.
An independent opt-in integration test logs into the legacy Garmin APIs,
requires an account containing at least one dive, downloads its details, and
verifies that a second synchronization adds nothing:

```sh
GARMIN_EMAIL='...' GARMIN_PASSWORD='...' GARMIN_MFA_CODE='...' \
  go test -run TestGarminDiveIntegration -count=1 -v
```

SQLite storage is covered by the default hermetic suite. PostgreSQL can also be
validated against a disposable database:

```sh
TEST_POSTGRES_DSN='postgres://...' go test -run TestPostgresStoreIntegration -v
```

A complete existing YAML archive can be run through the SQL mapper as an
additional regression test:

```sh
TEST_DIVE_ARCHIVE=/path/to/dives.yaml go test -run TestDiveArchiveSQLIntegration -v
```

## Credits

Developed with Codex using
[go-garminconnect](https://github.com/se0wtf/go-garminconnect), whose
legacy authentication implementation is based on research from
[python-garminconnect](https://github.com/cyberjunky/python-garminconnect).
FIT decoding uses [muktihari/fit](https://github.com/muktihari/fit).
