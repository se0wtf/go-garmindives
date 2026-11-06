# Acceptance scenarios and implementation checklist

[Specification index](../../SPECS.md) · [Backend algorithms](backend.md)

These scenarios define observable compatibility for a frontend or backend port.
Use synthetic fixtures and mocked provider responses. “Expected” means the
documented behavior to preserve, not a claim that every row has its own separate
automated test today. The linked test files contain the reference examples and
edge cases. Run against SQLite and PostgreSQL for any backend claiming both.

## Setup, configuration, and security

| ID | Given / when | Expected |
| --- | --- | --- |
| A01 | Fresh config; GET dives/assets/auth/session/config | 503 not_configured; setup/status/countries and docs remain available |
| A02 | Submit valid SQL settings, long admin password, dated residence with null end | 201 starting; YAML initialized identity matches SQL; readiness eventually true; no automatic login |
| A03 | Existing imported dives/FIT/media before first administrator setup | Preserve all bytes/metadata; add administrator/residence only |
| A04 | Config file save fails during setup | No administrator or new residence rows commit; explicit error; schema migrations may already exist |
| A05 | Concurrent setup on same process/database | One administrator; no reset by a competing request; successful initialization stays closed |
| A06 | Initialized database outage or missing/mismatched credentials | configured remains true, normal API unavailable, POST setup 409 |
| A07 | Database lost after runtime became ready | /healthz stays alive, /readyz 503, setup/status ready=false, setup stays closed |
| A08 | YAML lost; attempt setup with another valid new password | Reject adoption; existing SQL password required; successful adoption preserves identity/history |
| A09 | ENV overrides DB URI; setup omits managed URI | Use ENV; do not copy that secret into YAML; reject an API change to a different managed value |
| A10 | Admin stages another DB in config | Return active/pending redacted display and restart_required; keep active DB/session usable until restart |
| A11 | Anonymous read vs any mutation/config GET | Public domain reads allowed; every domain mutation and private config GET returns 401 |
| A12 | Successful login / repeated login | HttpOnly Strict host-only cookie; token rotated; only digest stored in SQL |
| A13 | Backend restart within session lifetime | Same session remains authenticated unless intentionally revoked during a language cutover |
| A14 | Password change with current password | New PHC hash/version; every old session invalid, including one re-saved by a late in-flight request |
| A15 | Logout / idle / absolute expiry | Current token becomes unusable; public browsing alone does not extend idle expiry |
| A16 | Cross-site setup/login/write | 403 csrf_rejected; same-origin browser and ordinary authorized nonbrowser requests work |
| A17 | Excess password attempts | 429 and Retry-After; hash work bounded; forwarded IP headers cannot bypass peer bucket |
| A18 | Password with leading/trailing spaces or Unicode | Preserve exact valid value; enforce code-point bounds; reject known predictable/all-identical patterns |
| A19 | Local CLI password recovery | No plaintext password argument/file; confirmation; all sessions revoked |
| A20 | JSON/YAML output selection or memory SQLite config | Reject unsupported persistence; no Garmin call before initialized SQL validation |

The [settings matrix](settings.md) is exhaustive: each operator YAML leaf has a
flag, ENV name and built-in default. Verify every source independently, flags >
ENV > YAML > defaults, valid flags overriding invalid ENV, canonical DB ENV aliases,
last-flag wins, private YAML credentials, help/API redaction, and preservation of
all flag overrides across setup/database PATCH. ENV/flag secrets must not be saved.

Reference evidence: [settings tests](../../internal/config/settings_test.go),
[runtime wiring tests](../../internal/app/config_runtime_test.go), [auth tests](../../internal/app/auth_test.go), [configuration tests](../../internal/app/config_test.go).

## Sync, notes, FIT, and measurements

