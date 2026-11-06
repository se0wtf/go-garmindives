# Runtime settings reference

[Specification index](../../SPECS.md) · [Configuration and security](configuration-security.md) · [Example YAML](../../config.example.yaml)

All 28 operator settings support **flags > ENV > YAML > built-in defaults**.
Every application ENV name starts with `DIVELOG_`. Provider settings retain the
provider name, for example `DIVELOG_GARMIN_REQUEST_INTERVAL` and
`DIVELOG_DEEPL_API_KEY`; application features use names such as
`DIVELOG_GEOCODING_ENABLED` and `DIVELOG_LOG_LEVEL`.

Deployments using the previous `GARMIN_*`, `DEEPL_*` or `LOG_LEVEL` names must
update their environment. Those names are no longer read. YAML keys, flags,
defaults and precedence are unchanged. The `DIVELOG_OUTPUT_TYPE` /
`DIVELOG_OUTPUT_DSN` SQL aliases remain available alongside `DIVELOG_DB_TYPE` /
`DIVELOG_DB_URI`.
The same common flags are accepted by `sync`, `serve`, and `admin reset-password`.
Each role consumes only the settings it needs: sync uses SQL/Garmin/logging;
serve uses SQL/HTTP/auth/media/Garmin resync/translation/geocoding/logging;
password recovery uses SQL/logging. No provider calls are made just to parse settings.

| YAML key | Flag(s) | ENV (canonical first) | Default |
| --- | --- | --- | --- |
| `database.type` | `-db-type`, `-output-type` | `DIVELOG_DB_TYPE`, `DIVELOG_OUTPUT_TYPE` | `sqlite` |
| `database.uri` | `-db-uri`, `-output-dsn` | `DIVELOG_DB_URI`, `DIVELOG_OUTPUT_DSN` | `dives.db` |
| `http.listen` | `-listen` | `DIVELOG_API_ADDR` | `127.0.0.1:8080` |
| `http.cookie_secure` | `-cookie-secure` | `DIVELOG_COOKIE_SECURE` | `true` |
| `http.public_url` | `-public-url` | `DIVELOG_PUBLIC_URL` | `empty` |
| `auth.session_lifetime` | `-session-lifetime` | `DIVELOG_SESSION_LIFETIME` | `24h` |
| `auth.idle_timeout` | `-session-idle-timeout` | `DIVELOG_SESSION_IDLE_TIMEOUT` | `2h` |
| `media.directory` | `-assets-dir` | `DIVELOG_ASSETS_DIR` | `assets` |
| `media.max_upload_bytes` | `-media-max-upload-bytes` | `DIVELOG_MEDIA_MAX_UPLOAD_BYTES` | `17179869184` |
| `media.pending_directory` | `-pending-dir` | `DIVELOG_PENDING_DIR` | `pending` |
| `media.pending_scan_interval` | `-pending-scan-interval` | `DIVELOG_PENDING_SCAN_INTERVAL` | `60s` |
| `garmin.personal_api_key` | `-garmin-personal-api-key` | `DIVELOG_GARMIN_PERSONAL_API_KEY` | `empty` |
| `garmin.email` | `-garmin-email` | `DIVELOG_GARMIN_EMAIL` | `empty` |
| `garmin.password` | `-garmin-password` | `DIVELOG_GARMIN_PASSWORD` | `empty` |
| `garmin.mfa_code` | `-garmin-mfa-code` | `DIVELOG_GARMIN_MFA_CODE` | `empty` |
| `garmin.token_file` | `-token-file` | `DIVELOG_GARMIN_TOKEN_FILE` | `empty` |
| `garmin.resync_token_file` | `-garmin-resync-token-file` | `DIVELOG_GARMIN_RESYNC_TOKEN_FILE` | `empty` |
| `garmin.request_interval` | `-garmin-request-interval` | `DIVELOG_GARMIN_REQUEST_INTERVAL` | `2s` |
| `translation.api_key` | `-deepl-api-key` | `DIVELOG_DEEPL_API_KEY` | `empty` |
| `translation.url` | `-deepl-api-url` | `DIVELOG_DEEPL_API_URL` | `empty` |
| `translation.target_lang` | `-deepl-target-lang` | `DIVELOG_DEEPL_TARGET_LANG` | `EN-GB` |
| `translation.glossary_id` | `-deepl-glossary-id` | `DIVELOG_DEEPL_GLOSSARY_ID` | `empty` |
| `translation.interval` | `-deepl-request-interval` | `DIVELOG_DEEPL_REQUEST_INTERVAL` | `2s` |
| `geocoding.enabled` | `-geocoding-enabled` | `DIVELOG_GEOCODING_ENABLED` | `true` |
| `geocoding.url` | `-geocoding-url` | `DIVELOG_GEOCODING_URL` | `https://nominatim.openstreetmap.org` |
| `geocoding.interval` | `-geocoding-interval` | `DIVELOG_GEOCODING_INTERVAL` | `15s` |
| `geocoding.user_agent` | `-geocoding-user-agent` | `DIVELOG_GEOCODING_USER_AGENT` | `go-garmindives/1.0 (https://github.com/se0wtf/go-garmindives)` |
| `logging.level` | `-log-level` | `DIVELOG_LOG_LEVEL` | `info` |

