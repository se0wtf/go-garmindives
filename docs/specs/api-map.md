# HTTP API map

[Specification index](../../SPECS.md) · [Frontend flows](frontend.md) · [OpenAPI contract](../openapi.yaml)

This inventory is extracted from the schema-11 baseline OpenAPI document. Use the
linked contract for exact request/response schemas, required fields, nullable
properties, error codes, and examples. Domain behavior is specified in the prose
guides; the interactive Swagger UI is available at `/swagger/` (also `/docs`).

Public below means available without an administrator session **after setup**,
unless the route is explicitly a bootstrap/liveness route. All mutations are
administrator-only except initial setup and login. Auth/session reads anonymous
state; GET config and all `/api/v1/admin/` routes are administrator-only. HEAD follows GET routing, and common
OPTIONS returns 204. Cookie auth and CSRF apply as described in
[configuration/security](configuration-security.md).

| Method | Path | Access | Purpose | Request body | Query parameters | Response statuses |
| --- | --- | --- | --- | --- | --- | --- |
| GET | `/api/v1/health` | Public | Check database connectivity | — | — | 200, 500, 503 |
| GET | `/api/v1/dives` | Public | List dives, newest first | — | `limit`, `cursor`, `from`, `to`, `location`, `year`, `country`, `trip_id` | 200, 400, 500, 503 |
| GET | `/api/v1/dives/countries` | Public | List effective countries present in stored dives for the filter dropdown | — | `q` | 200, 500, 503 |
| GET | `/api/v1/dives/{id}` | Public | Read a dive with source documents, decoded FIT data, and media | — | — | 200, 400, 404, 500, 503 |
| PATCH | `/api/v1/dives/{id}` | Admin | Update cover, country override, or trip membership | `application/json` / DivePatch | — | 200, 400, 404, 500, 503, 401, 403 |
| GET | `/api/v1/dives/{id}/samples` | Public | Read dive profile samples in sample-index order | — | `limit`, `cursor` | 200, 400, 404, 500, 503 |
| GET | `/api/v1/dives/{id}/media` | Public | List attachments, sorted by original filename | — | `limit`, `cursor` | 200, 400, 404, 500, 503 |
| POST | `/api/v1/dives/{id}/media` | Admin | Upload one original photo or video | `multipart/form-data` / object | — | 201, 400, 404, 409, 413, 415, 500, 503, 401, 403 |
| GET | `/api/v1/dives/{id}/media/{mediaID}` | Public | Read attachment metadata and URLs | — | — | 200, 400, 404, 500, 503 |
| PATCH | `/api/v1/dives/{id}/media/{mediaID}` | Admin | Update media description, caption, favorite, or projection | `application/json` / MediaPatch | — | 200, 400, 404, 409, 500, 503, 401, 403 |
| DELETE | `/api/v1/dives/{id}/media/{mediaID}` | Admin | Delete an attachment and its files | — | — | 204, 400, 404, 500, 503, 401, 403 |
| GET | `/api/v1/dives/{id}/media/{mediaID}/thumbnail` | Public | Serve the JPEG thumbnail, up to 640 pixels per dimension | — | `v` | 200, 206, 304, 400, 404, 412, 416, 500, 503 |
| HEAD | `/api/v1/dives/{id}/media/{mediaID}/thumbnail` | Public | Read thumbnail headers without downloading it | — | `v` | 200, 206, 304, 400, 404, 412, 416, 500, 503 |
| GET | `/assets/{id}/{filename}` | Public | Serve an original photo or video | — | — | 200, 206, 304, 400, 404, 409, 412, 416, 500, 503 |
| HEAD | `/assets/{id}/{filename}` | Public | Read original-file headers without downloading it | — | — | 200, 206, 304, 400, 404, 409, 412, 416, 500, 503 |
| POST | `/api/v1/media/scan` | Admin | Reconcile media metadata and thumbnails with the assets directory | — | `force` | 200, 400, 500, 503, 401, 403 |
| GET | `/api/v1/media/status` | Public | Read indexed media totals and configured upload limits | — | — | 200, 500, 503 |
| GET | `/api/v1/translations/status` | Public | Read English translation progress and configuration | — | — | 200, 500, 503 |
| POST | `/api/v1/translations/retry` | Admin | Retry failed English translations and resume a blocked worker | — | — | 202, 503, 500, 401, 403 |
| GET | `/api/v1/countries` | Public | List the worldwide country catalog for home-country selection | — | `q` | 200, 400, 404, 500, 503 |
| GET | `/api/v1/countries/status` | Public | Read cached country lookup progress | — | — | 200, 400, 404, 500, 503 |
| GET | `/api/v1/admin/dives/missing-country` | Admin | Review dives without a country and their lookup details for manual correction | — | `lookup_status`, `limit`, `cursor` | 200, 400, 401, 500, 503 |
| POST | `/api/v1/countries/retry` | Admin | Retry failed or unresolved country lookups | — | — | 202, 400, 404, 500, 503, 401, 403 |
| GET | `/api/v1/home-countries` | Public | List dated home-country periods | — | — | 200, 400, 404, 500, 503 |
| POST | `/api/v1/home-countries` | Admin | Create a home-country period | `application/json` / HomeCountryCreate | — | 201, 400, 404, 500, 409, 503, 401, 403 |
| GET | `/api/v1/home-countries/{homeID}` | Public | Read a home-country period | — | — | 200, 400, 404, 500, 503 |
| PATCH | `/api/v1/home-countries/{homeID}` | Admin | Update a home-country period | `application/json` / HomeCountryPatch | — | 200, 400, 404, 500, 409, 503, 401, 403 |
| DELETE | `/api/v1/home-countries/{homeID}` | Admin | Delete a home-country period | — | — | 204, 400, 404, 500, 503, 401, 403 |
| GET | `/api/v1/trips` | Public | List and filter dive trips | — | `limit`, `cursor`, `year`, `country`, `name`, `from`, `to`, `min_rating`, `max_rating` | 200, 400, 404, 500, 503 |
| POST | `/api/v1/trips` | Admin | Create a trip | `application/json` / TripCreate | — | 201, 400, 404, 500, 503, 401, 403 |
| POST | `/api/v1/trips/rebuild` | Admin | Recalculate automatic trip assignments | — | — | 200, 400, 404, 500, 503, 401, 403 |
| GET | `/api/v1/trips/{tripID}` | Public | Read trip metadata, dive IDs, own media, and favorite dive media | — | — | 200, 400, 404, 500, 503 |
| PATCH | `/api/v1/trips/{tripID}` | Admin | Update trip metadata or replace dive membership | `application/json` / TripPatch | — | 200, 400, 404, 500, 503, 401, 403 |
| DELETE | `/api/v1/trips/{tripID}` | Admin | Delete a trip while retaining dives and original media | — | — | 204, 400, 404, 500, 503, 401, 403 |
| GET | `/api/v1/trips/{tripID}/dives` | Public | List linked dives using normal dive filters and pagination | — | `limit`, `cursor`, `from`, `to`, `location`, `year`, `country` | 200, 400, 404, 500, 503 |
| PUT | `/api/v1/trips/{tripID}/dives/{id}` | Admin | Manually assign a dive to this trip | — | — | 204, 400, 404, 500, 503, 401, 403 |
| DELETE | `/api/v1/trips/{tripID}/dives/{id}` | Admin | Remove a dive and persist its automatic-trip exclusion | — | — | 204, 400, 404, 500, 503, 401, 403 |
| GET | `/api/v1/media/favorites` | Public | List favorite photos/videos across dives | — | `limit`, `cursor`, `trip_id` | 200, 400, 404, 500, 503 |
| GET | `/api/v1/trips/{tripID}/media` | Public | List attachments, sorted by original filename | — | `limit`, `cursor` | 200, 400, 404, 500, 503 |
| POST | `/api/v1/trips/{tripID}/media` | Admin | Upload one original photo or video | `multipart/form-data` / object | — | 201, 400, 404, 409, 413, 415, 500, 503, 401, 403 |
| GET | `/api/v1/trips/{tripID}/media/{mediaID}` | Public | Read attachment metadata and URLs | — | — | 200, 400, 404, 500, 503 |
| PATCH | `/api/v1/trips/{tripID}/media/{mediaID}` | Admin | Update description, caption, or 360-degree projection settings | `application/json` / MediaPatch | — | 200, 400, 404, 409, 500, 503, 401, 403 |
| DELETE | `/api/v1/trips/{tripID}/media/{mediaID}` | Admin | Delete an attachment and its files | — | — | 204, 400, 404, 500, 503, 401, 403 |
| GET | `/api/v1/trips/{tripID}/media/{mediaID}/thumbnail` | Public | Serve the JPEG thumbnail, up to 640 pixels per dimension | — | `v` | 200, 206, 304, 400, 404, 412, 416, 500, 503 |
| HEAD | `/api/v1/trips/{tripID}/media/{mediaID}/thumbnail` | Public | Read thumbnail headers without downloading it | — | `v` | 200, 206, 304, 400, 404, 412, 416, 500, 503 |
| GET | `/assets/trips/{tripID}/{filename}` | Public | Serve an original photo or video | — | — | 200, 206, 304, 400, 404, 409, 412, 416, 500, 503 |
| HEAD | `/assets/trips/{tripID}/{filename}` | Public | Read original-file headers without downloading it | — | — | 200, 206, 304, 400, 404, 409, 412, 416, 500, 503 |
| GET | `/api/v1/setup/status` | Bootstrap | Read persistent configuration state and runtime readiness | — | — | 200 |
| GET | `/api/v1/setup/countries` | Bootstrap | List country choices before SQL initialization | — | — | 200 |
| POST | `/api/v1/setup` | Bootstrap | Configure SQL, administrator password, and residence history | `application/json` / SetupRequest | — | 201, 400, 503, 403, 429, 409, 401, 500, 415 |
| GET | `/api/v1/auth/session` | Public | Read current login state | — | — | 200, 503 |
| POST | `/api/v1/auth/login` | Public | Log in and set an administrator session cookie | `application/json` / LoginRequest | — | 200, 400, 503, 403, 429, 401, 415 |
| POST | `/api/v1/auth/logout` | Admin | Revoke the current session and clear its cookie | — | — | 204, 400, 503, 401, 403 |
| POST | `/api/v1/auth/password` | Admin | Change password and revoke all sessions | `application/json` / ChangePasswordRequest | — | 200, 400, 503, 401, 403, 429, 409, 415 |
| GET | `/api/v1/config` | Admin | Read effective and pending settings without connection secrets | — | — | 200, 400, 503, 401 |
| PATCH | `/api/v1/config` | Admin | Save SQL settings for the next restart | `application/json` / ConfigurationPatch | — | 200, 400, 503, 401, 403, 415 |
| GET | `/healthz` | Bootstrap | Check process liveness | — | — | 200 |
| GET | `/readyz` | Public readiness | Check whether normal configured APIs are ready | — | — | 200, 503 |