| ID | Given / when | Expected |
| --- | --- | --- |
| B01 | First history import, mixed sport types and duplicate activities | Import each supported dive once; ignore other types |
| B02 | Second incremental sync with complete FIT | List history; do not redownload details/FIT; preserve local edits |
| B03 | Stored dive lacks FIT | Backfill original bytes/profile without overwriting source metadata/local edits |
| B04 | Valid FIT with no record messages | Mark import complete; do not redownload every run; retain legacy profile if present |
| B05 | Later required download or CRC check fails in a batch | No partial batch of new/backfilled dives saved |
| B06 | Targeted existing dive refresh | No history list; refresh metadata/notes; preserve FIT/media/covers/country/manual trip mode |
| B07 | Targeted absent/non-dive/mismatched ID | Import absent supported dive; reject unsupported or mismatched activity without partial changes |
| B08 | Raw FIT, ZIP with multiple FITs, chained sequences | All valid sequences/messages/developer fields retained with exact individual original bytes |
| B09 | >128 MiB, >64 files, no FIT entries, corrupt data | Bounded failure; no partial persisted record |
| B10 | Read original FIT locally after network access removed | Size/checksum validation passes; decode original without Garmin |
| B11 | Missing vs recorded zero depth/temperature/HR | Zero retained in FIT/Personal profiles; missing omitted/null per schema |
| B12 | Equal record timestamps across FIT files | Stable order, distinct indices, elapsed from first record; no timestamp deduplication |
| B13 | Session and lap-level dive summaries disagree | Fill missing queryable summary from session before dive_summary; retain raw messages |
| B14 | Garmin returns 429 with seconds/date Retry-After | Wait at least header/config interval; <=3 safe retries; no login/MFA replay |
| B15 | Cancellation during pacing/provider transfer | Stop promptly; close responses; do not save staged partial sync |
| B16 | Real API key rejected | Return auth failure; never silently use email/password |

Reference evidence: [sync](../../internal/app/sync_test.go), [targeted refresh](../../internal/app/refresh_test.go),
[FIT decoding](../../internal/app/fit_test.go), [FIT storage](../../internal/app/fit_store_test.go),
[Personal API](../../internal/app/personal_api_test.go), [pacing](../../internal/garminhttp/rate_limit_test.go).

Complete note-parser fixtures (JSON-like expected values; LF means a real newline):

| Source notes | Fallback description | Expected parsed fields |
| --- | --- | --- |
| `N:4/5\nCommentaire:\nEnfin !\nHL:\n- Requins\nRaies` | empty | rating 4, max 5, comment `Enfin !`, highlights `[Requins, Raies]` |
| `n = 4,5 / 5` | empty | rating 4.5, max 5 |
| `N:0` | empty | rating 0, max null |
| `N:8/5` | `N:4/5` | rating/max null; explicit invalid notes blocks fallback |
| `Commentaire:` | `Commentaire: Fallback\nHL: Raies` | comment empty; highlights `[Raies]` |
| `HL:\n* Raies\n• Requins\n– Tortue\n-` | empty | highlights `[Raies, Requins, Tortue]` |
| unlabeled prose | empty | empty comment/highlights; raw prose preserved |

Reference: [note parsing/migration tests](../../internal/app/dive_notes_test.go). Verify imported
legacy notes and Personal description shapes produce the same structured results.

## Countries, residence, and trip grouping

Assume France home throughout unless stated otherwise. Trips are chronological;
dates below are local, not UTC conversions. Compare sets/membership rather than
newly generated random trip IDs.

