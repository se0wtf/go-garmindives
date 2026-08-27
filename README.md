# go-garmindives

`go-garmindives` exports every Garmin Connect diving activity to one YAML
archive. Each record contains the activity summary, standard Garmin Connect
details, and Garmin Dive-specific details.

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

Existing activity IDs are never fetched again. New records are downloaded
sequentially and the archive is replaced atomically only after every new dive
succeeds. The archive and token file use mode `0600`.

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

## Credits

Developed with Codex using
[go-garminconnect](https://github.com/se0wtf/go-garminconnect), whose
authentication implementation is based on research from
[python-garminconnect](https://github.com/cyberjunky/python-garminconnect).