## Representation rules

- API prefix: `/api/v1`; original media paths are under `/assets`. Returned URLs are origin-relative.
- JSON uses snake_case. Missing optional numeric measurements stay absent where the schema permits; nullable editorial fields such as ratings/covers/year are explicitly null. Empty collections are arrays, not null.
- Error envelope: `{ "error": { "code": "...", "message": "..." } }`. Standard router errors and HTTP asset precondition/range responses can differ.
- Pagination envelopes have `items` and optional `next_cursor`; no total count or numeric page offset.
- Cursor wire format is unpadded base64url of compact JSON. Clients must treat it as opaque; backend ports preserving existing cursors should retain the payload shapes below.

| Collection | Cursor payload | Order / next-page condition |
| --- | --- | --- |
| Dives | `{ "date": "YYYY-MM-DD", "time": "...", "id": 123 }` | local_date DESC, start_time DESC, id DESC; lexicographically smaller tuple |
| Samples | `{ "index": 42 }` | sample_index ASC; index strictly greater |
| Owner media / favorites | `{ "filename": "...", "id": "..." }` | filename ASC, id ASC; tuple strictly greater |
| Trips | `{ "start_date": "YYYY-MM-DD", "id": "..." }` | start_date DESC, id DESC; tuple strictly smaller |