| ID | Fixture / change | Expected |
| --- | --- | --- |
| C01 | EG dives Dec 28 2025 and Jan 3 2026 | One trip Égypte 2025 / Egypt 2025; end date in 2026 |
| C02 | EG Jan 31 and Apr 30 2025 | One trip; clamped three-month boundary is inclusive |
| C03 | EG Jan 31 and May 1 2025 | Two trips |
| C04 | EG March then EG July | Two trips, possibly same generated display name; distinct IDs |
| C05 | EG, FR home dive, EG within one month | Two foreign trips separated by the home dive |
| C06 | EG, PF, EG close together | Three groups, country boundaries preserved |
| C07 | No matching residence for dates | No automatic assignment; existing manual links stay |
| C08 | Add/update/delete a residence period | Auto assignments recalculate in that transaction, without waiting for a cron |
| C09 | Residence Jan 1–Jun 30 and another Jun 30–infinity | Reject overlap; changing second start to Jul 1 is valid |
| C10 | Entry GPS invalid, exit GPS valid | Use exit coordinate pair; zeroes are allowed; invalid both leaves unknown |
| C11 | Two dives share rounded coordinate key | One cached lookup result; reuse across restarts |
| C12 | Nominatim country FR with regional ISO FR-PF or FR-NC | Destination PF or NC, not FR |
| C13 | Offshore provider has no country | Unknown/not_found; no invented country; manual EG override available |
| C14 | Set country override; later sync/GPS change | Override survives; detected country/key can change separately |
| C15 | Clear country override | Reuse cached detection or queue lookup; recalculate trip |
| C16 | Manually move one dive to another trip | One membership, manual mode; old/new trip extents and favorites update |
| C17 | Remove a dive, then rebuild repeatedly | Excluded remains unassigned until trip_mode auto |
| C18 | Replace trip dive_ids | Supplied dives manual; removed dives excluded; no duplicate/nonexistent IDs accepted |
| C19 | Override name/year/country, then rebuild/backfill | Overrides and ID retained; dates still reflect linked dives |
| C20 | Clear name override, with linked dives | Generate localized country/year names again |
| C21 | Backfill joins temporary automatic groups; one has media/custom fields | Favor protected group ID; retain edited data/media rows |
| C22 | Delete trip | Hide it and its asset routes, preserve originals/rows/dives, exclude current members |
| C23 | Empty automatic untouched / edited / manual / media-bearing groups | Hide only untouched automatic groups without media |
| C24 | Bilingual/accent-insensitive country and trip-name filters | Codes/French/English country names match; custom English trip name becomes searchable after ready translation |
| C25 | Trip overlapping date filter and nullable rating | Inclusive overlap; empty dates excluded; null rating excluded by rating selections; zero accepted |
| C26 | Fetch setup or configured country choices | Unique canonical codes, nonempty distinct French/English labels; France and United Kingdom searches return one choice each |
| C27 | Country input FX/UK vs blank/unlabeled historical code | Resolve FX to FR and UK to GB; reject blank or ambiguous unlabeled historical choices |
| C28 | GET dives/countries with duplicates, unknowns and manual overrides | Return only distinct effective stored destinations, bilingual and sorted; empty logbook yields items: [] |
| C29 | Use a canonical dive-country choice over historical stored code aliases | Country filter returns matching dives; home-country/catalog endpoints still include worldwide choices |
| C30 | Anonymous GET/HEAD of admin missing-country review | 401 after setup; normal initialization gate before setup; public catalogs/dive lists unaffected |
| C31 | Unresolved shared lookup, error, pending and missing GPS | Per-dive review rows with correct status, name/date/GPS/error/check time; status filtering and cursor pagination work |
| C32 | Set a country on one not_found dive, then clear it | Remove only that dive from review and recalculate eligible trips; other dives at the same coordinates stay unresolved; clearing can restore the row |

Reference evidence: [travel rules](../../internal/app/travel_test.go), [geocoding](../../internal/app/geocoding_test.go),
[travel integration/pagination](../../internal/app/travel_integration_test.go).
Country-choice regressions also live in [catalog tests](../../internal/country/catalog_test.go)
and [country endpoint tests](../../internal/app/countries_api_test.go).
Admin review coverage is in [country issue tests](../../internal/app/country_issues_test.go).

## Media and translations

