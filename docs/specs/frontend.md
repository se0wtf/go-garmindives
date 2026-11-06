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
All `/api/v1/admin/` routes require login too, including GET/HEAD review lists.
There is no username field or role selector. Preserve browser Origin/fetch metadata
through the proxy. A `403 csrf_rejected` is an origin/deployment problem, not an
incorrect password. Login attempts can return 429 with `Retry-After` seconds.

## Screens and data dependencies

These are functional responsibilities, not a prescribed visual design.

| Surface | Data and actions |
| --- | --- |
| Dive list | GET dives with filters, summary metrics, localized country, rating and cover thumbnail; cursor-based continuation |
| Dive detail | GET one dive, separate paginated samples for chart, raw notes plus parsed comment/highlights, media; link to trip |
| Trip list | GET trips filtered by bilingual name, year, country, overlapping dates, rating selections |
| Trip detail | Editorial content, cruise/operator/price, cover, linked dives, own gallery, favorite dive gallery |
| Media viewer/editor | Original URL, thumbnail, caption/description, resolved projection; favorite for dive media; cover selection on owner |
| Residence settings | List/create/edit/delete inclusive home-country periods |
| Application settings | Authenticated current/pending DB settings, redacted URI, ENV overrides, restart-required state |
| Maintenance | Media status/rescan, country and translation progress/retry, trip rebuild |
| Country correction (admin) | Paginated missing-country dives, lookup reason/GPS, manual country selection from the full catalog |

Use `GET /api/v1/dives/years` for available dive years and
`GET /api/v1/dives/countries` for available dive countries. These complete choice
lists are independent of the current dive page. No public endpoint triggers
Garmin sync or edits imported dive notes/rating. Do not implement these controls
as if such routes existed.

## Filtering, pagination, and charts

Dive filters: `year`, `country`, `trip_id`, `from`, `to`, `location`, `ratings`. Country
accepts code/French/English name; codes are preferable for query state. Location
is a case-insensitive substring, not the same accent-folded matching used for
country/trip names. Combine filters with AND. Trip date filters match overlap;
dive date filters match individual local dates. Unrated trips do not match
rating selections. Zero ratings must remain visible.

Populate the year dropdown from **`GET /api/v1/dives/years`**, for example
`{"items":[2026,2025,2023]}`. Items are numeric years, unique and newest first,
derived from dive local dates across the whole logbook. Missing dates are omitted;
an empty logbook returns `{"items":[]}`. This endpoint has no pagination or
filters and remains independent of active year/country/rating/date filters.
Do not infer the available years from one page of dives or generate a continuous
range that includes years without dives. Use the selected value with
`GET /api/v1/dives?year=2025`, combine with other filters, and reset the dive
cursor when changing years. Include an “All years” control by omitting `year`,
not by sending zero. Refetch year choices after a Garmin import/refresh or when
reloading the logbook; they reflect stored data immediately without a restart.
The endpoint is public after setup, with the normal 503 gate beforehand.

Use `GET /api/v1/dives?ratings=4,5` for a selection of exact ratings (4 OR 5).
This also works on `/api/v1/trips/{tripID}/dives`. A multi-select can send a
comma-separated list, including decimal scores with a dot, such as `ratings=4,4.5,5`.
Omit the parameter when no rating is selected; `ratings=` is invalid. Unrated
dives are excluded while filtering; zero is a valid selection. Compare the raw
`rating`, without converting `rating_max`: `N:8/10` matches 8, not 4. Duplicates
are ignored, and repeated parameters are accepted. At most 100 supplied values
are allowed; malformed, empty, negative, nonfinite or excessive values return
400 `invalid_request`. Rating selections combine with other filters using AND.
The order stays newest first. Retain the selection for cursor requests and reset
pagination when it changes.

Trip lists support the same selection: **`GET /api/v1/trips?ratings=4,5`**.
Use the same multi-select serialization, validation, duplicate handling and
100-value limit as dives. `ratings=4,5` selects exact 4 or 5 scores; include 4.5
explicitly when wanted. This matches the trip's own nullable 0–5 rating, independent
of linked dive scores. Omit the parameter for all rated and unrated trips.
Use `ratings` for every rating selection; `min_rating` and `max_rating` have been
removed and return 400 `invalid_request`, including empty values or combinations
with `ratings`. All name/year/country/date filters intersect the selection,
and the order remains descending trip start date/ID. Keep every
filter unchanged on subsequent cursor pages; reset the cursor on selection changes.

Populate the dive-country dropdown from **`GET /api/v1/dives/countries`**, which
returns `{items: [{code, name_fr, name_en}]}` containing only countries present in
stored dives. It uses each dive's effective country (manual override first),
deduplicates countries, omits unknowns, and sorts by code. It covers the entire
logbook rather than the current page/year/date filter. Optional `q` performs a
case/accent-insensitive substring search. Empty logbooks return `items: []`.
Refetch choices after country edits or background detection updates. Use the
selected code with the existing `/api/v1/dives?country=...` filter; historical
stored aliases match their canonical code too.

Home-country forms continue using **`GET /api/v1/countries`** for all worldwide
choices, or `/api/v1/setup/countries` during first setup. Do not use the reduced
dive-country choices for residence selection.

Collections use `{items: [], next_cursor?: "..."}`. Preserve the cursor exactly;
keep filters unchanged between pages and reset pagination when a filter changes.
No `total`, arbitrary offset/page-number, or snapshot-isolation guarantee exists.
Dives/trips/media normally default to 50 with maximum 100; sample pages default
to 5000 with maximum 10000. The complete API map identifies each route's parameters.

