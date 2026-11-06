# go-garmindives

`go-garmindives` stores every Garmin Connect diving activity in SQLite or
PostgreSQL. It supports Garmin's official Personal API through an API key,
and the legacy Connect/Dive APIs through an account login. Each dive preserves
the available JSON documents, original FIT files, and all decoded FIT messages,
including depth-over-time samples. The local API also manages photos and videos
attached to each activity, with media files on disk and metadata in SQL.

The app is unofficial and is not affiliated with Garmin.

For product concepts, frontend integration flows, backend algorithms, storage,
security, deployment, and reimplementation acceptance scenarios, start with
[`SPECS.md`](SPECS.md). The OpenAPI contract complements those guides with exact
HTTP request/response schemas.

Source packages and test locations are described in
[repository organization](docs/specs/repository.md). The root executable delegates
to `internal/app`; independent parsing, configuration, provider, security, and
media-processing code lives in dedicated internal packages.

## Install

Requires Go 1.26 or newer.

```sh
go install github.com/se0wtf/go-garmindives@latest
```

### Docker

Build the image and start the API with persistent storage:

```sh
docker build -t go-garmindives:local .
docker run -d --name go-garmindives \
  -p 127.0.0.1:8080:8080 \
  --mount source=garmindives-data,target=/data \
  go-garmindives:local
```

The image includes FFmpeg/FFprobe, JPEG XL encoder/decoder (`cjxl`, `djxl`),
libheif tools with the libde265 HEVC decoder, ExifTool, TLS certificates, and timezone data.
It runs as UID/GID **10001:10001**. With default
settings, `/data` contains `config.yaml`, SQLite `dives.db`, and the `assets/`
directory. Mount the whole configuration directory so atomic YAML saves work.
Host bind mounts must be writable by this UID/GID. No personal config, database,
media, or credentials are copied into the image.

Complete setup through the frontend as described below. Secure cookies are enabled
for production HTTPS; for local HTTP testing add `-e GARMIN_COOKIE_SECURE=false`
to `docker run`. The published port above is local to the host; a frontend container
should reach the backend on a shared Docker network at port 8080.

The same image runs one-shot synchronization after setup:

```sh
docker run --rm \
  --mount source=garmindives-data,target=/data \
  --mount source=garmindives-tokens,target=/tokens \
  --env-file /path/to/private-sync.env \
  go-garmindives:local sync
```

Put Garmin credentials/API key in the private runtime env file. The default token
cache lives under `/tokens/go-garmindives/`; persist it across sync runs. PostgreSQL
uses the usual YAML or `GARMIN_DB_TYPE` / `GARMIN_DB_URI` settings in both backend
roles. Schedule the sync invocation externally; the image has no internal cron.
CLI arguments after the image replace `serve`, for example
`admin reset-password` (use `docker run -it` for its password prompt).

## Configuration and administrator setup

The application uses **SQL storage only**: SQLite or PostgreSQL. The old YAML/file
archive output and `-output` flag have been removed. Raw Garmin JSON and original
FIT BLOBs remain inside SQL. YAML is used only for application settings.

Start the API, even before a database has been configured:

```sh
# Local HTTP development; keep secure cookies enabled behind production HTTPS.
GARMIN_COOKIE_SECURE=false go-garmindives serve -config ./config.yaml
```

`-config` or `GARMIN_CONFIG` selects the file (default `config.yaml`). See
[`config.example.yaml`](config.example.yaml). Relative SQLite paths and media
directories resolve against the configuration directory. Explicit command flags
win over **ENV → YAML → defaults**. The YAML file is saved atomically with mode
`0600`. `GARMIN_DB_TYPE` and `GARMIN_DB_URI` override `database.type` and
`database.uri`; the older `GARMIN_OUTPUT_TYPE` / `GARMIN_OUTPUT_DSN` aliases also
work, with the newer names taking precedence. ENV secrets are not copied into
the YAML file automatically.

A minimal configuration before setup is:

```yaml
version: 1
initialized: false
database:
  type: sqlite
  uri: dives.db
```

The frontend setup flow is:

1. `GET /api/v1/setup/status` returns `configured`, `ready`, `state`, setting
   override sources, and password-length constraints. Before initialization,
   normal APIs and asset routes return `503` / `not_configured`.
2. `GET /api/v1/setup/countries` supplies the bilingual country choices without
   requiring a database.
3. `POST /api/v1/setup` accepts:
   ```json
   {
     "database": {"type": "sqlite", "uri": "dives.db"},
     "admin_password": "your long unique passphrase",
     "home_countries": [
       {"country_code": "FR", "from": "2020-01-01", "to": null}
     ]
   }
   ```
4. Setup checks/migrates SQL, preserves existing dives/FIT/media, creates the
   administrator, and records residence history. At least one residence period
   must exist. Identical existing periods are reused; overlaps are rejected.
5. A successful response is `201` with `configured: true`, `ready: false`,
   `state: "starting"`. Poll status until ready; then log in. Startup media
   indexing and the configured workers start in the same Go process.

Setup deliberately has **no extra setup token**. After completion, the setup
endpoint returns `409 already_configured`. A database outage or a missing admin
row never reopens it. Do not reset `initialized` or `instance_id` to move databases;
copy/migrate the initialized SQL data and preserve the configuration identity.
If the YAML file was lost, supplying the existing SQL administrator password
through setup can adopt that database without resetting credentials or history.
A failed configuration-file save rolls SQL initialization back.

### Login and edit permissions

Anonymous users can read configured dives, trips, and media. Every mutation
(POST/PUT/PATCH/DELETE, including uploads, scans, retries, and regrouping) requires
an administrator session. Application settings and `/api/v1/admin/` routes are
private even for GET requests.
No external authentication service or JWT infrastructure is needed.