| ID | Fixture / change | Expected |
| --- | --- | --- |
| D01 | One complete multipart photo/video upload | 201, photo profile applied or source retained on failure, video bytes unchanged; owner-relative URLs returned |
| D02 | Duplicate owner filename, including unindexed on-disk file | 409, no overwrite; same name under another owner allowed |
| D03 | Empty/oversized/extra multipart part, invalid filename | Reject before publishing; clean temporary files |
| D04 | Direct file copy then scan/startup | Index known owners; log counts; unknown owner untouched for later |
| D05 | Rescan unchanged file; edit captions/favorite; replace bytes under same name | Skip reusable thumbnail; retain ID/editorial fields while updating technical metadata |
| D06 | Original disappears vs directory listing fails | Missing original prunes index/cover; failed listing does not prune |
| D07 | Symlink/traversal/nested/hidden file | Never escape/import through root; skip or reject appropriately |
| D08 | No FFmpeg/codec, otherwise valid supported extension | Original retained and served; thumbnail error exposed, later rescan can recover |
| D09 | Owned photo chosen as cover; delete/remove it | Cover on list/detail; then null automatically; wrong owner/video rejected atomically |
| D10 | Favorite dive photo/video; move/unlink dive | Favorites follow current trip membership without copying bytes |
| D11 | Trip-owned photo upload/cover | Independent owner/path; cannot favorite as dive media |
| D12 | HEAD/ranges/If-None-Match/invalid range | Correct lengths/206/304/416; no directory listing |
| D13 | On-disk original size/mtime changes since index | 409 media_changed until rescan |
| D14 | 2:1 video without spherical metadata | Auto stays flat; explicit 360 enables equirectangular presentation |
| D15 | Spherical equirectangular metadata vs cubemap | 360 detected; perspective thumbnail for equirectangular, scaled frame for other formats |
| D16 | Caption/description translated asynchronously | API immediately retains French + non-ready English state; later ready cache, no request-time provider call |
| D17 | French changes after ready translation | New source key; never show old English for new source; restored text reuses old ready result |
| D18 | Remove provider key after caching English | Ready cache still served; untranslated content disabled; empty content ready |
| D19 | Multi-batch content; later batch transient failure | Successful segments persist, only missing segments retried; 50/120KiB limits enforced |
| D20 | Provider quota/auth failure / retry | Pause, fallback French, explicit retry preserves ready progress; no automatic subscription upgrade |
| D21 | Sync process adds new text while serve runs | Background reconciliation eventually discovers it (~30-second scan, plus paced queue delay) |
| D22 | Backend restarts with retained media/FIT/overrides/cache | User edits and retained bytes survive; reusable thumbnails and ready translations reused |
| D23 | Photo below both strict thresholds vs exactly 2048 shorter side or at least 1,000,000 bytes | Keep small source; otherwise JXL 1080 shorter side at d=1.5/e=7; JXL thumbnail 480 at d=2/e=8; no upscaling |
| D24 | Encoder/thumbnail/SQL failure during photo conversion | Source bytes retained; SQL failure rolls back publication; metadata and covers remain consistent |
| D25 | Interrupted publication before vs after SQL commit, including in-place JXL | Startup rolls back uncommitted target/restores backup, or completes committed cleanup; changed source bytes retained |
| D26 | Forced rescan or missing-thumbnail repair | Retained photo SHA/ID/editorial fields unchanged; no repeated master encoding |
| D27 | JXL supported, wildcard Accept, or explicit q=0; format override | Correct native/JPEG selection, Vary: Accept for auto, distinct ETags; format=original/jpeg respected |
| D28 | Repeat fallback/conditional GET, HEAD, media deletion | Cached JPEG reused, 304/HEAD have no body, deletion clears corresponding fallback caches |
| D29 | New small photo copied under converted original-name alias | Same media ID, small bytes retained, obsolete JXL retired after SQL commit; subsequent scan creates no duplicate |
| D30 | Tiled/rotated HEIC | Inspect complete oriented dimensions; all quadrants remain in 1080 master, 480 thumbnail and JPEG fallback; never select one HEVC tile |
| D31 | Restore/re-upload identical original after legacy HEIC conversion | Reprocess instead of deleting as unchanged; preserve ID/editorial fields/favorites/cover; missing source reports error and leaves old JXL intact |
| D32 | Small legacy HEIC with old thumbnail/JPEG cache | Source bytes retained; decoder-version keys force complete-image previews and JPEG fallback |
| D33 | Missing HEIF tool/codec or multiple top-level images | Retain source and report processing error; no FFmpeg first-tile fallback or arbitrary output selection |
| D34 | Interrupted legacy HEIC publication with source present | Recovery retains that source for the corrected decoder rather than finishing deletion |

Reference evidence: [media](../../internal/app/media_test.go), [descriptions](../../internal/app/media_description_test.go),
[360](../../internal/app/media_360_test.go), [cover](../../internal/app/cover_test.go), [trip media/favorites](../../internal/app/trip_media_test.go),
[translations](../../internal/app/translations_test.go).
Photo profile and recovery evidence: [photo lifecycle](../../internal/app/media_photo_test.go),
[publication recovery](../../internal/app/media_photo_jobs_test.go),
[real JPEG XL encoding](../../internal/media/photo_test.go).
HEIC evidence: [grid/orientation decoding](../../internal/media/photo_heif_test.go),
[legacy repairs and cache replacement](../../internal/app/media_heif_test.go).

## API and migration verification

Sample JSON must omit raw for anonymous and admin callers, even with
include_raw=true. Large raw_json values stay stored internally without inflating
responses or being selected by sample queries. Preserve every indexed reading,
zero/missing distinctions, ordering and cursor pagination. Reference:
[compact sample tests](../../internal/app/compact_samples_test.go), also exercised
against PostgreSQL, and [authenticated privacy checks](../../internal/app/fit_download_test.go).

