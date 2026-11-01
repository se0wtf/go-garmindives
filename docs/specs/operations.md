# Running, maintaining, and recovering the application

[Specification index](../../SPECS.md) · [Configuration/security](configuration-security.md)

## Commands and deployment roles

The reference binary requires Go 1.26+ to build. `go-garmindives` and
`go-garmindives sync` are equivalent one-shot sync commands. `serve` is a long-running
HTTP process. `admin reset-password` is an interactive local recovery command.
Use `-help` / `serve -help` for flags. Runtime application settings are YAML plus
ENV; Garmin/DeepL/geocoding settings listed below currently remain ENV-only.

| Flag | Applies to | Meaning |
| --- | --- | --- |
| -config PATH | all roles | YAML settings file |
| -output-type sqlite/postgres | sync/serve | SQL dialect override |
| -output-dsn VALUE | sync/serve | SQLite file or PostgreSQL URI/DSN override |
| -activity-id ID | sync | Refresh only this positive activity ID |
| -token-file PATH | sync | Garmin token cache path |
| -listen ADDR | serve | Listener override |
| -assets-dir PATH | serve | Original media/cache root override |
| -media-max-upload-bytes N | serve | Per-file upload limit override |

For same-server containers:

| Role | Durable access / network |
| --- | --- |
| Frontend | Serve UI and proxy backend paths under the same HTTPS origin |
| Serve backend | Writable config **directory**, SQL, assets; outgoing geocoding/optional DeepL; no Garmin credentials required |
| Sync backend | Readable config directory, same SQL settings, private Garmin token-cache volume; outgoing Garmin |
| PostgreSQL | Persistent SQL volume; reachable by both backend roles |

Use `http.listen: 0.0.0.0:8080` or GARMIN_API_ADDR in the serve container; the
loopback default is for local use. Share configuration by directory because saves
rename the file. Mount SQLite storage too if using SQLite; paths/permissions must
agree between roles. Match the config-reader UID/group to mode-0600 ownership.
The sync process does not need the media filesystem because it does not scan it.

The proxy must forward `/api/`, `/assets/`, `/swagger/`, `/docs`, and any public
probe paths being used. Preserve Host, Origin, and Sec-Fetch-Site. If Host changes,
set the exact external origin in GARMIN_PUBLIC_URL. Preserve Range, conditional
headers, and response validators for video seeking. TLS terminates at the proxy;
secure cookies stay enabled. Local HTTP testing uses explicit
`GARMIN_COOKIE_SECURE=false`. The binary does not provision TLS certificates.

Do not gate frontend startup on backend readiness: setup is rendered by the
frontend while readiness is false. Start serve, visit the frontend setup page,
complete setup, then wait for ready before scheduling sync.

## Startup, background tasks, and scheduling

Serve exposes its listener while runtime activation proceeds. Activation validates
settings, opens/migrates SQL, verifies instance identity, reconciles country/trips,
opens assets and completes startup scanning, then installs the normal API/auth
runtime and starts translation/geocoding workers. Per-file processing failures
are recorded/logged; fatal scan/storage errors keep runtime unavailable and trigger
activation retry after five seconds. A large first scan may take minutes while
status/docs/liveness stay available. Graceful HTTP shutdown has a ten-second
budget; worker cancellation and lease cleanup follow.

| Task | Trigger / interval | Network |
| --- | --- | --- |
| Garmin sync | Explicit command or external scheduler | Garmin; default 2-second pacing |
| Startup assets scan | Each successful serve activation | Local disk / FFmpeg |
| Later assets scan | Admin POST scan, optional force=true | Local disk / FFmpeg |
| Country/trip reconciliation | Startup, domain transactions, approximately every 30 seconds | SQL only |
| Country provider | Referenced queued coordinates while serve runs | Nominatim; default 15-second database-wide gate |
| Translation discovery | Startup, edits, approximately every 30 seconds | SQL only |
| Translation provider | Pending work while enabled/unblocked | DeepL; default 2 seconds per worker |
| Expired session cleanup | Approximately every 10 minutes | SQL only |

An hourly deployment schedule should invoke `go-garmindives sync -config ...`
after readiness, with overlap prevention and normal process failure reporting.
The binary has no internal hourly scheduler or long-running sync daemon. One
Linux cron example, when the binary/config are directly available to the job:

```cron
0 * * * * flock -n /run/go-garmindives-sync.lock /usr/local/bin/go-garmindives sync -config /config/config.yaml
```

Container scheduling can use the equivalent one-shot invocation. Supply the same
ENV secrets and volumes each time. Locks must cover all scheduled invocations;
separate processes do not share the in-memory Garmin limiter. No credentialed
browser login is needed by sync after application initialization.

## Provider environment settings

| Setting | Default / behavior |
| --- | --- |
| GARMIN_PERSONAL_API_KEY | Optional key; selects official Personal API and takes precedence over legacy |
| GARMIN_EMAIL / GARMIN_PASSWORD | Legacy login; prompt if required/unset |
| GARMIN_MFA_CODE | Legacy one-time MFA; prompt if required/unset |
| GARMIN_REQUEST_INTERVAL | 2s, positive duration |
| DEEPL_API_KEY | Optional; no key means no outgoing translation calls |
| DEEPL_TARGET_LANG | EN-GB; EN-US also supported |
| DEEPL_REQUEST_INTERVAL | 2s, positive duration |
| DEEPL_API_URL | api-free.deepl.com for :fx key, otherwise api.deepl.com; HTTPS origin |
| DEEPL_GLOSSARY_ID | Optional FR→EN glossary identifier |
| GARMIN_GEOCODING_ENABLED | true; false disables outgoing lookup but retains cache/reconciliation |
| GARMIN_GEOCODING_URL | https://nominatim.openstreetmap.org; compatible reverse-geocoding origin |
| GARMIN_GEOCODING_INTERVAL | 15s; public Nominatim rejects a shorter configured gap |
| GARMIN_GEOCODING_USER_AGENT | Identifying application/repository string; at least 8 characters, no CR/LF |
| LOG_LEVEL | info; debug/info/warn/error/fatal supported |

