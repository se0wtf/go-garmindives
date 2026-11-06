# Video ingestion and conversion

[Specification index](../../SPECS.md) · [Pending media](pending-media.md) · [OpenAPI](../openapi.yaml)

Video normalization applies to dive and trip uploads, startup/admin filesystem
scans, and assignments from the pending inbox. It is local work using FFmpeg and
FFprobe. It does not contact Garmin and requires no additional service.

## Retained video profile

| Property | Rule |
| --- | --- |
| Container | MP4 (existing compatible MP4/M4V may remain unchanged) |
| Video codec | H.264, 8-bit 4:2:0 (`yuv420p` or full-range `yuvj420p`) |
| Maximum landscape dimensions | Fit inside 1920 × 1080 |
| Maximum portrait dimensions | Fit inside 1080 × 1920 |
| Aspect ratio | Preserve displayed aspect ratio; no crop, stretch, or upscaling; even pixel dimensions |
| Rotation / pixel shape | Apply display rotation and normalize to square pixels when converting |
| Encoding | `libx264`, CRF 23, preset medium, two encoder threads |
| Audio | Retain AAC; convert other audio codecs to AAC at 160 kbit/s per track; retain all audio tracks |
| Frame rate | Preserve source cadence; no frame-rate cap |
| Interlaced sources | Deinterlace using `bwdif=mode=send_frame` |
| HDR PQ/HLG | Tone-map to SDR BT.709 using zscale + Hable tonemapping before 8-bit H.264 encoding |
| Converted output | Streamable fragmented MP4, initialization metadata before media fragments; byte-range/HEAD seeking supported |

A video already in MP4/M4V with one H.264 8-bit 4:2:0 stream, AAC audio (or no audio),
square pixels, no rotation/interlacing/HDR, and dimensions within the limits keeps
its original bytes. No lossy video encoding is done merely to change a container
or audio codec: copy compatible H.264 and normalize only what is needed. An
unchanged or forced rescan does not recompress a compatible retained master.
FFprobe may report full-range samples as `yuvj420p`, including after an encode
requested as `yuv420p`. Both representations are accepted; preserve the range
flag instead of forcing a range conversion merely to change the format name.
Other chroma layouts and 10-bit pixel formats still require normalization.

Keep an MP4/M4V filename and capitalization. For other containers append `.mp4`
to the full original name, e.g. `Shark.mov.mp4`. Long UTF-8 names use the same
238-byte-prefix/12-hex-hash rule as converted photos, with an `.mp4` suffix.
Never overwrite an unrelated destination, including an unindexed filesystem file.
The original filename remains an alias for duplicate checks and interrupted cleanup.

Every retained video gets the existing JPEG thumbnail, normally at most 640 ×
640, or a 640 × 360 perspective for equirectangular 360 footage. `url`, `filename`,
`content_type`, dimensions, size, checksum and `probe` describe the retained master.
IDs, capture data, captions, descriptions, favorites and projection overrides survive
conversion and rescans. Use returned URLs instead of assuming the upload filename.
There is no HLS/DASH manifest or on-demand video transcode on an asset GET.

## Capture time and edited GoPro footage

The inbox accepts the same supported video extensions as owner uploads/scans.
Read capture metadata before converting. It uses the same inclusive
start-minus-10-minute through end-plus-10-minute matching window and ambiguity
rules as photos. A video is matched by its recording/capture **start timestamp**;
its edited playback duration is not interpreted as an original capture interval.
A video assembled from several dives needs an explicit manual choice.

Prefer EXIF/XMP original-capture tags, QuickTime `Keys:CreationDate` or
`UserData:DateTimeOriginal`, including any explicit offset. Without an offset,
use the candidate dive's local timezone, per the application's camera-clock rule.
GPS UTC capture data also remains usable. A bare QuickTime movie-header `CreateDate`
is accepted only when it is nonzero and no recognizable editor is recorded.
This follows the requested local-clock assumption; integer QuickTime timestamps
do not reliably declare whether a camera wrote UTC or its local clock.