Dive detail/list/sample JSON must not embed a full FIT document or original
Base64 file bytes, for anonymous or authenticated callers, including include=fit.
Large private fit_json data must not inflate detail responses or be selected by
detail reads. Indexed graph measurements remain available. The only FIT download
is GET /api/v1/admin/dives/{id}/fit: setup gating applies, GET/HEAD/ranges require
a valid admin session, logout/revocation removes access, and no public alias exists.
Verify exact single-file bytes, safe attachment filename, private/no-store cache,
HEAD and ranges; multiple files download as a ZIP with safe distinct entry names
and unchanged bytes. Missing files return 404 fit_unavailable, unknown dives
404 not_found, invalid IDs 400, and checksum failures never return corrupt bytes.
The download makes no Garmin request. Reference:
[FIT API/download tests](../../internal/app/fit_download_test.go),
[SQL FIT retention](../../internal/app/fit_store_test.go), and
[PostgreSQL integration](../../internal/app/integration_test.go).

`GET /api/v1/dives/years` is gated before setup and public afterward. An empty
logbook returns items: []; multiple dives in one year produce a single numeric
choice, ordered newest first. Include years beyond the first dive-list page,
leave gaps rather than inventing years, omit missing dates/invalid years, and use
local dates at Dec/Jan timezone boundaries. Active filters/pagination must not
narrow the choice list. Every offered year must work with the existing dive
year filter, and new imports must update choices on the next read. Reference:
[year-choice API tests](../../internal/app/dive_years_test.go) and
[PostgreSQL integration](../../internal/app/integration_test.go).

Verify all operations in [the API map](api-map.md), including auth/status/config,
against [OpenAPI](../openapi.yaml). Validate response field omission/nullability,
empty arrays, JSON errors, cursor round trips, ties in ordering, all filters, and
unavailable startup state. Mutations must be tested through the production auth
wrapper, not only a low-level domain router. Media byte-range tests must read real
response bytes, not only inspect status codes.

For dive rating selections, `ratings=4,5` includes exact 4 and 5 scores and
excludes 4.5/unrated dives; `ratings=4.5` includes that decimal, and `ratings=0`
includes zero without matching null. Omission leaves all scores/unrated dives
eligible. Verify 8/10 matches 8 without normalization, duplicates/repeated
parameters, invalid/empty/negative/nonfinite values and the 100-value limit.
Combine selections with year/country/date/location/trip filters and paginate
without losing or duplicating matches. Trip dive lists apply the same selection
and pagination within their membership. Reference: [rating filter tests](../../internal/app/dive_rating_filters_test.go)
and [PostgreSQL integration](../../internal/app/integration_test.go).

Trip lists use the same exact `ratings` selection, validation and pagination.
Verify 4/5 versus 4.5, zero versus null, repeated values, empty/invalid/nonfinite
inputs and the 100-value limit. The matched score is the trip's own rating:
a trip with highly rated member dives remains excluded when its own rating is
missing or unselected. Preserve all existing visibility rules and intersect
name/year/country/date filters. Removed min_rating/max_rating parameters return
400 invalid_request with guidance to use ratings, even when empty or combined
with a selection. Scores above 5 are valid query selections but match no trip.
Reference: [trip rating filter tests](../../internal/app/trip_rating_filters_test.go),
also executed against PostgreSQL by the integration suite.

Canceled dive/trip requests must log at debug without attempting an error response
when both the request and failed operation report cancellation. Preserve error
logging for timeouts, unrelated failures, and internal cancellation with a live
caller. Both list endpoints must remain responsive while a country provider call
is stalled outside a transaction. Reference: [cancellation logging tests](../../internal/app/request_cancellation_test.go)
and [worker/API concurrency tests](../../internal/app/travel_integration_test.go).

Upgrade fixtures from earlier schemas must keep dives, original FIT, descriptions,
covers, favorites, trips, residence, and credentials. Exercise rollback and reject
newer schemas. Test a PostgreSQL BYTEA round trip and SQLite foreign-key checks.
For a language cutover, compare a sanitized representative installation before and
after, including source JSON, original byte checksums, membership/overrides, and
cached English. Document whether active sessions/token caches are migrated or revoked.

Reference evidence: [SQL storage](../../internal/app/sqlstore_test.go), [OpenAPI route/schema checks](../../internal/app/openapi_test.go),
[PostgreSQL/live integration entry points](../../internal/app/integration_test.go).
The existing suite requires real Garmin credentials only for explicitly enabled
live tests; acceptance work should normally run without external account access.

## Pending media inbox

