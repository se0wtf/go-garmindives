# Repository organization

[Specification index](../../SPECS.md) · [Backend behavior](backend.md)

The executable remains at the module root so `go run .`, `go build .`, and
`go install github.com/se0wtf/go-garmindives@latest` retain their command path.
Root `main.go` creates logging/signal handling and calls `internal/app.Run`.

```text
main.go                         executable entry point
config.example.yaml             operator-facing settings example
README.md / SPECS.md             usage and specification entry points
internal/
  app/                          command/runtime composition and integrated persistence
    cli.go, cli_admin.go        sync/serve dispatch and local admin recovery
    application.go              setup gate, activation, readiness, worker lifecycle
    http*.go                    HTTP handlers, authentication, and middleware
    sqlstore*.go                SQL connection, migrations, queries, persistence
    sync*.go, destination.go    import selection and transactional destination
    trips.go, dive_patch.go     trip grouping and local dive metadata
    media*.go, cover.go         filesystem library, ownership and cover selection
    country_worker.go           persistent geocoding work and travel reconciliation
    translation_worker.go       persistent translation work and cache assembly
    *_test.go, testdata/         domain/API/storage integration tests and fixtures
  config/                       YAML/ENV resolution, SQL destinations and validation
  country/                      bilingual catalog and normalized country resolution
  fit/                          FIT containers, decoder and lossless document types
  garminhttp/                    Garmin request pacing/retries and focused tests
  geocoding/                     Nominatim configuration and HTTP adapter
  media/                         FFprobe inspection and FFmpeg thumbnails
  notes/                         pure French note parser
  security/                      administrator password policy and Argon2id
  translation/                   DeepL configuration and HTTP adapter
  env/, httpx/, identity/         shared environment, HTTP and identifier helpers
docs/
  openapi.go, openapi.yaml        embedded HTTP contract and documentation routes
  swagger/                       bundled Swagger UI assets and licenses
  specs/                         product/frontend/backend/operations specifications
```

The application package owns the SQL-backed runtime and coordinates transactions,
workers, handlers, and filesystem operations. Their shared state stays in one
package. Provider clients, parsing, configuration, media processing, and password
hashing have independent package boundaries and do not import `internal/app`.
Shared HTTP response helpers let documentation routes remain independent too.

Integration tests stay alongside the application they exercise. Garmin transport
unit tests live with that transport, keeping its synchronization internals private.
The main fixture is `internal/app/testdata/personal-dive.json`. The OpenAPI route
coverage test reads the handler files from the application directory; update its
file inventory when adding or renaming handler registration files.

Run the complete suite from the root with `go test ./...`, race checks with
`go test -race ./...`, and static checks with `go vet ./...`. Target live/SQL
integration tests at `./internal/app`, and transport tests at
`./internal/garminhttp`. New package imports remain internal to this module;
the public API is the executable plus the documented HTTP contract.
