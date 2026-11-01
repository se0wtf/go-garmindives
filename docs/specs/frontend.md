# Frontend integration guide

[Specification index](../../SPECS.md) · [Concepts](product.md) · [API map](api-map.md)

## Bootstrap and authentication

On application entry, fetch `/api/v1/setup/status` with same-origin credentials.
This request is available without SQL connectivity. Interpret the booleans first:

| `configured` | `ready` | UI behavior |
| --- | --- | --- |
| false | false | Show initial setup |
| true | false | Show starting/unavailable state and retry status; keep setup closed |
| true | true | Load public content and GET `/api/v1/auth/session` |

Do not redirect every `503` to setup. Only `configured: false` means setup is
needed. `/healthz` is process liveness, not proof that public data is available.

Setup form: SQL type (`sqlite` or `postgres`), URI/path, administrator password,
and dated home-country periods. Country choices come from
`GET /api/v1/setup/countries`, which also works before initialization. Use the
password length constraints from status (currently 15–128 Unicode characters),
and display backend validation messages for common-password/date overlap rules.
End dates use null for “no planned end”. ENV/flag-managed settings are reported
in `overrides`; allow the form to omit those database properties.

Complete request example, using synthetic values:

```json
{
  "database": {"type": "sqlite", "uri": "dives.db"},
  "admin_password": "My unique long diving passphrase!",
  "home_countries": [
    {"country_code": "FR", "from": "2020-01-01", "to": "2025-06-30"},
    {"country_code": "NC", "from": "2025-07-01", "to": null}
  ]
}
```

POST it to `/api/v1/setup` as `application/json`. On 201, poll status until ready,
then offer login. Setup does not log the administrator in. Suggested frontend
polling is every 2–5 seconds with backoff during outages; this is a UI choice,
not an internal backend cron or a mandatory API interval.

Login posts `{"password":"..."}` to `/api/v1/auth/login`. The server sets an
HttpOnly cookie. Use normal same-origin requests; do not put session tokens in
localStorage or attach a Bearer token. `auth/session` returns authenticated false
for expired/absent sessions. Hide editing controls when anonymous, but rely on
server enforcement. On write `401 authentication_required`, retain unsaved form
state and ask for login. Logout is POST. Password change requires current/new
password and logs out every session, including the current one.

All edits, uploads, scans, retries, trip rebuilds, and config reads require login.
There is no username field or role selector. Preserve browser Origin/fetch metadata
through the proxy. A `403 csrf_rejected` is an origin/deployment problem, not an
incorrect password. Login attempts can return 429 with `Retry-After` seconds.

## Screens and data dependencies

These are functional responsibilities, not a prescribed visual design.

| Surface | Data and actions |
| --- | --- |
| Dive list | GET dives with filters, summary metrics, localized country, rating and cover thumbnail; cursor-based continuation |
| Dive detail | GET one dive, separate paginated samples for chart, raw notes plus parsed comment/highlights, media; link to trip |
| Trip list | GET trips filtered by bilingual name, year, country, overlapping dates, rating bounds |
| Trip detail | Editorial content, cruise/operator/price, cover, linked dives, own gallery, favorite dive gallery |
| Media viewer/editor | Original URL, thumbnail, caption/description, resolved projection; favorite for dive media; cover selection on owner |
| Residence settings | List/create/edit/delete inclusive home-country periods |
| Application settings | Authenticated current/pending DB settings, redacted URI, ENV overrides, restart-required state |
| Maintenance | Media status/rescan, country and translation progress/retry, trip rebuild |

No API enumerates available years separately. Use meaningful year controls or
derive available years from fully loaded results; a single list page is not a
complete facet list. No public endpoint triggers Garmin sync or edits imported
dive notes/rating. Do not implement these controls as if such routes existed.

## Filtering, pagination, and charts

Dive filters: `year`, `country`, `trip_id`, `from`, `to`, `location`. Country
accepts code/French/English name; codes are preferable for query state. Location
is a case-insensitive substring, not the same accent-folded matching used for
country/trip names. Combine filters with AND. Trip date filters match overlap;
dive date filters match individual local dates. An unrated trip does not match
a rating-bound filter. Zero ratings must remain visible.

Collections use `{items: [], next_cursor?: "..."}`. Preserve the cursor exactly;
keep filters unchanged between pages and reset pagination when a filter changes.
No `total`, arbitrary offset/page-number, or snapshot-isolation guarantee exists.
Dives/trips/media normally default to 50 with maximum 100; sample pages default
to 5000 with maximum 10000. The complete API map identifies each route's parameters.

Dives sort newest first by local date, start time, and activity ID. Samples sort
by ascending index. Use `elapsed_ms / 1000` for chart time and metres for depth;
depth normally increases downward visually. Missing samples/measurements are gaps,
not zeroes. Equal timestamps can occur; do not deduplicate by timestamp. Indexed
samples are separate from the full FIT document and may require multiple pages.
Dive detail includes all of its own attachments. Trip detail includes all own
attachments and the first 100 favorite dive attachments; `favorites_next_cursor`
continues that favorite collection through `/api/v1/media/favorites?trip_id=...`.
The dedicated owner-media collection endpoints support pagination when preferred.