| ID | Fixture / action | Expected |
| --- | --- | --- |
| P01 | Unzoned capture within one dive window | Interpret in dive timezone; import and persist assumed timezone/UTC capture |
| P02 | Exactly 10 minutes before start or after end; just outside | Inclusive boundaries match; outside remains pending |
| P03 | Overlapping windows, different local timezones, repeated DST hour | Keep pending with candidates/ambiguity reason; no nearest-dive guess |
| P04 | Missing/invalid capture or no matching dive | Private review; file timestamps ignored; later sync can resolve a cached capture |
| P05 | Copy in progress, replacement, symlink, unsafe inbox path | Settle/check source; preserve newer/external bytes; reject overlapping roots |
| P06 | Filename conflict or codec/SQL failure | No overwrite or incomplete media row; pending original retained; retry same reserved ID |
| P07 | Crash before/after media commit | Recover stable identity/capture; no duplicate; cleanup retains a newer inbox replacement |
| P08 | Unauthenticated GET/HEAD/ranges/list/assign/delete | 401; pending bytes never available through public assets |
| P09 | Admin list/filter/cursor/preview/manual assignment/delete | Correct pagination, private previews, explicit target may override metadata, cleanup on success |
| P10 | Real HEIC capture metadata plus tiled/rotated source | ExifTool date matches local dive; complete JXL image and preserved SQL capture |
| P11 | Worker startup/manual wake/shutdown; SQLite/PostgreSQL | Background imports complete without blocking reads; clean cancellation and both SQL dialects |

Evidence: [capture reader](../../internal/media/capture_test.go),
[matching](../../internal/app/pending_match_test.go),
[lifecycle](../../internal/app/pending_test.go),
[admin API](../../internal/app/pending_http_test.go).

## Video normalization

| ID | Fixture / action | Expected |
| --- | --- | --- |
| V01 | Compatible H.264/AAC MP4 within limits | Original bytes/name retained; thumbnail created; forced scan does not recompress |
| V02 | 4K, portrait, HEVC, MOV with PCM audio | Correct 1080p bounds, H.264/yuv420p, AAC tracks retained; only needed streams re-encoded |
| V03 | Rotated, anamorphic, interlaced, PQ/HLG input | Correct display orientation/aspect; square pixels; deinterlace and SDR BT.709 when needed |
| V04 | Encoder/thumbnail/SQL failure or interruption | Original retained; uncommitted output rolled back; committed output cleanup recovers with stable ID |
| V05 | Pending capture date / Resolve render date only | Confident capture matches; render-only metadata stays pending; private previews/manual assignment work |
| V06 | 360 panorama conversion and forced scan | Complete panorama/aspect preserved; SQL projection/stereo hints survive |
| V07 | Trip upload/scan or filename collision | Same profile, stable editorial metadata; no unrelated destination overwrite |
| V08 | Full-range HEVC/H.264, MOV remux, previously rejected yuvj420p output | Accept 8-bit 4:2:0 aliases without changing levels; no repeat encoding; failed retained originals retry with stable media ID |

Evidence: [encoder tests](../../internal/media/video_test.go),
[upload/scan tests](../../internal/app/media_video_test.go),
[pending videos](../../internal/app/pending_video_test.go).

## Administrator metadata resync

| ID | Fixture / action | Expected |
| --- | --- | --- |
| R01 | Admin single refresh, cached FIT present or absent | Edited metadata/parsed notes replaced; no FIT/profile requests or data writes; local media/cover retained |
| R02 | Bulk snapshot then add another stored dive | Only snapshotted IDs requested, sequentially; new IDs await later sync |
| R03 | One Garmin 404/invalid response | Failed item retained unchanged; others continue; partial parent status |
| R04 | Expired credentials or persistent 429 | Safe failure code; stop remaining requests and mark skipped |
| R05 | Slow Garmin request while reading public APIs | No held SQL transaction; data reads available; status shows current ID |
| R06 | Concurrent starts or browser disconnect | One accepted active job; 409 Location for others; disconnect does not cancel work |
| R07 | Cancellation or restart/shutdown | Preserve committed items, skip unfinished; durable canceled/interrupted state; no silent resume |
| R08 | SQL item-result write fails after metadata update | Roll back metadata and result together; source unchanged |
| R09 | Anonymous GET/HEAD/start/cancel or cross-origin write | 401/403; setup gate returns 503 before configuration |
| R10 | History/results pagination, filters, status reads | Correct cursor pages; safe result codes; no extra Garmin requests |
| R11 | Personal API with synthetic token and real SDK, SQLite/PostgreSQL | One metadata GET per target; shared pacing, zero FIT/history/profile requests |

Evidence: [worker tests](../../internal/app/resync_test.go),
[API/PostgreSQL tests](../../internal/app/resync_http_test.go).