Encoder/software strings identifying Resolve, FFmpeg/Lavf, HandBrake, Premiere
or Final Cut prevent using a bare header creation date as the capture time. Such
a date may be the export date. An original/content-capture tag remains usable
after editing. Missing original metadata stays `missing_capture_time`; it can be
assigned manually to any stored dive. Do not infer recording time from filesystem
creation/modification, a filename, timeline timecode, or render duration.

To inspect a source/export pair locally:

```sh
exiftool -G1 -a -s -time:all original.MP4 edited.mp4
```

Restore a known original recording date in a capture tag before dropping an
export into pending, or use the admin assignment endpoint. The backend cannot
reconstruct metadata that an editor discarded or determine a montage's source
clips automatically. Capture provenance is retained in SQL as `MediaItem.capture`.

Pending items expose `kind: photo|video` through the existing list/detail APIs.
Pending video thumbnails are private JPEG decoded-frame previews (at most
640 × 640); the source stays untouched while under review. The private file route
serves the original video, including ranges/HEAD; photo `format` choices do not
transcode videos. Normal assigned 360 thumbnails use the retained projection hints.

## 360 and camera-specific formats

Resize the entire stitched panorama, preserving its aspect ratio. For example,
a 2:1 5760 × 2880 export becomes 1920 × 960 under the landscape limit. It is not
flattened to a perspective video. Spherical projection/texture and stereo hints
are retained in SQL even if the output container drops them. The frontend must
continue using `effective_projection`, `projection_format` and stereo metadata.
An untagged stitched export can be marked `projection: "360"` through media PATCH.

There is no raw GoPro/Insta360 lens stitching. Multiple usable video streams are
rejected for conversion rather than silently dropping a lens/angle; the original
is retained with a processing error. Export a stitched single-stream file first.
Conversion drops subtitle, chapter and data/telemetry streams. Original GPMF data
is not promised in the retained MP4: capture/projection provenance is saved in SQL.

## Publication, recovery and operation

Encode to a hidden staging file. Verify that the output can be probed, conforms
to the profile, has expected dimensions, retains the audio track count, and differs
in duration by no more than max(1 second, 1%). Sync the output and thumbnail, then
recheck source identity/fingerprint/checksum before publication. Record a durable
filesystem/SQL intent before replacing a target. Commit SQL before deleting source
bytes. A same-name conversion uses a recoverable backup of the original.

The existing `.photo-jobs` journal directory also holds video intents, identified
by `kind: "video"`; older photo journals omit kind. Video journals refer to JPEG
thumbnails. On restart/scan, finish a verified committed publication or roll back
the uncommitted output and restore its backup. Only matching source checksums can
be deleted. Changed files and failures preserve the original for retry. Pending
receipts preserve the reserved ID and resolved capture through both phases.

`probe_json.garmindives_video` stores profile `version: 1`, source filename/checksum
when converted, and original projection/format/stereo hints. No extra SQL migration
is needed beyond the schema-12 capture/inbox migration. Existing videos without
the processing marker are revisited on the next startup/admin scan.

Tools receive confined input descriptors; video output uses stdout into an already
opened staging file. No user-controlled shell commands run. Conversions have a
six-hour maximum, bounded diagnostics, and cancellation tied to the request/worker.
Metadata/probe/thumbnail operations retain their two-minute limits. Media writes
are serialized; public SQL reads remain available. Uploads/manual assignments/scans
are synchronous, so configure frontend/proxy timeouts for processing after upload.

The Docker image includes libx264, AAC, zscale/tonemap and the existing photo tools.
Custom FFmpeg builds need those components. A missing codec/filter leaves the
source intact with a processing error; the backend does not publish a damaged HDR
conversion. Back up original footage separately if it must remain available after
normalization; successful conversion retires it like the photo pipeline.

Evidence: [encoder tests](../../internal/media/video_test.go),
[upload/scan/recovery tests](../../internal/app/media_video_test.go),
[pending video tests](../../internal/app/pending_video_test.go).