Provider URLs permit HTTP only for loopback proxies/tests, disallow embedded
credentials/query/fragment/non-root path, and do not follow redirects. No DeepL
subscription upgrade or billing automation exists. Keep one active provider worker
deployment by default; geocoding has SQL-wide pacing but translation does not have
an account-wide distributed limiter. For the public geocoder, show its returned
attribution and consult provider policy when deploying additional instances.

Default token caches are under the operating system's user configuration directory:
`go-garmindives/personal-tokens.json` for an API-key-selected run, otherwise
`go-garmindives/tokens.json`. An explicitly selected unexpired Personal token cache
can be resumed without its key until renewal is required. A renewal failure does
not turn a Personal cache into legacy authentication. Token-cache files use private
permissions. The API key, Garmin passwords/MFA, and DeepL key are not persisted
by these application settings/token caches or printed intentionally in logs.

## Media support

| Extensions (case insensitive) | Kind |
| --- | --- |
| jpg, jpeg, jps, png, gif, webp, heic, heif, avif, tif, tiff, bmp | photo |
| mp4, m4v, mov, mkv, webm, avi, mts, m2ts, 3gp, insv, 360 | video |

Install FFmpeg and FFprobe in the serve container for inspection/previews, with
codecs for intended formats and v360 for perspective previews. Processing targets
Linux/macOS. Unsupported tools/codecs produce thumbnail_error while retaining
accepted originals. Camera-specific formats may require an already stitched export
for browser playback; storing a file does not add a browser codec or stitcher.

API uploads default to 16 GiB per file, plus a bounded multipart overhead allowance.
Upload/asset handlers extend the read deadline to one hour and clear the normal
30-second write timeout. Configure proxy body/time limits consistently; the proxy
must not reject a permitted large file first. The general server header/read/write/
idle timeouts are 5/15/30/60 seconds. Media processing can take up to two minutes
for inspection plus two for thumbnail generation per changed file.

Copy filesystem imports using a hidden temporary name, then rename into the owner
directory when complete. Scan explicitly after copying during runtime; there is no
watcher. Use force=true when content changed with unchanged size/mtime. Missing
originals prune metadata and covers. Restoring SQL before its matching media files
and immediately starting serve can therefore prune restored attachment metadata.

## Observability and recovery

| Signal | Interpretation / action |
| --- | --- |
| setup/status configured=false | Complete first setup |
| configured=true, ready=false | Startup or outage; inspect state/logs, keep setup closed |
| /healthz 200 | Listener/process is alive |
| /readyz 200 | Runtime activated and live SQL ping succeeds |
| media/status | Indexed photo/video/thumbnail counts and upload limit |
| countries/status | Active-position queue progress, provider state/attribution |
| translations/status | Locale, active pending/ready/failed, cached revisions, blocked reason |
| Startup scan log | Per-file action plus added/updated/unchanged/removed/skipped/failed totals |
| Synchronization log | SQL output type/destination, progress and imported/backfilled count |

Retry country failures/not_found through admin POST `/api/v1/countries/retry`;
disabled outgoing lookup returns its disabled state instead of generating calls.
Retry translations through POST `/api/v1/translations/retry`; no key returns
503 translation_disabled. Fix provider credentials/quota/config first, then retry
or restart as appropriate. Status codes are sanitized; provider response bodies
and secret headers are not an operational log interface.

For forgotten administrator password, run interactively inside the serve image
with matching config/SQL access:

```sh
go-garmindives admin reset-password -config /config/config.yaml
```

It securely asks for the new password twice and revokes all sessions. It does not
require the old password. If YAML is lost, the documented setup adoption flow
requires the existing SQL administrator password and retains history/identity.

## Backup, restore, and upgrades

Back up SQL, the YAML identity/settings, externally managed configuration/secrets,
and the original assets together. Keep Garmin token-cache files if convenient;
keys can issue fresh tokens. Thumbnails are rebuildable; original media, descriptions,
trip edits, residence, FIT bytes, and paid translation results are durable data.
Use database-consistent backups (PostgreSQL tooling or a consistent SQLite backup/
stopped-writer copy). For a simple coordinated backup/restore, stop serve and sync,
copy consistent SQL plus assets/config, and restore all before restarting scans.

Before upgrade, retain a recoverable backup. The opener applies migrations and
backfills; it rejects databases from a newer schema. Do not run older/newer backend
versions concurrently through an upgrade. Stage a new database URI only after
planning data migration; PATCH config is not a migration/copy tool. Restart both
roles against the migrated data with the preserved initialization identity.
There is no built-in backup scheduler, rollback-migration command, database export
API, or automatic cross-dialect migration utility.

For development validation, use `go test ./...`, `go test -race ./...`, and
`go vet ./...`; OpenAPI is checked by openapi-spec-validator 0.7.2. CI additionally
provides disposable PostgreSQL integration. Live Garmin tests are opt-in and use
temporary storage; ordinary tests mock external providers. See acceptance scenarios.