## Editing semantics

PATCH is field-specific, not generic JSON Merge Patch. Omitted properties keep
their current values. Only send null where explicitly allowed. Text fields are
cleared with `""`; a zero rating and a null rating have different meanings.

```json
{"country_code":"EG"}
```

PATCH `/api/v1/dives/{id}` above overrides detected country. The same property
with null restores automatic detection. Other dive PATCH examples:

| Complete patch | Effect |
| --- | --- |
| `{"cover_media_id":"PHOTO_ID"}` | Select an owned photo |
| `{"cover_media_id":null}` | Clear cover |
| `{"trip_id":"TRIP_ID"}` | Manually assign/move dive |
| `{"trip_id":null}` | Exclude from automatic grouping |
| `{"trip_mode":"auto"}` | Restore automatic grouping; do not combine with `trip_id` |

Trip creation needs `year` and `country_code`. Optional `dive_ids` must contain
unique, existing positive IDs. When editing membership, prefer single-dive
PUT/DELETE for individual moves; replacing `dive_ids` explicitly replaces the
entire set. Explain that removing a dive excludes it from automatic grouping.
Expose a distinct “restore automatic assignment” action.

Trip nullable overrides: `name`, `year`, `country_code`; nullable values also
include `rating` and `cover_media_id`. See product rules for custom `name_en`.
String limits are **UTF-8 bytes**, not character counts: name/name_en and operator
name 1024; subtitle and price 4096; description 32768; operator website 2048;
operator comment 8192. Website must be HTTP(S), have a host, and contain no userinfo.
Media caption limit is 4096 bytes, description 8192. Use `TextEncoder` or equivalent
for byte-length validation. Password limits instead count Unicode code points.

After country/residence/membership edits, invalidate affected dive and trip list
caches as grouping and names may change. After cover, caption, description, or
favorite changes, invalidate owner cards and trip favorite collections as needed.
Return values are authoritative; optimistic changes must be reversible on errors.

## Media and language presentation

Upload one `file` multipart field. In browsers let `FormData` generate Content-Type
and boundary. Upload directly to the owner's media route; 201 returns metadata
and Location. Show progress for large videos; processing is synchronous and can
continue after all bytes have been sent. Duplicate filenames produce 409, not an
overwrite dialog backed by an imaginary replace API. 413 means the file exceeds
the configured upload size. Direct filesystem imports need an explicit scan.

Use server-provided `url` and `thumbnail_url`; do not reconstruct paths from
unescaped filenames. Use thumbnail only when available. A failed thumbnail can
fall back to original for supported photos or a placeholder/download for video.
A video player chooses flat/180/360 based on `effective_projection`, respecting
`projection_format` and optional stereo layout. Do not infer 360 from a filename.
Original URLs support range seeking. No transcoded streaming manifest is supplied.

Cover fields are null when unselected. Clearing a cover is not deleting the photo.
The trip's own cover cannot reference a favorite from a dive; the UI must select
from trip-owned photos. Favorites and trip media remain separate arrays and owners.

For French, use source fields. For English, use a ready `translations.en` field
when present, otherwise source French. Generated trip names use `name_en` directly;
custom names may instead receive `translations.en.name`. Never change the French
edit form's value to a machine translation. `pending`, `error`, and `disabled`
are fallback states, not empty content. Display country names from name_fr/name_en.
Render user/provider prose as text, not trusted HTML; retain comment line breaks.
Show OpenStreetMap attribution when presenting geocoded information.

## Failure behavior

API errors normally have `{"error":{"code":"...","message":"..."}}`.
Branch on code/status, not English message text. Unsupported paths/methods can
produce standard HTTP router responses rather than this JSON envelope.

| Condition | Expected frontend handling |
| --- | --- |
| 400 validation / invalid cursor | Show field/request error; reset a bad pagination cursor |
| 401 | Login state needs attention; invalid_credentials is a password failure |
| 403 csrf_rejected | Report origin/configuration problem |
| 404 | Owner or attachment no longer available |
| 409 filename conflict / media_changed / overlap | Explain conflict; media_changed needs rescan |
| 413 / 415 | Upload too large / unsupported media or JSON Content-Type |
| 429 | Respect Retry-After; avoid a retry loop |
| 503 not_configured | Verify setup/status before redirecting |
| Other 5xx | Keep inputs; show retryable service failure |

GETs can be repeated. Do not blindly retry successful-or-unknown POST uploads,
trip creation, or residence creation after a connection loss: inspect the owner
collection first. The API has no generic idempotency-key support or edit ETag
preconditions. Conditional ETags apply to file reads, not concurrent form edits.
