# Backend algorithms and integration contracts

[Specification index](../../SPECS.md) · [Storage](storage.md) · [Acceptance](acceptance.md)

This describes language-independent processing rules. Library names identify the
reference implementation, not a requirement to use Go. External API details below
are the adapter behavior pinned by go-garminconnect v0.2.0; they are not a promise
that Garmin's future upstream protocol will stay unchanged.

## Synchronization

The default command and `sync` perform the same one-shot incremental import.
Require an initialized YAML identity matching SQL before contacting Garmin.
Select Personal API when an API key is supplied; otherwise use a valid token
cache or legacy email/password/MFA authentication. An explicitly rejected Personal
key must not silently switch to password authentication.

List the complete activity history in pages of 100 (`start` offset increases by
returned page length; stop when page is shorter than limit). Filter locally to:
`apnea_diving`, `apnea_hunting`, `ccr_diving`, `gauge_diving`, `multi_gas_diving`,
`resort_diving`, `single_gas_diving`. Match type keys case-insensitively and
deduplicate by activity ID. Listing history is not the same as downloading all
activity documents again.

```text
load persisted IDs and FIT-complete state
list history; choose supported activities
for each unique supported activity, sequentially:
    if stored and FIT complete: skip
    download and decode original FIT
    if stored: stage FIT backfill preserving stored source metadata
    otherwise: fetch source details and stage a new dive
if any required request/decoding fails: do not save the staged batch
otherwise: persist all staged dives/children/FIT and reconcile travel in one transaction
```

FIT complete includes a valid FIT document with no profile records. Do not keep
downloading it just because no depth chart exists. Existing source documents,
local cover, country override, trip mode/link, and attachments survive backfill.
Normal incremental runs do not refresh changed names/notes for complete dives,
delete dives missing from Garmin, or scan assets.

`sync -activity-id ID` fetches exactly one activity without listing history. Verify
returned activity ID and supported dive type. With the Personal API, its dive
response is already the full activity document, so reuse it. For legacy API,
fetch the activity separately and graph details only for a new local record.
Refresh source metadata and parsed notes; reuse FIT/samples when already complete,
download FIT only when missing. Commit one-dive changes transactionally and retain
all local editorial metadata. A previously absent dive is imported normally.

### Garmin Personal adapter

Origin: `https://personal.connectapi.garmin.com`. Exchange the key with an empty
POST to `/personalapitoken-service/personal-api/personal-api-key/token`, header
`Authorization: ApiKey <key>`. Response is a plain-text JWT with a future `exp`.
Subsequent requests use `Authorization: Bearer <token>`.

| Operation | Upstream path |
| --- | --- |
| Activity pages | GET `/activitylist-service/activities/search/activities?start=0&limit=100` |
| Full activity / dive metadata | GET `/activity-service/activity/{id}` |
| Original download | GET `/download-service/files/activity/{id}` |

No Personal graph-details request is made; store `activity_details: null` and use
FIT for profiles. The activity-list document is the adapter's modeled subset;
the full Personal activity document is retained in `dive_details`. Keep unknown
properties in the full source document. The key stays in memory, never in the
token cache. Token expiry/renewal is serialized. Refresh 15 minutes before expiry,
or halfway through remaining lifetime for a newly issued shorter-lived token.
A rejected bearer token can renew once and retry once. Preserve transport pacing
for exchanges, renewals, pagination, JSON details, and FIT downloads alike.

