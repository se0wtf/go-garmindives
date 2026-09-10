# go-garmindives

`go-garmindives` exports every Garmin Connect diving activity to YAML, SQLite,
or PostgreSQL. Each dive contains the activity summary, standard Garmin Connect
details, Garmin Dive-specific details, and timestamped profile samples.

The app is unofficial and is not affiliated with Garmin. Both APIs are
undocumented and may change without notice.

## Install

```sh
go install github.com/se0wtf/go-garmindives@latest
```

## Usage

On the first run, the app prompts for Garmin credentials and MFA when needed:

```sh
go-garmindives
```

Credentials can also be supplied for non-interactive use:

```sh
GARMIN_EMAIL='you@example.com' GARMIN_PASSWORD='...' go-garmindives
```

The password is never stored. API tokens and the authenticated Garmin browser
session needed by the Dive endpoint are cached under the operating system's
user configuration directory with owner-only permissions. Later runs reuse the
cached session and automatically refresh expiring API tokens. If Garmin expires
either part of the session, the app asks for credentials and MFA again.

The default archive is `dives.yaml` in the current directory. Choose other
locations with:

```sh
go-garmindives -output /path/to/dives.yaml -token-file /path/to/tokens.json
```

Select SQLite with environment variables:

```sh
GARMIN_OUTPUT_TYPE=sqlite GARMIN_OUTPUT_DSN=/path/to/dives.db go-garmindives
```

The SQLite database is created with owner-only permissions. If
`GARMIN_OUTPUT_DSN` is omitted, its default path is `dives.db`.

For PostgreSQL, provide a standard connection URL:

```sh
GARMIN_OUTPUT_TYPE=postgres \
GARMIN_OUTPUT_DSN='postgres://user:password@localhost:5432/garmindives?sslmode=require' \
go-garmindives
```

The DSN is never written to application logs. The same settings are available
as `-output-type` and `-output-dsn` flags; flags override environment values.

Existing activity IDs are never fetched again. New records are downloaded
sequentially. YAML is replaced atomically only after every new dive succeeds;
SQL batches are committed in one transaction. YAML, SQLite, and the token file
use mode `0600` where applicable.

## SQL schema and migrations

SQL writes are transactional. The schema is designed for a future frontend:

- `dives` stores queryable summary, location, profile, environment, and heart
  rate fields, plus the three complete source JSON documents so no Garmin data
  is lost.
- `dive_samples` stores one row per timestamp with indexed `dive_id` and
  `timestamp_ms`, elapsed time, depth, temperature, heart rate, decompression,
  gas-consumption, PO2, and ascent-rate columns. It also retains the original
  sample JSON for forward compatibility.
- `dive_record_metadata` preserves Garmin's position-to-metric mapping.
- `dive_alerts` stores timestamped dive-computer alerts.

The app runs forward-only migrations automatically when it opens a SQL
database. Applied versions are recorded in `schema_migrations`, and each new
migration runs in a transaction. A database newer than the app is rejected
instead of being modified. Future schema changes are added as a new numbered
migration; existing migrations are never rewritten.

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

The versioned endpoints are:

- `GET /api/v1/health`
- `GET /api/v1/dives`
- `GET /api/v1/dives/{id}`
- `GET /api/v1/dives/{id}/samples`

Dive lists are newest-first and accept `limit` (default 50, maximum 100), an
opaque `cursor`, inclusive local-date filters `from` and `to` in `YYYY-MM-DD`
format, and a case-insensitive `location` substring. For example:

```text
/api/v1/dives?from=2026-01-01&to=2026-08-31&location=noumea&limit=20
```

When more results exist, use the returned `next_cursor` unchanged in the next
request. Sample lists use the same scheme with a default of 5,000 and maximum
of 10,000 rows per page. The dive detail endpoint includes all three original
Garmin JSON documents, record metadata, and alerts; the samples endpoint
provides graph-ready elapsed time, depth, heart rate, temperature, gas, and
decompression values.

## Development

```sh
go test ./...
go test -race ./...
go vet ./...
```

Tests are offline by default. An opt-in integration test logs into Garmin,
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
authentication implementation is based on research from
[python-garminconnect](https://github.com/cyberjunky/python-garminconnect).