- `POST /api/v1/auth/login`: JSON `{"password":"..."}`; sets the session cookie.
- `GET /api/v1/auth/session`: `{ "authenticated": true, "expires_at": "..." }`,
  or `{ "authenticated": false }` for an anonymous/expired session.
- `POST /api/v1/auth/logout`: deletes the current SQL session and clears its cookie.
- `POST /api/v1/auth/password`: JSON `current_password` and `new_password`;
  revokes **all** sessions, including requests holding an older credential version.
- Unauthenticated writes return `401 authentication_required`; login failures
  return `401 invalid_credentials`. Rate-limited attempts return `429` with
  `Retry-After`.

Passwords require **15–128 Unicode characters**, support passphrases and significant
whitespace, and reject common/repeated patterns. Passwords are salted and hashed
with Argon2id (64 MiB, three iterations); only hashes live in SQL. Login/setup
hashing concurrency and password attempts are bounded. Recovery requires local
access to the backend:

```sh
go-garmindives admin reset-password -config /config/config.yaml
```

The command prompts securely and revokes all sessions. It never accepts a password
as a command argument or stores it in YAML.

Sessions use SCS with the existing SQL database. Only token digests are stored.
The cookie is `HttpOnly`, host-only, `SameSite=Strict`, `Path=/`, and `Secure` by
default (`__Host-garmindives_session`). For explicit local HTTP development,
`GARMIN_COOKIE_SECURE=false` uses `garmindives_session`. Same-origin browser
requests send cookies automatically; JavaScript should not store session tokens.
Go's cross-origin protection rejects cross-site writes, including login and setup.
Wildcard CORS is disabled. Password-attempt rate limits use the connection's peer
address, never untrusted forwarded-IP headers.

YAML settings include:

```yaml
http:
  listen: 0.0.0.0:8080
  cookie_secure: true
  # public_url: https://dives.example.com
auth:
  session_lifetime: 24h
  idle_timeout: 2h
media:
  directory: assets
  max_upload_bytes: 17179869184
```

ENV overrides: `GARMIN_API_ADDR`, `GARMIN_COOKIE_SECURE`, `GARMIN_PUBLIC_URL`,
`GARMIN_SESSION_LIFETIME`, `GARMIN_SESSION_IDLE_TIMEOUT`, `GARMIN_ASSETS_DIR`,
`GARMIN_MEDIA_MAX_UPLOAD_BYTES`. Durations use Go syntax (`24h`, not `1d`). Sessions
expire on idle timeout even before their absolute lifetime. Credential changes
and logout take effect immediately; sessions otherwise survive backend restarts.

`GET /api/v1/config` reports current/pending settings, override sources, and a
redacted database destination. `PATCH /api/v1/config` accepts a `database` object
with `type`/`uri` to save changes for the next restart. It does not move data or
switch live connections. Restart both backend roles after explicitly migrating
SQL. Setting values managed by ENV/flags cannot be overridden through the API.

### Serve and sync containers

For a frontend, serve backend, sync backend, and SQL database on one server:

- Share the **configuration directory** between the two backend containers;
  serve needs write access and sync can mount it read-only. Mount the directory,
  not just the file, because configuration saves use atomic replacement.
- Use the same `GARMIN_CONFIG` path and SQL ENV overrides in both backends.
  The sync command reads configuration on each invocation and checks the SQL
  initialization identity before contacting Garmin. It needs no browser login.
- Start the frontend before setup is complete. Proxy `/api`, `/assets`, and docs
  to the serve backend under the same HTTPS origin, preserving `Host` and browser
  origin/fetch headers. `GARMIN_PUBLIC_URL` can specify the external origin when
  the proxy changes `Host`. Keep the backend on the private container network.
- `/healthz` is process liveness and is available before setup; `/readyz` returns
  `503` until normal APIs are ready and whenever the SQL connection fails its
  health check. Gate the sync container/scheduled sync on
  readiness. Do **not** gate frontend startup on readiness, because it serves setup.
- The sync role gets Garmin credentials/API key through ENV as before. The serve
  role does not need Garmin credentials. SQLite deployments must also share the
  database volume at matching paths; PostgreSQL deployments use the shared URI.

With curl, save the cookie returned by login (`-c admin-cookies.txt`) and include
`-b admin-cookies.txt` on authenticated requests. The mutation examples below
assume this cookie file already exists. Swagger's **Try it out** can call login
first and then uses the browser cookie automatically.

## Usage