## Resolution and validation

- Select the file using `-config PATH` > `DIVELOG_CONFIG` > `config.yaml` in the working directory. This bootstrap selector cannot live inside the file it selects.
- Start with defaults, decode one YAML document with known fields only, apply ENV, then flags. The final effective values are validated before startup. A valid flag can override an invalid ENV value. Malformed YAML, unknown fields and invalid internal state are always errors.
- Canonical DB ENV names win over legacy ENV aliases; the last flag wins when aliases repeat. Source metadata records `flag:-NAME` or the ENV variable name, without its value. YAML/default values have no entry in `overrides`.
- An unset ENV variable leaves the YAML/default value. An explicitly empty ENV or flag value clears optional strings; required values fail validation. Boolean false must be explicit (`-cookie-secure=false`, `-geocoding-enabled=false`); bare boolean flags mean true. Other flags accept `-flag VALUE` or `-flag=VALUE`.
- Durations use Go syntax (`500ms`, `2s`, `24h`; not `1d`). Session lifetime is 1m–720h; idle timeout is at least 1m and no greater than lifetime. Pending scan interval is 1s–24h. Provider intervals must be positive; public Nominatim requires at least 15s. Upload bytes must be positive and at most 2^60.
- Provider origins must have no credentials/query/fragment/non-root path and use HTTPS, with HTTP allowed for loopback test/proxy origins. Translation target is EN-GB or EN-US. Geocoder User-Agent is at least eight characters without CR/LF. Log level is debug/info/warn/error/fatal.
- YAML/ENV relative paths resolve against the YAML directory; `media.pending_directory` instead resolves against the assets root. Relative token-cache flags and sync's explicit relative SQL filename flags keep working-directory semantics. Serve's relative SQL filename flags resolve against the YAML directory. Absolute paths avoid deployment ambiguity.
- Empty `garmin.token_file` selects `<OS user config dir>/go-garmindives/tokens.json`, or `personal-tokens.json` when a Personal API key is present. Empty `garmin.resync_token_file` selects `resync-personal-tokens.json` in that directory. Serve resync requires a Personal API key or an explicitly configured resync cache; it never prompts for legacy credentials. The CLI and serve caches are separate.
- Empty `translation.api_key` disables outgoing translation. Empty `translation.url` selects `https://api-free.deepl.com` for a key ending in `:fx`, otherwise `https://api.deepl.com`.
- Settings are resolved once at command startup. File/ENV changes require restarting that command. Setup and database PATCH retain all original flag overrides when saving/re-resolving the YAML base.

## Credentials and persisted state

Garmin API key, email, password, MFA code and DeepL API key may be explicitly
configured in YAML. The application writes YAML atomically with mode 0600. ENV and
flag values are never copied back to YAML. Explicit YAML credentials survive
setup and database PATCH. Password whitespace is preserved. Credentials are
redacted from flag help and never included in the authenticated configuration API;
the database URI is displayed without credentials/options. Prefer ENV for secrets
when flags would expose them in shell history or process arguments.

`GET /api/v1/config` includes effective nonsecret settings for all groups and
source metadata. `PATCH /api/v1/config` still edits only SQL connection settings;
other settings are changed through file/ENV/flags and require restart.

`version`, `initialized`, and `instance_id` are persisted lifecycle state, managed
by setup/version checks. They are deliberately not flag/ENV overrides. The admin
password and dated home-country periods are SQL application data set through
setup/authenticated APIs, rather than runtime settings. `-activity-id` is a sync
action, not a persistent setting. OS variables (`HOME`, XDG config paths, `PATH`)
and test-only database DSNs are infrastructure inputs, not application settings.
Protocol limits, security policy, and photo/video encoding profiles remain defined
by their respective specifications; this inventory covers the runtime settings.
