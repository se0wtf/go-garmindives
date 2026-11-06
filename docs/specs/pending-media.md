# Pending media inbox

[Specification index](../../SPECS.md) · [OpenAPI](../openapi.yaml)

The inbox lets the administrator copy a batch of original photos/videos into one
directory. The backend reads capture metadata, finds matching dives, and imports
only confident matches through the existing photo/video pipelines. Unresolved files stay
in the inbox for private review, manual assignment, or deletion.

## Directory and background work

`media.pending_directory` defaults to `pending`, relative to `media.directory`.
Thus an assets root of `/assets` uses `/assets/pending`; the default Docker layout
uses `/data/assets/pending`. Absolute inbox paths work too, including a separate
volume/filesystem. The directory must not contain the assets root or overlap an
activity, trip, or internal media/cache directory.

```yaml
media:
  directory: assets
  max_upload_bytes: 17179869184
  pending_directory: pending
  pending_scan_interval: 60s
```

`DIVELOG_PENDING_DIR` and `DIVELOG_PENDING_SCAN_INTERVAL` override YAML. The interval
is a Go duration between 1 second and 24 hours. Changes require restarting serve.
The sync command does not scan the inbox and makes no filesystem imports.

Serve inventories the inbox when its background worker starts, then scans every
60 seconds by default. `POST /api/v1/admin/media/pending/scan` queues another pass;
requests coalesce while work is running. `?force=true` re-hashes/re-reads metadata
even if size and mtime are unchanged. Only direct regular photo/video files with supported
extensions are imported. Nested directories, hidden names and symlinks are skipped. Normal activity-directory scans remain startup/admin-triggered.

A new or changed file must be observed with unchanged size/mtime for at least
two seconds before automatic ingestion. With the default polling interval this
usually takes until the next minute. Copy large/slow imports under a hidden
temporary name and rename them into place when complete. File identity, size,
mtime and checksum checks also protect publication and cleanup from replacement.

The worker uses local SQL/filesystem data and ExifTool; it never contacts Garmin,
geocoding, or translation providers. It serializes media mutations and
releases SQL connections before metadata/image processing. Public reads remain
available while the inbox processes a batch.

## Capture time and matching

Read capture metadata before conversion/deletion. ExifTool is invoked without
user configuration and only capture-related tags are requested. Supported sources
are EXIF `DateTimeOriginal` with its subseconds/offset, the corresponding composite
tag, XMP original/date-created metadata, and GPS UTC capture time. Explicitly zoned
capture metadata wins; GPS UTC is used before a floating wall clock. Filesystem
creation/modification times are never treated as capture time. Missing or invalid
capture metadata stays pending. Metadata is cached by file fingerprint; missing
metadata is not repeatedly extracted from unchanged files.

For a dive, derive the end from its stored start plus `duration_seconds`. A usable
interval requires a valid start and a positive finite duration. A start without
an offset needs a valid IANA dive timezone. An offset-bearing start remains usable
without an IANA name, using that explicit offset as the local timezone fallback.
Ambiguous/nonexistent unzoned dive start times are excluded from automatic matching.

The matching window is inclusive:

```text
dive start - 10 minutes <= media capture time <= dive end + 10 minutes
```

An explicit capture UTC offset is honored. Without one, interpret the camera wall
clock in **each candidate dive's local timezone**, never the server/browser timezone.
Account for midnight/year boundaries and timezone transitions. A repeated local
clock hour stays pending even if only one dive fits; a nonexistent clock time
does not match. Evaluate all dives: overlapping windows or matches in different
local timezones remain ambiguous. Never choose the closest dive as a tiebreaker.

Exactly one unambiguous candidate triggers automatic assignment. Unmatched media
are re-evaluated against current SQL dives on later scans, so they can wait for a
later Garmin sync without re-reading unchanged metadata.

| Reason | Meaning / administrator action |
| --- | --- |
| waiting_for_file | New/changed/empty file has not settled yet |
| missing_capture_time | No usable capture tag; assign manually |
| no_matching_dive | No valid interval includes the capture time; wait for sync or assign manually |
| multiple_matches | Several dives fit; show candidates and let admin choose |
| ambiguous_time | A timezone transition repeats the camera clock time; assign manually |
| metadata_error | Metadata tool unavailable, file invalid, or capture tag malformed |
| filename_conflict | Destination has that name/alias; never overwrite it |
| processing_error | Copy, conversion, thumbnail, SQL or cleanup failed; original remains pending |
| source_changed | Source bytes changed since indexing; rescan |
| matched / assigning | Transient successful match / recorded import being completed |

## APIs and frontend behavior

All routes below require an administrator session, including GET, HEAD and byte
ranges. Before application setup the existing 503 `not_configured` gate applies.
An inbox is never served through the public `/assets` routes.