Create a key in [Garmin Connect's Personal API settings](https://connect.garmin.com/app/settings/personalApiToken)
and supply it to the sync command after initial setup:

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

The internal `fit_json` document in SQL contains every decoded message:
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
`dives.db` relative to the configuration directory, with owner-only permissions. Set another
database path with:

```sh
go-garmindives -output-dsn /path/to/dives.db
```

The database path can also be configured through the environment:

```sh
GARMIN_OUTPUT_DSN=/path/to/dives.db go-garmindives
```

`-output-type` and `-output-dsn` remain SQL-only command overrides. The selected
database must match the initialized configuration identity.

For PostgreSQL, provide a standard connection URL:

```sh
GARMIN_DB_TYPE=postgres \
GARMIN_DB_URI='postgres://user:password@localhost:5432/garmindives?sslmode=require' \
go-garmindives serve
```

Use the same ENV values for the separate sync role after setup.
The DSN is never written to application logs. The same settings are available
as `-output-type` and `-output-dsn` flags; flags override environment values.

Dives without FIT data are automatically backfilled on the next sync, including
previously imported summaries and legacy dives. Backfilling keeps their original
JSON documents and fills missing SQL summary values from FIT. When FIT contains
samples, those become the indexed profile. Once FIT has been imported, the dive
is skipped on later runs, even if its file has no profile samples.

To refresh one dive after editing its notes on Garmin, select its activity ID:

```sh
go-garmindives sync -activity-id 24424941853
```

This fetches only the selected dive directly, without listing activity history
or importing other dives. It refreshes the source metadata and reparses the
rating, comment, and highlights. Existing FIT files, profile samples, media
descriptions, and cover selection are retained. FIT is downloaded only if it is
missing; a dive not yet stored is imported on its own. The usual authentication,
request pacing, and output flags also apply, for example `-output-dsn /path/to/dives.db`.
The translation worker discovers changed French text while `serve` is running.

Downloads are sequential. SQL batches, including FIT backfills, commit in one
transaction. Corrupt or unavailable original files fail the sync without saving
partial changes. Configuration, SQLite, and token files use mode `0600` where applicable.

## Countries, years, residence history, and trips

Dive list/detail summaries now expose `year`, bilingual `country`,
`detected_country`, `country_override`, `country_source`, `trip_id`, and
`trip_assignment`. Years use the dive's **local date**. Combine the existing
date/location filters with `year`, `country`, or `trip_id`:

```sh
curl 'http://127.0.0.1:8080/api/v1/dives?year=2025&country=Egypt'
curl 'http://127.0.0.1:8080/api/v1/dives?country=Egypte'
```

Use **`GET /api/v1/dives/years`** to populate the frontend's year dropdown:

```json
{"items":[2026,2025,2023]}
```

Only years present in stored dive local dates are returned, unique and newest
first. Missing dates are omitted; an empty logbook returns `{"items":[]}`.
The complete list is independent of pagination and active country/rating/date
filters. Choose a value with `/api/v1/dives?year=2025`, or omit `year` for all
years. Refetch after imports/refreshes; no restart or Garmin request is needed.

Countries use stable uppercase codes and both names, for example
`{"code":"EG","name_fr":"Égypte","name_en":"Egypt"}`. Filters accept a
code or either name, ignoring case and accents. `GET /api/v1/countries?q=egypte`
provides the searchable catalog. France (`FR`), New Caledonia (`NC`), and French
Polynesia (`PF`) are separate destinations.
Country-choice endpoints return unique canonical codes with French and English
names. Deprecated aliases and unlabeled historical regions are omitted from the
choices; unambiguous legacy inputs such as `UK` still resolve to the current code.

Use **`GET /api/v1/dives/countries`** for the frontend's dive-country filter.
It returns only distinct effective countries in stored dives, with `code`,
`name_fr`, and `name_en`, sorted by code. Manual overrides replace detections;
unknown countries are omitted. An empty logbook returns `{"items":[]}`. Optional
`?q=egypte` searches these choices. The list covers the whole logbook, independently
of dive pagination/year/date filters, and updates as countries are resolved or edited.
Canonical filter codes also match historical aliases in existing dive rows.
Keep `GET /api/v1/countries` for the **worldwide home-country selector**, or
`GET /api/v1/setup/countries` before initial setup. All three are local reads;
requesting choices never triggers a geocoding call.

### Automatic country lookup

While `serve` runs, a background worker resolves entry GPS coordinates, falling
back to exit GPS when entry coordinates are missing/invalid. Each position is
cached in `country_lookup_cache` using six decimal places. New imports and GPS
edits are discovered without waiting in API requests; completed positions are
reused across dives and restarts. Country detection also works offshore when
the provider has a country boundary for the location: the Small Brother test
position returned Egypt. Unresolved locations remain unknown and can be corrected.

The default external provider is **Nominatim/OpenStreetMap**. Using it sends only
GPS coordinates and country-lookup options, without activity IDs, dates, names,
or Garmin credentials. Requests identify the application and are paced at least
**15 seconds apart** across processes sharing the database (four per minute).
This intentionally makes the first backfill gradual. The worker respects
`Retry-After`, limits retries, and stops repeating successful/unresolved lookups.
Provider configuration/authentication rejections pause lookups until an explicit
retry or restart. Run public Nominatim lookups on one machine; disable the worker
on any additional deployment instances. Regional ISO identifiers distinguish
French overseas destinations from France, even when the provider's sovereign
country field says France.
See the [Nominatim usage policy](https://operations.osmfoundation.org/policies/nominatim/).
Display the returned attribution when showing geocoded data:
[© OpenStreetMap contributors](https://www.openstreetmap.org/copyright), ODbL 1.0.

| Environment variable | Default / purpose |
| --- | --- |
| `GARMIN_GEOCODING_ENABLED` | `true`; set `false` to disable outgoing country requests |
| `GARMIN_GEOCODING_URL` | `https://nominatim.openstreetmap.org`; switchable origin for a compatible provider/self-hosted instance |
| `GARMIN_GEOCODING_INTERVAL` | `15s`; public Nominatim requires at least this interval for recurring background work |
| `GARMIN_GEOCODING_USER_AGENT` | Identifying app name and repository URL; customize with your app/contact details |

- `GET /api/v1/countries/status`: active unique-position progress and attribution.
- `POST /api/v1/countries/retry`: explicitly retry failed/unresolved positions.
- `PATCH /api/v1/dives/{id}` with `{"country_code":"EG"}`: manual override.
- The same patch with `{"country_code":null}`: restore automatic detection.

Country overrides survive sync and single-dive refreshes. Cached detected
countries remain available with outgoing requests disabled. Country names are
localized locally and require no translation key.

For the admin country-correction page, use
**`GET /api/v1/admin/dives/missing-country?lookup_status=not_found`** to list dives
whose lookup returned no result. An administrator session is required. Each row
contains `dive` (ID, name, date, original GPS, summary) and `lookup` (status, cached
GPS, optional error code, failed attempts, last check in Unix milliseconds).
Choose a country from the full `/api/v1/countries` catalog, then correct it with:

```sh
curl -b admin-cookies.txt -X PATCH http://127.0.0.1:8080/api/v1/dives/DIVE_ID \
  -H 'Content-Type: application/json' -d '{"country_code":"EG"}'
```

Corrected dives immediately leave the review list; trip grouping is recalculated
using residence history. This changes only the selected dive, not every dive at
the same coordinates. Clearing the override with null restores automatic lookup
and can put the dive back in the list. The review GET never calls the provider.
Omit `lookup_status` (or use `all`) to include every dive with no country; other
filters are `error`, `pending`, and `missing_coordinates`. The default page size
is 50 (maximum 100); use `next_cursor` to continue with the same status filter.

### Home-country history

Create residence periods through the API. Dates are inclusive; `to: null`
means the period has no end date. Periods cannot overlap (including shared end
dates). For example:

```sh
curl -b admin-cookies.txt -X POST http://127.0.0.1:8080/api/v1/home-countries \
  -H 'Content-Type: application/json' \
  -d '{"country_code":"FR","from":"2020-01-01","to":"2025-06-30"}'
curl -b admin-cookies.txt -X POST http://127.0.0.1:8080/api/v1/home-countries \
  -H 'Content-Type: application/json' \
  -d '{"country_code":"NC","from":"2025-07-01","to":null}'
```

Use `GET /api/v1/home-countries`, or GET/PATCH/DELETE
`/api/v1/home-countries/{homeID}` to manage history. **No residence is assumed**:
dives outside recorded periods wait before automatic trip assignment. Adding,
editing, or removing a residence period recalculates automatic assignments.

### Dive trips

For dives with a known destination and matching residence period, automatic trips:

- Include dives outside the home country.
- Split on a destination-country change, a home-country dive, an uncovered/
  unknown-country date, or a gap **longer than three calendar months** between
  eligible dives. Month ends are clamped, e.g. January 31 → April 30.
- Keep December/January dives together when those rules allow it. The first
  linked dive supplies the primary country and starting year: `Égypte 2025` /
  `Egypt 2025`, even if the trip finishes in 2026.
- Retain stable trip IDs and custom metadata during ordinary rebuilds.

Manual links can span countries and include any stored dives. Explicitly removed
dives stay excluded from automatic regrouping until restored with
`PATCH /api/v1/dives/{id}` and `{"trip_mode":"auto"}`.

Trip fields include French/English name, year, country, subtitle, description,
manual rating from **0 to 5** (nullable; zero is preserved), `cruise`,
`operator_name`, `operator_website`, `operator_comment`, free-text `price`
(approximate **per-person** cost), cover photo, and derived start/end dates.
Trip editorial text uses the existing cached French-to-English DeepL worker;
generated country/year names are bilingual immediately without DeepL.

| Method / path | Purpose |
| --- | --- |
| `GET /api/v1/trips` | Paginated list; filters `name` (French/English), `year`, `country`, `from`, `to`, `ratings` |
| `POST /api/v1/trips` | Create a manual trip; provide `year`, `country_code`, optional text/metadata and `dive_ids` |
| `GET /api/v1/trips/{tripID}` | Metadata, linked `dive_ids`, own `media`, and `favorite_dive_media` |
| `PATCH /api/v1/trips/{tripID}` | Update metadata, overrides, cover, or replace `dive_ids` |
| `DELETE /api/v1/trips/{tripID}` | Soft-delete trip, excluding its dives; retain dives and original files/media metadata |
| `GET /api/v1/trips/{tripID}/dives` | Linked dives with normal filters and cursor pagination |
| `PUT /api/v1/trips/{tripID}/dives/{id}` | Idempotent manual assignment, moving the dive from its previous trip |
| `DELETE /api/v1/trips/{tripID}/dives/{id}` | Remove a dive and persist its exclusion |
| `POST /api/v1/trips/rebuild` | Recalculate automatic assignments without changing manual links/exclusions |

Date filters match trips **overlapping** the inclusive range; trips without dives
have empty dates and do not match date filters. Name search ignores case and
accents. Missing ratings do not match rating selections. The first/last linked dives
determine dates. `name`, `year`, and `country_code` accept `null` in PATCH to
restore generated values; `name_en` optionally supplies a custom English name.
Replacing `dive_ids` makes listed links manual and persistently excludes removed
members. Deleting a trip does not cause its current dives to reappear in a new
automatic trip. Empty, untouched automatic groups are hidden from the trip list
unless they contain trip media. Backfills prefer retaining a customized/media-
bearing group when joining temporary automatic groups.

Filter trip ratings the same way as dives:

```text
/api/v1/trips?ratings=4,5
/api/v1/trips?ratings=4,4.5,5&year=2026&country=EG
```

Selections match exact values of the **trip's own rating**, independently of
member dives. Zero and decimals are supported; unrated trips are excluded while
filtering. Omit `ratings` to include rated and unrated trips. The removed
`min_rating`/`max_rating` parameters return 400 `invalid_request`; use `ratings`
instead. Keep the same filters while following `next_cursor`.

### Trip media and favorite dive media

Trips own independent photos/videos under
`assets/trips/<tripID>/<original filename>`. The normal startup scan and
`POST /api/v1/media/scan` index these directories too, preserving descriptions,
projection settings and attachment IDs. Uploads, original filenames, 360-degree
hints, photo/video thumbnails, range requests, and duplicate-name protection behave like
dive media:

```sh
curl -b admin-cookies.txt -F 'file=@Le bateau.jpeg' http://127.0.0.1:8080/api/v1/trips/TRIP_ID/media
```

GET/POST `/api/v1/trips/{tripID}/media`, GET/PATCH/DELETE
`/api/v1/trips/{tripID}/media/{mediaID}`, and the corresponding `/thumbnail`
endpoint manage trip media. Originals use `/assets/trips/{tripID}/{filename}`.
Select a trip-owned **photo** with `PATCH /api/v1/trips/{tripID}` and
`{"cover_media_id":"MEDIA_ID"}`; null clears it. Removing the selected photo
clears the cover automatically. Trip deletion retains original files and metadata
but removes the trip and its media from the active APIs.

Mark a dive photo **or video** with:

```sh
curl -b admin-cookies.txt -X PATCH http://127.0.0.1:8080/api/v1/dives/DIVE_ID/media/MEDIA_ID \
  -H 'Content-Type: application/json' -d '{"favorite":true}'
```

Set false to unmark it. Favorites survive rescans and are not copied into trip
storage. `GET /api/v1/media/favorites` lists all favorites with pagination; add
`?trip_id=TRIP_ID` to select favorites from currently linked dives. Trip detail
returns its own `media` separately from `favorite_dive_media` (first 100; use
`favorites_next_cursor` with the favorites endpoint to continue). Moving or
removing a dive immediately updates that collection.

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
Internal SQL FIT documents retain `original_base64` for offline reuse; JSON API
responses never include the full FIT document or original file bytes. A stored
binary file also marks its dive as cached for sync,
even if the decoded JSON is subsequently cleared for reprocessing.

## REST API

The API reads directly from SQLite or PostgreSQL and does not contact Garmin or
require Garmin credentials. Start it with the same SQL configuration used for
synchronization:

```sh
go-garmindives serve -output-type sqlite -output-dsn /path/to/dives.db
```

It listens on `127.0.0.1:8080` by default. Change this with `-listen` or
`GARMIN_API_ADDR`. Reads are public after initial configuration; writes and
application settings require an administrator session. The browser frontend and
API share one origin through the frontend proxy.

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

Select exact dive ratings with `ratings=4,5` (4 OR 5), optionally combined with
the other filters:

```text
/api/v1/dives?ratings=4,5&year=2026
/api/v1/trips/{tripID}/dives?ratings=4,5
```

Decimal scores use a dot (`ratings=4.5`); commas separate choices. Zero is valid,
unrated dives are excluded while filtering, and omitting `ratings` includes all
scores and unrated dives. Scores use their stored value without normalization
by `rating_max` (8/10 matches 8, not 4). At most 100 supplied values are accepted;
duplicates are ignored and empty, negative, nonfinite or malformed choices return
400. The order stays newest first. Keep the same filters on subsequent pages.

When more results exist, use the returned `next_cursor` unchanged in the next
request. Sample lists use the same scheme with a default of 5,000 and maximum
of 10,000 rows per page. The dive detail endpoint includes the available original
Garmin JSON documents, record metadata, and alerts; the samples endpoint
provides graph-ready elapsed time, depth, heart rate, temperature, gas, and
decompression values. Dive details also include a `media` array with attachment
metadata, original URLs, and available thumbnail URLs.

Sample responses contain extracted measurements only; `raw` is omitted for all
callers, including admins and `include_raw=true` requests. Raw record JSON remains
stored internally for future processing. Every sample, its order, and existing
cursor pagination are retained; no chart samples are discarded or downsampled.

Dive JSON omits `fit` entirely, even for administrators and `?include=fit`.
For an authenticated admin download, use:

```text
GET /api/v1/admin/dives/{id}/fit
```

One cached file downloads as an original binary `.fit` attachment; multiple
files download as a ZIP. Downloads read SQL directly, verify file sizes/checksums,
and never contact Garmin. GET/HEAD/range requests require an admin session and
successful downloads use `Cache-Control: private, no-store`. No cached original
returns `404 fit_unavailable`. The frontend should show a download control only
to administrators and use the samples endpoint for public depth/metric charts.

Dive summaries in list/detail responses also include structured fields parsed
from the stored Garmin note. For example:

```text
N:4/5
Commentaire:
Enfin pu faire la passe !
On parle de canyons mais ce sont plus des tranchés que des vrais canyons.
HL:
Pleiiin de requins vers les 50m
Raies aigles
Requin dormeur
```

Produces:

```json
{
  "rating": 4,
  "rating_max": 5,
  "comment": "Enfin pu faire la passe !\nOn parle de canyons mais ce sont plus des tranchés que des vrais canyons.",
  "highlights": ["Pleiiin de requins vers les 50m", "Raies aigles", "Requin dormeur"]
}
```

`N`, `Commentaire`/`Commentaires`, and `HL` labels are case-insensitive and may
appear in any order. Comments retain internal line breaks; highlights are one
nonempty line per item, with optional `-`, `*`, or `•` list bullets removed.
Decimal ratings accept a dot or French comma. A score without a denominator,
such as `N:4`, has `rating_max: null`; no scale is assumed or normalized.
Missing/invalid ratings are `null`, missing comments are `""`, and missing
highlights are `[]`. Zero is a valid rating. Unstructured text stays available
in the original `notes` and `description` fields.

The parser uses legacy `notes` first and falls back per section to the activity
`description`, which contains notes in the Personal API. Raw source text is
preserved. Existing records are backfilled locally when the database opens after
upgrade, without downloading dives again. As with other stored dive metadata,
these fields reflect the imported text; normal incremental sync does not refresh
later edits made on Garmin. Use `sync -activity-id <activityID>` to fetch edited
notes and update the parsed fields for one dive.

### French and English translations

Set a DeepL API key when starting the REST server to automatically translate dive
comments/highlights and media captions/descriptions from French into English:

```sh
export DEEPL_API_KEY='your-deepl-api-key'
go-garmindives serve
```

The existing text fields remain French. List/detail, upload, and metadata-update
responses also contain `translations.en`, for example:

```json
{
  "comment": "Enfin pu faire la passe !",
  "translations": {
    "en": {
      "status": "ready",
      "comment": "Finally got to dive the pass!",
      "highlights": ["Eagle rays", "Sharks"]
    }
  }
}
```

An English block has status `pending`, `ready`, `error`, or `disabled`. While it
is not ready, the frontend should show the French fields. Empty translated fields
are omitted; empty source content is ready immediately without a provider request.
Cover photo descriptions include the same English block in the dive list.

A background worker discovers existing text at startup and reacts to media edits
and uploads. It also checks for changes every 30 seconds, including new dives
written by a separate sync process. API requests only read the cache and never
wait for DeepL. The default **2-second gap** between translation requests is
independent of Garmin's limiter. Temporary failures retry up to five consecutive
attempts with backoff, respecting `Retry-After`; authentication errors and exhausted
provider quotas pause calls until restart or explicit retry. Individual requests
time out after 30 seconds and shutdown cancels ongoing processing.

The `translation_cache` SQL table stores source/result text and work state.
Identical note/media content with the same locale and glossary reuses a cached
result. Changing French text selects a new content hash, so old English is never
shown for a newer source. Completed batches are persisted separately, allowing
retries to resume without translating successful batches again. Older completed
revisions remain reusable if text is restored. Unreferenced pending revisions
are skipped. Cached English continues to work after a restart even without a key.
Original files, ratings, and raw Garmin documents are not sent to the provider.
Only parsed comments/highlights and media captions/descriptions are translated.

Configuration through the environment:

| Variable | Default / purpose |
| --- | --- |
| `DEEPL_API_KEY` | Optional; enables provider calls; never stored or logged |
| `DEEPL_TARGET_LANG` | `EN-GB`; also accepts `EN-US` |
| `DEEPL_REQUEST_INTERVAL` | `2s`; any positive Go duration |
| `DEEPL_API_URL` | `https://api-free.deepl.com` for keys ending in `:fx`, otherwise `https://api.deepl.com`; override with the origin shown for your API account |
| `DEEPL_GLOSSARY_ID` | Optional French-to-English glossary for recurring dive/species names |

The source language is fixed to `FR`. Requests include surrounding French prose
and scuba-diving context to help disambiguate short highlights. See the official
[DeepL translation API](https://developers.deepl.com/api-reference/translate/request-translation)
and [authentication documentation](https://developers.deepl.com/docs/getting-started/auth).
No real provider calls are made by the default test suite.

- `GET /api/v1/translations/status` reports enabled state, locale, active pending/
  ready/error counts, cached revision count, and any quota/authentication pause.
- `POST /api/v1/translations/retry` requeues failed work and clears the pause,
  preserving ready entries. With no API key it returns `503 translation_disabled`.

Large notes are divided into batches of at most 50 nonempty text segments and
120 KiB per request, with progress saved after each batch. A single text segment
that exceeds the request limit has `translation_text_too_large` status and stays
available in French. Account usage limits and billing are managed by DeepL; the
app does not upgrade plans automatically. Cache reuse minimizes repeated usage,
but a network failure after provider processing or failure to persist a response
can still consume quota on retry.

### Cover photos

Select a cover photo using the ID returned by the media API:

```sh
curl -b admin-cookies.txt -X PATCH -H 'Content-Type: application/json' \
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
    "description": "A sea turtle swimming above the reef.",
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
Install **FFmpeg** (`ffmpeg` and `ffprobe`), **JPEG XL tools** (`cjxl` and `djxl`),
and **libheif** (`heif-convert` or `heif-dec`, with an HEVC decoder) on `PATH`:
`brew install ffmpeg jpeg-xl libheif` on macOS, or
`apt-get install ffmpeg libjxl-tools libheif-examples libheif-plugin-libde265`
on Debian/Ubuntu. Media processing targets Linux/macOS.
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

Compatible H.264/AAC videos within 1080p bounds and small photos preserve their
filenames, capitalization, and bytes. Other videos are normalized to H.264/AAC
MP4 without upscaling; non-MP4 names append `.mp4` (e.g. `Reef.mov.mp4`).
Converted photos append `.jxl` to the source filename, for example
`Turtle original.jpeg.jxl`; exceptionally long names are shortened with a hash
to fit the filename limit. Filenames must be
at most 255 bytes and cannot contain path separators, control characters, or
start with a dot. Activity directories contain files directly; nested directories
and symlinks are skipped. Supported extensions include JPEG/JPS, PNG, GIF, WebP,
HEIC/HEIF, AVIF, TIFF, BMP, JXL, MP4/M4V, MOV, MKV, WebM, AVI, MTS/M2TS, 3GP, INSV, and .360.
Actual metadata/thumbnail support depends on the installed FFmpeg build.

Every `serve` activation scans the assets directory **before normal APIs become
ready**; setup/status, documentation, and liveness are already reachable. It adds
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

Media reads are public; uploads, metadata edits, deletions, and rescans require an administrator session:

| Method and path | Purpose |
| --- | --- |
| `POST /api/v1/dives/{id}/media` | Upload one multipart file, field name `file` |
| `GET /api/v1/dives/{id}/media` | List media, sorted by filename; `limit` defaults to 50 (maximum 100), with an opaque `cursor` |
| `GET /api/v1/dives/{id}/media/{mediaID}` | Read attachment metadata |
| `PATCH /api/v1/dives/{id}/media/{mediaID}` | Update description, caption, or 360-degree projection settings |
| `DELETE /api/v1/dives/{id}/media/{mediaID}` | Delete the original, thumbnail, and metadata |
| `GET /api/v1/dives/{id}/media/{mediaID}/thumbnail` | Serve the photo/video thumbnail; optional `format=auto`, `original`, or `jpeg` |
| `GET /assets/{activityID}/{filename}` | Serve the retained media file; URL-encode the filename; same format options |
| `POST /api/v1/media/scan` | Scan/reconcile files; optional `?force=true` |
| `GET /api/v1/media/status` | Read totals, thumbnail status counts, and upload limits |

For example:

```sh
curl -b admin-cookies.txt -F 'file=@/path/to/Turtle original.jpeg' \
  http://127.0.0.1:8080/api/v1/dives/123456789/media

curl -b admin-cookies.txt -X POST http://127.0.0.1:8080/api/v1/media/scan

curl -b admin-cookies.txt -X PATCH -H 'Content-Type: application/json' \
  -d '{"caption":"Reef wall panorama","description":"A sea turtle swimming above the reef near the end of the dive.","projection":"360"}' \
  http://127.0.0.1:8080/api/v1/dives/123456789/media/MEDIA_ID
```

Uploads stream to disk and publish only after the complete multipart request is
validated. They return `201` with the media object and a `Location` header.
An existing filename within the activity returns `409`; an existing source file
is never overwritten, even if it has not been indexed yet. Re-upload of a missing
original HEIC can repair an older conversion as described below. The default per-file
upload limit is **16 GiB**, configurable with `-media-max-upload-bytes` or
`GARMIN_MEDIA_MAX_UPLOAD_BYTES`; oversized uploads return `413`. Direct filesystem
imports have no upload-size limit. Uploads have a one-hour request read deadline.

Media objects include `id`, `dive_id`, `filename`, `kind`, `content_type`,
`size_bytes`, `sha256`, `width`, `height`, `duration_seconds`, `caption`, `description`,
`projection`, `effective_projection`, optional `projection_format`, timestamps, `probe`, `url`,
and `thumbnail_status` (`ready` or `error`). A successful thumbnail adds
`thumbnail_url`; a failed inspection or thumbnail adds `thumbnail_error` instead.
Sources are still accepted and served when tools/codecs are missing; install
the tools and rescan to retry. Video thumbnails are JPEG images with a maximum
dimension of 640 pixels. Photo thumbnails are JPEG XL, as described below.

Photo processing applies to API uploads and filesystem scans, for dives and trips:

| Source condition | Retained photo | Thumbnail |
| --- | --- | --- |
| Strictly less than 1,000,000 bytes **and** shorter side strictly less than 2048 pixels | Original bytes and filename | JXL, shorter side 480, `-d 2.0 -e 8` |
| Any other size/resolution | JXL, shorter side 1080, `-d 1.5 -e 7` | JXL, shorter side 480, `-d 2.0 -e 8` |

Resizing preserves aspect ratio and never upscales. The thumbnail is generated
from the retained photo; stereo and 360 settings affect only its preview.
The source is deleted **only after** the master and thumbnail successfully encode,
decode for verification, and the SQL metadata update succeeds. Failures retain
the source and report `thumbnail_error`. Animated sources requiring conversion
are retained with a processing error rather than silently reduced to a still.
An interrupted publication is recovered on the next scan using `.photo-jobs`.
Existing IDs, descriptions, favorites, and cover selections survive conversion.
A processing marker prevents repeated lossy conversion, including forced rescans.

HEIC/HEIF decoding uses libheif to assemble the full image grid and apply its
crop/rotation/mirror transforms before resizing. FFmpeg's first video stream can
be just one HEVC tile and is never used as a HEIC fallback. A missing HEIF decoder
or a container with multiple top-level images keeps the original with a processing
error instead of publishing an arbitrary image.

To repair photos processed by the older HEIC decoder, restore the original HEIC
under its original filename and rescan, or upload that original again. This repair
exception to duplicate-name protection preserves the existing media ID, description,
favorites and cover selection. Completed repairs restore normal duplicate protection.
Without the original, scanning reports `thumbnail_status=error` and a
`thumbnail_error` requesting that source; a tile-only JXL cannot reconstruct the
missing image. The retained JXL is left intact until a successful repair. HEIF
decoder version 1 is recorded separately from the photo profile and invalidates
older thumbnail/JPEG fallback caches.

Photo and thumbnail URLs default to `format=auto`: clients explicitly advertising
`Accept: image/jxl` receive JXL; other clients receive a JPEG fallback generated
on first use and cached in `.fallbacks`. JPEG uses quality 90 and a white background
for transparency. HEIC/TIFF/BMP also get JPEG fallbacks in auto mode. Responses
include `Vary: Accept` and representation-specific ETags. `format=original` forces
the retained bytes; `format=jpeg` forces a JPEG photo representation. These options
apply equally to trip assets and thumbnails. Metadata `content_type` describes
the retained file; HTTP `Content-Type` describes the selected response.

The frontend can simply use the returned URLs. For explicit `<picture>` selection,
use the returned URL with `format=original` on a `source type="image/jxl"` and
`format=jpeg` on its fallback `img` (preserve the existing `v` query parameter on
thumbnail URLs). `media/status` exposes photo byte/resolution thresholds alongside
the existing video thumbnail limit.

Use `description` to explain what a photo or video shows. It accepts up to 8192
UTF-8 bytes and defaults to an empty string. Edit it through the media PATCH
endpoint; omit it to keep the current text, or send `"description":""` to clear it.
Descriptions are returned by media list/detail and dive detail endpoints, and
selected cover photos include their description in the dive list. Descriptions
persist across filesystem rescans, changed originals, synchronization, and restarts.

Original and thumbnail endpoints support `HEAD`, HTTP byte ranges for video
seeking, ETags, and conditional requests. Directory browsing is disabled. If an
original changed on disk since indexing, serving returns `409 media_changed`
until a rescan refreshes its metadata and validators.

**360-degree videos** preserve the complete panorama and spherical presentation
hints during normalization; SQL retains the projection metadata. New media uses `projection: "auto"`: explicit spherical mapping metadata
identifies full equirectangular/cubemap 360 footage. A wide aspect ratio or filename
alone does not mark a file as 360. `effective_projection` resolves to `flat`, `180`,
or `360` so the frontend can choose its player; `projection_format` identifies
the source texture format when known.

For a stitched equirectangular export without spherical metadata, set
`projection: "360"` through PATCH, as shown above. `flat` forces normal playback;
`auto` returns to metadata detection. Changing projection regenerates the
thumbnail, and rescans preserve explicit overrides. Existing media previously
stored with `flat` keeps that setting until updated through PATCH.

Equirectangular 360 video gets a **640x360 perspective thumbnail**, using FFmpeg's
[v360 filter](https://ffmpeg.org/ffmpeg-filters.html#v360), for a normal preview in
the frontend. Photo perspective previews target a 480-pixel shorter side without
upscaling. Other projection formats keep a scaled decoded frame. Stereo layout
fields remain optional for stereoscopic footage; ordinary 360 video needs no stereo
setting. Originals are served with byte ranges for the frontend's 360 player.
The server normalizes video playback to H.264/AAC MP4, but does not stitch raw
camera footage. The 1080p landscape limit makes a 2:1 panorama at most 1920x960.
For reliable panoramic previews and browser playback, use a stitched equirectangular
MP4/MOV export from your camera software. Raw multiple-stream `.insv`/`.360`
footage requires stitching before conversion; failures retain the source.

The `dive_media` table stores metadata only. Back up **both the database and assets
directory** to retain your media. Photo/video conversion replaces eligible originals
with the retained JXL/MP4; keep a separate source backup if desired. Removing an attachment through the API deletes
its files; deleting an entire dive from SQL cascades its media metadata but leaves
the originals available for a later reimport.

## Pending media inbox

Drop original photos or videos into `<assets_dir>/pending` (`/assets/pending` for an assets
root of `/assets`, or `/data/assets/pending` in the default Docker setup). Serve
scans it at startup and every **60 seconds**. A file must settle before import;
for slow copies use a hidden temporary filename, then rename it into place.

Capture time is read from EXIF/XMP/GPS metadata. A missing timezone is interpreted
in the **dive's local timezone**. Exactly one matching dive in the inclusive
**start − 10 minutes through end + 10 minutes** window is assigned automatically.
Ambiguous/missing dates, no match, filename conflicts and processing failures stay
pending for the administrator. Current SQL dives are reconsidered on every pass,
so a later sync can resolve a waiting photo. Filesystem dates are not capture dates.

Assignment uses normal JXL photo processing or H.264/1080p video conversion and thumbnails. The pending original is removed
only after completed processing and SQL save; capture metadata is retained as
`media.capture`. A recorded import recovers safely after interruption.

All inbox routes require an admin session under `/api/v1/admin/media/pending`:

| Method/path | Purpose |
| --- | --- |
| GET root | List media kind, capture dates, reasons and candidate dives; `limit`, `cursor`, `reason` |
| GET `/status` | Counts and scan status |
| POST `/scan` | Queue scan; optional `?force=true` |
| GET `/{id}` | Read one pending item |
| GET/HEAD `/{id}/thumbnail` or `/{id}/file` | Private preview/original |
| POST `/{id}/assign` | Manually assign with `{"dive_id":123456789}` |
| DELETE `/{id}` | Delete pending media |

YAML `media.pending_directory` defaults to `pending`, relative to the assets root;
absolute paths work too. `media.pending_scan_interval` defaults to `60s`.
ENV overrides are `GARMIN_PENDING_DIR` and `GARMIN_PENDING_SCAN_INTERVAL`.
Local builds need `brew install exiftool` or `apt-get install libimage-exiftool-perl`;
the Docker image already includes it. Resolve exports with only a render date
remain pending; a preserved/restored capture tag enables matching. See the
[full inbox specification](docs/specs/pending-media.md) and
[video encoding profile](docs/specs/video-processing.md).

## Development

```sh
go test ./...
go test -race ./...
go vet ./...
```

Tests are offline by default, including Personal API authentication, incremental
sync, FIT decoding, developer/unknown fields, invalid values, compressed
timestamps, CRC errors, transactional backfills, SQL-only storage and setup/authentication, REST
responses, and missing versus zero measurements.
Documentation tests check that API routes and response fields match the OpenAPI
contract, that its references resolve, and that the bundled UI/spec routes work.
CI also validates the specification with `openapi-spec-validator` and runs the
PostgreSQL integration tests, including initial setup, binary session storage,
and password revocation, against a disposable database.
Original FIT storage tests cover binary round trips, offline re-decoding,
migration from existing JSON, rollback, checksums, and cascading deletion.
Request-pacing tests use a fake clock and cover concurrent callers, HTTP 429,
`Retry-After`, retry limits, token renewal, and cancellation without contacting
Garmin or waiting through real cooldowns.
To validate the official API against one real dive with temporary SQLite storage, set `GARMIN_PERSONAL_API_KEY` and run:

```sh
go test ./internal/app -run '^TestGarminPersonalDiveIntegration$' -count=1 -v
```

The test checks the real FIT depth profile as well as persistence and incremental
sync. It skips when the key is absent and never prints the key or dive documents.
An independent opt-in integration test logs into the legacy Garmin APIs,
requires an account containing at least one dive, downloads its details, and
verifies that a second synchronization adds nothing:

```sh
GARMIN_EMAIL='...' GARMIN_PASSWORD='...' GARMIN_MFA_CODE='...' \
  go test ./internal/app -run TestGarminDiveIntegration -count=1 -v
```

SQLite storage is covered by the default hermetic suite. PostgreSQL can also be
validated against a disposable database:

```sh
TEST_POSTGRES_DSN='postgres://...' go test ./internal/app -run TestPostgresStoreIntegration -v
```

## Credits

Developed with Codex using
[go-garminconnect](https://github.com/se0wtf/go-garminconnect), whose
legacy authentication implementation is based on research from
[python-garminconnect](https://github.com/cyberjunky/python-garminconnect).
FIT decoding uses [muktihari/fit](https://github.com/muktihari/fit).