Cursors reject invalid encoding, unknown fields, trailing JSON, and route-specific invalid values. Return at most limit items, fetch limit+1 to detect continuation. Matching file/identifier collation matters when porting.

## Documentation and probes

| Path | Behavior |
| --- | --- |
| `/swagger/` | Bundled interactive UI, no CDN required |
| `/docs` | Documentation entry/redirect |
| `/api/v1/openapi.json` | Embedded JSON derived from YAML |
| `/api/v1/openapi.yaml` | Embedded editable-contract YAML |
| `/healthz` | Process alive, even before setup |
| `/readyz` | Normal runtime and live SQL ping ready; 503 otherwise |
| `/api/v1/health` | Configured-domain database health; follows initialization gate |

## Schema navigation

The OpenAPI components include the complete DiveSummary/DiveDetail/DiveSample,
DiveTrip/TripDetail, Media, HomeCountry, Country, translation/status, setup, auth,
and configuration representations. Do not send response objects back as PATCH
bodies: use DivePatch, TripPatch, MediaPatch, HomeCountry request schemas and the
dedicated SetupRequest/LoginRequest/ChangePasswordRequest/ConfigurationPatch shapes.
The following sections in the prose explain cross-field semantics the schemas
alone cannot express:

- [Covers, membership and null handling](frontend.md#editing-semantics).
- [Trip grouping and residence](product.md#dive-trips).
- [Media import and serving](backend.md#filesystem-media-lifecycle).
- [Security and setup](configuration-security.md).

## Not exposed by HTTP

Garmin synchronization and single-dive refresh are CLI commands. There is no
public endpoint to delete a dive, change its imported notes/rating/name/date/GPS,
download a dedicated FIT binary, move media between owners, undelete a trip,
enumerate available years, or trigger server restarts. Any new frontend control
requiring these behaviors needs an explicit backend feature, not a guessed route.