Legacy Garmin Connect/Dive authentication is encapsulated by the pinned adapter:
cached mobile OAuth plus browser session/CSRF for Dive details, with interactive
MFA fallback. A port claiming legacy support must replace that entire adapter,
including session renewal, cookie persistence, and its tests; using only the
Personal HTTP paths does not implement the legacy mode. The default, complete
API-key workflow needs no legacy password. See [porting boundaries](storage.md#porting-and-compatibility)
for dependency specifications a reimplementation must supply.

### Garmin pacing and retries

One shared transport per sync invocation serializes callers through response
headers, then schedules the next request at least 2 seconds later by default.
The first request may run immediately; no accumulated burst allowance exists.
`GARMIN_REQUEST_INTERVAL` must be positive. Limiting is per process, not SQL-wide;
deployment must prevent overlapping sync invocations.

Each network attempt has a 30-second timeout, including reading its body. Pacing
and Retry-After sleeps occur outside that timeout and obey cancellation. Only
HTTP 429 triggers the transport retry policy: at most three retries (four total
attempts), delays from Retry-After seconds/HTTP date or 30/60/120 seconds fallback,
always at least the configured request interval. Even the final 429 leaves the
transport cooldown in effect. Replay only bodyless GET/HEAD or the exact empty
Personal token-exchange POST. Never automatically replay login/MFA bodies.

## Source-document mapping

Persist `activity` (activity-list modeled summary, or full activity on targeted
refresh), `activity_details` (legacy graph document or null), and `dive_details`
(legacy Dive document or full Personal activity). Choose Personal shape when
`dive_details.activityId > 0`. The table gives queryable summary mapping before
FIT fallback. `A` means the activity document and `D` the dive_details document;
first nonempty string wins where a fallback is shown.

| Stored/API summary | Personal D | Legacy D, with A fallback where stated |
| --- | --- | --- |
| id | Selected activity ID | Selected activity ID |
| garmin_dive_id / dive_number | No separate ID/number unless provided by legacy-shaped fields or FIT fallback | id / number |
| name | activityName, else A.activityName | name, else A.activityName |
| activity_type | activityTypeDTO.typeKey, else A.activityType.typeKey | A.activityType.typeKey |
| dive_type | Empty unless legacy-shaped diveType exists | diveType |
| start_time | summaryDTO.startTimeLocal, else A.startTimeLocal | startTime, else A.startTimeLocal |
| timezone | timeZoneUnitDTO.timeZone, else unitKey | timezone |
| location | locationName | location.location |
| entry coordinates | summaryDTO.startLatitude / startLongitude | location.entryLoc.latitude / longitude |
| exit coordinates | summaryDTO.endLatitude / endLongitude | location.exitLoc.latitude / longitude |
| notes / description | legacy-shaped notes if any / description, else A.description | notes / A.description |
| duration_seconds | summaryDTO.duration | first nonzero totalTime, A.duration |
| max_depth_meters / average_depth_meters | summaryDTO.maxDepth / averageDepth | profile.maxDepth / averageDepth |
| average_heart_rate / max_heart_rate | summaryDTO.averageHR / maxHR | performance.avgHr / maxHr, else A.averageHR / maxHR |
| min/average/max_temperature_celsius | summaryDTO.minTemperature / averageTemperature / maxTemperature | environment.minTemperature / avgTemperature / maxTemperature |

Personal numerical fields are nullable pointers, retaining explicit zero. Legacy
summary fields are scalar: zero maps to missing, including coordinate components.
Missing/invalid calendar starts give local_date empty and API year null. Keep all
unmapped equipment, gases, settings, split summaries, and future properties in
source JSON. Activity graph data is preserved separately; queryable legacy profile
comes from the Dive document's records described below.

On targeted refresh, a missing incoming numeric/coordinate/ID value retains the
previous stored value via COALESCE. Strings and parsed note fields are replaced;
an incoming null activity_details retains its previous document. This avoids
erasing existing measurements when one source variant omits them. It does not
provide local editing for imported summary fields.

## FIT preservation and projection

Accept raw FIT identified by `.FIT` at bytes 8–11, or ZIP containing case-insensitive
`.fit` entries. Keep ZIP entry names as metadata; do not extract paths to disk.
Ignore unrelated ZIP entries. Limits: downloaded body 128 MiB, total uncompressed
FIT bytes 128 MiB, at most 64 FIT files. Reject archives with no FIT files. ZIP
integrity and FIT CRC checks must run. A file can contain multiple chained FIT
sequences; decode until all bytes are consumed, without silently dropping a tail.

For each sequence preserve protocol/profile versions and **all** messages, not
just recognized dive records. Store message number/name/timestamp, every field's
number/name/type/base type/units/scale/offset/raw/value, component-expanded status,
and developer index/description where available. Resolve dynamic subfields using
the surrounding message. Preserve unknown data by numeric IDs. Developer-field
definitions are sequence-local. Invalid FIT sentinel values become null; finite
zeroes stay zero. Apply scale/offset according to FIT (`raw / scale - offset`)
and preserve arrays. FIT timestamps become Unix milliseconds; coordinates used
for summary fallback convert semicircles by `value * 180 / 2^31`.

`fit.version` is 1. Each decoded file also has `original_base64`. Persist each
file's exact decoded bytes as a SQL BLOB/BYTEA with byte count and SHA-256.
The persisted bytes are the individual FIT files, not the surrounding ZIP
container. Re-decoding from SQL requires no Garmin request. Do not advertise a
public “download original FIT” endpoint: no dedicated such route exists; original
base64 is in the complete decoded document.

Queryable summary precedence is source JSON, then FIT session summaries, then
FIT dive summaries, filling missing fields only. Session summaries describe the
activity; a dive-summary message may describe a lap. Personal nullable summary
fields preserve zero. Legacy scalar summary extraction treats zero as missing.
Profile records with timestamps from all files/sequences are sorted stably by
timestamp; equal timestamps retain encounter order. Index from zero, and compute
elapsed time from the first sorted record. If FIT has records, replace the legacy
profile; otherwise retain any legacy records. If FIT has recognized alerts,
replace legacy alerts; otherwise retain them.

| FIT field | Indexed key / API field |
| --- | --- |
| `heart_rate`, `temperature`, `depth` | HR / heart_rate; TEMPERATURE / temperature_celsius; DEPTH / depth_meters |
| `next_stop_depth`, `next_stop_time` | NEXT_STOP_DEPTH / next_stop_depth_meters; NEXT_STOP_TIME / next_stop_time_seconds |
| `time_to_surface`, `ndl_time` | TIME_TO_SURFACE / time_to_surface_seconds; NDL_TIME / ndl_time_seconds |
| `cns_load`, `n2_load` | PERCENT_CNS_LOAD / percent_cns_load; PERCENT_N2_LOAD / percent_n2_load |
| `pressure_sac`, `volume_sac`, `rmv` | PSAC / psac; VSAC / vsac; RMV / rmv |
| `air_time_remaining`, `po2`, `ascent_rate` | AIR_TIME_REMAINING / air_time_remaining_seconds; PO2 / po2; ASCENT_RATE / ascent_rate |

Other record fields remain in raw JSON, with uppercase metadata keys or
`FIT_FIELD_<number>`; developer keys use `FIT_DEVELOPER_<index>_<number>`.
Record metadata keys are sorted. Recognized event alerts include dive alert, gas
switch, tank reserve/critical/lost/battery-low, and tank-pod connect/disconnect.
All other events still exist in the full FIT document.

Legacy profiles map `recordMetaDatas[].key/position` into `recordData` arrays.
TIMESTAMP is required if rows exist; each timestamp must be numeric and valid.
Round timestamps to milliseconds, retain source row order, and compute elapsed
from the first row. Unknown/out-of-range optional metrics are absent. Keep raw
record arrays and original alerts. No inferred resampling or smoothing is applied.

## Note parsing

Normalize CRLF/CR to LF and remove one leading BOM. Recognize trimmed line headers,
normalizing nonbreaking/narrow-nonbreaking spaces for rating matching. Rating
grammar, case insensitive: `N`, optional `:` or `=`, unsigned decimal number
(dot or comma), optional `/` and unsigned decimal denominator. It must occupy
the entire trimmed line. Examples: `N:4/5`, `n = 4,5 / 5`, `N4`.

Other section headers are `N:`, `Commentaire:`, `Commentaires:`, `HL:` followed
by optional same-line text. Rating lines end the previous text section. A malformed
explicit `N:` marks the rating present but invalid. Nonfinite/negative values,
nonpositive denominators, or score > denominator produce null rating and max.
The last explicit rating wins; a missing denominator stays null.

Comment sections retain internal line breaks; repeated comment sections join
with an empty line, and the final combined comment is outer-trimmed. Highlights
append one nonempty trimmed line per item. Remove one leading `- `, `* `, `• `,
or `– `; a standalone bullet contributes nothing. Repeated HL sections append.
Text outside a recognized section is not parsed but remains in the raw source.
Parse notes and description separately, then fall back by section presence,
not by whether its parsed value is nonempty. Migration 8 backfills locally.

## Dive year choices

`GET /api/v1/dives/years` reads distinct four-digit year prefixes from stored
validated local_date values, descending, then returns them as integers 1–9999.
Omit missing/unparseable/zero years. The response is `{items: [...]}`, with an
empty array for no choices. Use the local date as stored, without UTC conversion,
and do not synthesize intermediate years. Read across the whole logbook,
independently of dive pagination and all other filters; no provider calls or
background jobs are needed. The route is public after the normal setup/readiness
gate. Newly imported/updated dates become visible on the next read.

## Rating selection filters

The ordinary dive list, trip dive list, and trip list accept an optional `ratings` selection.
Split comma-separated values across all occurrences of that query parameter,
trim whitespace, reject empty/malformed/negative/nonfinite numbers and more than
100 supplied values, and deduplicate numerically. An omitted parameter adds no
rating condition. Bind the selected scores in `dives.rating IN (...)`, before
cursor pagination and together with the other AND predicates. This implements
exact OR matching, excludes SQL NULL ratings, preserves zero/decimal values,
and never normalizes by rating_max. Keep the ordinary descending date/time/ID
ordering. Invalid selections return 400 invalid_request. No migration or
provider request is required.

For `/api/v1/trips`, apply the same parsed selection to `dive_trips.rating`.
This is the independently set trip rating, not a member-dive rating or average.
Trip scores remain 0–5, so a valid selection above 5 simply matches no trip at
that score. Reject the removed min_rating/max_rating query parameters with
400 invalid_request and a message directing callers to ratings; check parameter
presence even when values are empty or ratings is also supplied. No bound
parsing or SQL range predicates remain.
Apply every condition before trip cursor/limit handling, retaining descending
start_date/ID ordering and the existing visibility rules for deleted/empty trips.

## Country enrichment and trip reconciliation

Country keys are `country-v1:<latitude fixed to 6 decimals>,<longitude fixed to 6 decimals>`.
Both values must be finite/in range; zero coordinates are valid. Entry pair wins,
otherwise exit pair. A changed key clears prior detected country, never the manual
override. Create a queue entry only when GPS is valid and no override exists;
reuse ready/not_found cache results across dives and restarts. Names resolve
through the local country catalog with lowercase, NFD decomposition and removal
of combining marks. Build choices from canonicalized region codes, deduplicate
codes, and discard regions without both French and English labels. This removes
deprecated-code duplicates and blank historical regions from both country-list
endpoints. Recognized unambiguous legacy codes resolve to the canonical country;
do not guess a country for historical codes split into multiple replacements or
transliterate arbitrary aliases not recognized by the catalog.

The dive-country filter choices query distinct
`COALESCE(country_override,country_code)` values from all stored dives, map them
to labeled canonical countries, deduplicate by canonical code, and sort by code.
Ignore unknown/null/empty values and apply optional normalized `q` substring
search to code/French/English names. This GET is a local SQL read and must not
resolve GPS on demand. Country filtering of dives accepts persisted historical
code aliases for a selected canonical country. The worldwide catalog remains
independent of dive data for residence forms and first-run setup.

Admin review queries dives with `COALESCE(country_override,country_code,'')=''`,
left-joining their current country_lookup_key to the cache. Report no key as
missing_coordinates, not_found/error as those cached states, and other/missing
cache states as pending. Each row contains the ordinary dive summary and lookup
coordinates/error/attempts/latest result timestamp. Return one row per dive even
for shared lookup keys; exclude corrected dives. Filter by optional lookup_status,
then paginate with the normal descending dive tuple. GET performs no provider
request or retry. All `/api/v1/admin/` routes must pass session/version checks on
reads too. Existing country PATCH removes only that dive from review and runs
normal country/trip reconciliation transactionally.

Nominatim reverse request: `format=jsonv2`, `zoom=5`, `addressdetails=1`,
`accept-language=en`, lat/lon six decimals; identify the app with User-Agent.
HTTPS origin only except localhost proxies/tests; no credentials/path/query in
configured origin. Disable redirects. Timeout 30 seconds, response limit 1 MiB.
404 or no country is `not_found`, not an endlessly retried error. Uppercase the
country code. When sovereign country is FR, recognize regional ISO3166-2 values
`FR-PF/NC/WF/PM/TF/GP/MQ/GF/RE/YT/BL/MF` as their corresponding territory codes.
Other unknown catalog codes are errors.

The worker reconciles SQL approximately every 30 seconds, including when outgoing
lookups are disabled. Claim a currently referenced, pending, due job under the
travel transaction lock. Lease duration is 2 minutes; allow only one active
country lookup across the database. Reserve the singleton pacing row before
HTTP; default/minimum for public Nominatim is 15 seconds. No SQL transaction is
held across HTTP. Save only if lease owner still matches; update affected dives
and reconcile trips in the same completion transaction. Provider 429/5xx/network
failures retry, at most five consecutive failed attempts, exponential 30-second
base backoff plus Retry-After. 400/401/403 pause the process; error/not_found work
needs explicit retry (or eligible new work after restart). Never refresh successful
coordinates on a periodic TTL. Shutdown releases owned leases best-effort.

Trip reconciliation first builds chronological country/date groups using the
[product rules](product.md#dive-trips). For each group containing auto-mode dives:

1. Prefer a nondeleted automatic trip already linked to a member and not reused
   for another group in this rebuild. Prefer a trip with edited metadata, own
   media, or manual members over an unprotected candidate.
2. Otherwise reuse an unused automatic trip whose anchor dive is in the group,
   iterating existing trips by creation time and ID.
3. Otherwise create an automatic trip anchored at the first auto-mode dive.
4. Assign only auto-mode dives to the selected ID. Clear auto-mode links with
   no desired group. Leave manual and excluded modes untouched.
5. Recompute dates from **all** linked dives, derived year/country unless overridden,
   and generated French/English names unless overridden. Keep old empty trip rows;
   list visibility handles empty untouched groups.

Run this under one serialized transaction after residence, membership, country,
or sync changes; PostgreSQL uses advisory transaction lock `1734439538`. Include
stable-ID selection and overrides in a port: recreating trips on every scan would
orphan editorial work and attachments.

## Translation worker

Discover translatable text on startup, local edit signals, and every 30 seconds.
Translate only current content, while retaining older cache revisions. Source
kinds are `dive` (comment + highlights), `media` (caption + description), and
`trip` (custom untranslated name + subtitle + description + operator comment +
price). Custom names with explicit name_en do not send that name to DeepL.
Discovery includes retained media rows of soft-deleted trips in this release;
soft deletion hides their routes but does not purge media translation work.

Profile is SHA-256 hex of `deepl/fr/v1/<locale>/<glossary>`. Source key is SHA-256
hex of `profile + ":" + compact source JSON`. JSON uses declared field order
`kind,name,subtitle,operator_comment,price,comment,highlights,caption,description`,
omitting empty optional fields/arrays. Go JSON escaping is part of byte-for-byte
cache compatibility; a port can instead migrate/rekey rows from decoded source
values, retaining ready results. No entity ID, filename, or secret participates.

Reconciliation marks current-profile entries inactive then upserts current
nonempty sources active. Ready entries remain reusable when text is restored.
Claim one active pending/due row atomically with a 2-minute owner lease. Keep a
result vector aligned to source segments; null means unfinished, empty source
segments complete locally. Deduplicate identical segments within a job/batch.
At most 50 unique nonempty texts and 120 KiB encoded request per batch. Save
successful partial vectors immediately; retry only incomplete segments. An
oversized single segment becomes error `translation_text_too_large`.

POST `/v2/translate` to configured DeepL origin with `Authorization: DeepL-Auth-Key ...`.
Body: `text`, `source_lang: FR`, target locale EN-GB/EN-US, `preserve_formatting: true`,
optional glossary, and context. Context starts with
`Compte rendu de plongée sous-marine et observations de la vie marine.` plus LF
and source comment/description, truncated to 4000 Unicode characters. Timeout
30 seconds, no redirects, response limit 4 MiB; result count must match batch.
The default `:fx` key selects api-free.deepl.com, otherwise api.deepl.com.

Requests wait 2 seconds by default, per worker/process, independently of Garmin.
Network/429/5xx failures retry up to five consecutive failures with 30-second
exponential base and Retry-After. Successful batches reset the failure count.
400/404 configuration, 401/403 auth, and 456 quota failures pause calls; 413 is
oversized text. Explicit retry resets errors/pause without losing ready or partial
results; startup with a configured client also requeues failed translation jobs.
SQL leases prevent duplicate claims, but request pacing is not a global DeepL
quota limiter across processes. API rendering only reads the current source's
cache; it never waits for the provider. Ready trip-name saves update the bilingual
search index atomically and only if the source still matches.

## Filesystem media lifecycle

Use a confined filesystem root; no symlink traversal or user-controlled shell
commands. Scan only direct regular files of known dive/trip owners. Unknown owner
directories are left for later. Hidden files, nested directories, invalid names,
and unsupported extensions are skipped. A failed directory listing must not prune
its prior index. A missing known directory can remove its indexed attachments.
Soft-deleted trips retain indexed media and files, although active routes reject them.

For each eligible file, skip only if size+mtime match, thumbnail is ready/present,
and its cache key matches current processing options. Otherwise hash bytes,
inspect, generate preview, verify the same file identity/size/mtime still exists,
then upsert metadata. Preserve attachment ID, original creation time, caption,
description, favorite, and explicit projection/stereo overrides. Processing failure
records thumbnail error but keeps the original; a file changed during processing
leaves its previous metadata intact. Prune missing originals and their previews;
cover foreign keys clear automatically. Scan commits per item, not all-or-nothing.

Uploads stream to `.uploads/<random-id>`, enforce max+1 byte check and nonempty
content, validate the complete multipart body, then publish atomically without
overwrite (reference implementation uses a hard link within the same root).
Confirm the owner exists and serialize media mutation/indexing within the serve
process. Index after publication; roll back the published file on indexing error.
Deletion stages an original into hidden trash, deletes SQL metadata, then removes
trash/preview; on SQL failure restore the original best-effort. These are not
cross-resource ACID transactions; rescanning repairs recoverable discrepancies.

FFprobe reads the first usable video/image stream, retaining probe JSON, dimensions,
duration, stereo hints, and spherical mapping. Probe and thumbnail commands each
have a 2-minute timeout, bounded diagnostic output, and no shell interpolation.
Linux/macOS reference passes an already confined file descriptor (`/dev/fd/3`).
Select a representative frame using FFmpeg thumbnail=30. Crop one eye for
side-by-side/top-bottom; use 640x360 perspective (90° horizontal, 60° vertical)
for equirectangular 360, otherwise aspect-preserving maximum 640x640. Output JPEG.

Thumbnail key is SHA-256 hex of
`v2:<mediaID>:<originalSHA256>:<resolvedStereo>:<effectiveProjection>:<format>`.
Store `.thumbnails/<key>.jpg`, publish through a temporary file, remove obsolete
previews after successful metadata save. Originals and thumbnails support standard
HEAD/ranges/ETag/Last-Modified semantics. Reject original reads with
`409 media_changed` when indexed size/mtime no longer match; require rescan rather
than serving bytes under stale validators. Preserve normal conditional HTTP
statuses (304/412/416). No directory browsing.