Dives sort newest first by local date, start time, and activity ID. Samples sort
by ascending index. Use `elapsed_ms / 1000` for chart time and metres for depth;
depth normally increases downward visually. Missing samples/measurements are gaps,
not zeroes. Equal timestamps can occur; do not deduplicate by timestamp. Indexed
samples may require multiple pages. Dive JSON never includes a fit property,
the full decoded FIT document, or original_base64, even when logged in as admin.
Remove any raw-FIT JSON disclosure and do not request include=fit. Public chart
measurements remain available through the samples endpoint.
Sample items contain only the index, timestamps and extracted measurements;
the raw property is omitted entirely, including for admins and include_raw=true.
Read depth/temperature/heart-rate/decompression fields directly. Every sample
and its existing index/order is retained; follow next_cursor to load the profile.
Dive detail includes all of its own attachments. Trip detail includes all own
attachments and the first 100 favorite dive attachments; `favorites_next_cursor`
continues that favorite collection through `/api/v1/media/favorites?trip_id=...`.
The dedicated owner-media collection endpoints support pagination when preferred.

For an administrator-only “Download FIT” button, use
**`GET /api/v1/admin/dives/{id}/fit`** with the session cookie. The response is an
attachment containing original binary FIT bytes, or a ZIP if several files are
stored. Use the filename from Content-Disposition; a same-origin download link
can let the browser handle it. If using fetch, read a Blob rather than JSON.
Anonymous/expired sessions return 401; a dive without a stored original returns
404 fit_unavailable. Keep this control in the authenticated admin interface.
The route also protects HEAD/range requests and uses private, no-store caching.
It reads cached SQL files and never triggers a Garmin download.

## Admin country-correction workflow

For dives whose lookups returned no country, fetch
`GET /api/v1/admin/dives/missing-country?lookup_status=not_found` with the session
cookie. Anonymous requests receive 401. The response is `{items, next_cursor?}`;
each item has `dive` (the regular summary, including ID/name/date/entry and exit
GPS) and `lookup` (status, cached latitude/longitude, optional error_code, attempts,
and nullable checked_at_ms). Use dive.id for links and corrections.

The default `lookup_status=all` includes every dive still lacking a country. A
status control can also select `not_found`, `error`, `pending`, or
`missing_coordinates`. Pending includes active work/retries; avoid presenting it
as a completed failure. One shared lookup can affect multiple dives, so this
list is per dive while the status endpoint counts unique positions. Missing
lookup coordinates and check timestamps are null; original GPS is in dive.

Offer the **worldwide** `/api/v1/countries` catalog for manual country selection,
then PATCH `/api/v1/dives/{id}` with `{"country_code":"EG"}`. On success, remove
that row/refetch the queue and invalidate dive/trip/filter-country caches.
Automatic trips are recalculated when residence/date data permits. Other dives
sharing that failed lookup remain in the queue until separately corrected.
Sending country_code null restores automatic detection and can return the dive
to the list. The list is read-only and does not retry provider calls; the existing
retry endpoint retries failed/unresolved positions globally, not one selected dive.

Pages default to 50, maximum 100, ordered newest first by the ordinary dive cursor.
Keep lookup_status unchanged when continuing. This queue is live rather than a
snapshot; refetch from the beginning after correcting rows. There is no new bulk
edit endpoint or end-user frontend implementation in this backend repository.

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
Photo upload and scan can replace the source with JXL and change its filename
and URL (for example `reef.jpg.jxl`), while keeping ID, description, favorites and
cover selections. Use the returned metadata after processing. Compatible H.264/AAC MP4 videos
within 1080p bounds and photos below both size thresholds keep their original bytes.
Other videos can change filename/URL to `.mp4`. The [video profile](video-processing.md)
describes orientation, 360, audio and HDR behavior; no frontend conversion is needed.
Pending items include `kind` for choosing a photo or video review UI. Edited videos
with no recording date remain available for manual assignment.

Photo thumbnails are JXL with a target 480-pixel shorter side; video thumbnails
are JPEG up to 640x640. The returned URLs default to automatic negotiation:
explicit `Accept: image/jxl` selects JXL, otherwise a cached JPEG fallback is served.
No frontend conversion or capability check is required. `content_type` describes
the retained file; use the HTTP response Content-Type when downloading/rendering
fetched bytes. Auto responses vary by Accept and use separate ETags per format.
If using `<picture>`, set a `source type="image/jxl"` URL with `format=original`
and an `img` fallback URL with `format=jpeg`. Add query parameters with the URL
API so existing thumbnail `v` parameters survive. Explicit `format=original`
also provides a download of the retained file (the pre-conversion source is deleted
after successful processing). These rules apply to dive/trip media and cover URLs.

An earlier HEIC tile conversion can report `thumbnail_status: "error"` with a
`thumbnail_error` asking for the original HEIC. Show that explanation to the admin.
Re-uploading the same original filename repairs this specific case while retaining
the existing media ID, description, favorite and cover. Upsert that returned ID
instead of appending a duplicate gallery item. Normal duplicate uploads still
return 409 after successful repair. An original copied directly into the owner
directory can instead be repaired by an admin rescan. Missing pixels cannot be
recovered from a previously truncated JXL alone.

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

The [pending inbox guide](pending-media.md#apis-and-frontend-behavior) defines admin
batch import, matching reasons, private previews, polling and capture metadata.
Floating camera timestamps must not be interpreted in the browser timezone.

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