| Method | Path under `/api/v1/admin/media/pending` | Purpose |
| --- | --- | --- |
| GET | root | Filename/id cursor list; `limit` 50 by default, maximum 100; optional `reason` filter |
| GET | `/status` | Current counts by reason, interval/window/settling settings and scan status |
| POST | `/scan` | Queue scan; optional `force=true`; returns 202 queued |
| GET | `/{pendingID}` | Kind (photo/video), capture data, reason/error, suggested dives, private URLs |
| GET/HEAD | `/{pendingID}/thumbnail` | Private JPEG preview: photo at 480-pixel shorter side, video frame up to 640x640 |
| GET/HEAD | `/{pendingID}/file` | Private original, or cached JPEG using normal photo format negotiation |
| POST | `/{pendingID}/assign` | Assign to chosen dive with `{"dive_id": 123456789}`; returns 200 MediaItem |
| DELETE | `/{pendingID}` | Delete pending original/previews and SQL entry; returns 204 |

Listings expose camera `capture.local_time`, optional explicit `capture.offset`,
and `capture.source`. A missing capture is null. Show the local clock as a wall
clock string; do not append Z or interpret it in the browser timezone. Candidates
provide normalized `captured_at`, their timezone, start/end, and assumption flag.
Resolved capture data is saved as optional `MediaItem.capture` after assignment,
including UTC instant when resolvable and `timezone_assumed` provenance. Ordinary
rescans and photo/video conversions preserve it.

Manual assignment may choose **any** existing dive, including one outside the
candidate list and dives/media with incomplete timing information. When the
manual choice cannot resolve an instant, preserve the camera wall clock/source
and omit `captured_at`. Missing metadata does not prevent manual assignment.
Pending previews use complete HEIF/JXL decoding without deleting the source.
File/preview responses use `Cache-Control: private, no-store`; source files accept
the usual `format=auto|original|jpeg`, conditional and byte-range requests. Video
format choices do not transcode private originals. Video previews use a decoded
flat frame; assigned 360 previews use the retained projection hints.

Videos use [the video profile](video-processing.md), including Resolve timestamp
handling. A bare movie-header creation date from a recognizable editor is ignored;
a preserved/restored original capture tag remains usable. Match a video by its
capture start, without deriving a recording interval from edited playback duration.

Poll status/list while an admin inbox is open. `scan.running` describes the
current pass; `scan.result` is the last completed result. Image processing is
synchronous for a manual assignment. When it succeeds, remove the inbox item and
use returned media/Location in the dive gallery. A 409 source_changed means rescan;
filename_conflict needs another destination or a resolved filename collision.
An already committed receipt returns already_assigned for reassignment/deletion:
manage that attachment through the dive media APIs. A queued scan is not completion.

## Persistence and interrupted imports

Schema 12 adds `pending_media`, plus `capture_json` on both owner media tables.
Pending rows have stable IDs, filename/fingerprint, raw capture cache, candidate
explanations, settling time, and a durable assignment receipt (target dive,
reserved media ID, source checksum, resolved capture). The source stays in the
inbox until the destination media, thumbnail and SQL metadata are complete.

Record the receipt before copying. Copy into the assets root's staging directory,
verify the expected checksum, and publish without overwriting any existing name.
Reuse the normal JXL/video conversion and source-cleanup pipelines, but reject processing failures before
publishing an incomplete media row. SQL commits use the receipt's media ID and
capture data. A normal media scan also honors that reserved identity.

After successful publication, verify the destination checksum/thumbnail, retire
only the expected pending bytes, then remove the receipt and private caches.
If newer bytes replaced the pending path, retain those bytes for the next scan
instead of deleting them. Source cleanup or SQL deletion failure leaves the
receipt for recovery. Recover receipts before the startup owner-media scan and
before each inbox scan. An ambiguous commit is resolved by looking up the reserved
ID; retry never creates a second media item. Failed uncommitted copies are removed
only when their checksums match, leaving the pending original intact.

An unfinished receipt may be redirected manually after its uncommitted copy is
cleaned up. Already committed media is not silently moved/deleted by inbox edits.
Directory listing failure never prunes SQL entries. Missing pending files prune
unassigned entries; recorded assignments are recovered before deciding cleanup.
The worker is part of the single serve process; it is not a distributed filesystem
import queue. Deploy serve/sync from the same schema-13-compatible version.

Tests: [metadata](../../internal/media/capture_test.go),
[matching](../../internal/app/pending_match_test.go),
[lifecycle](../../internal/app/pending_test.go),
[admin API](../../internal/app/pending_http_test.go),
[video lifecycle](../../internal/app/pending_video_test.go).
