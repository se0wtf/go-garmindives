# Product concepts and rules

[Specification index](../../SPECS.md) · [Frontend guide](frontend.md)

## Identity, dates, and measurements

**Dive** means a supported Garmin diving activity. Its `id` is the Garmin activity
ID (a positive signed 64-bit integer), not `garmin_dive_id` and not the diver's
`dive_number`. Asset directories and all dive routes use the activity ID. Keep IDs
opaque in UI code; JSON numbers outside JavaScript's safe-integer range need a
lossless parser or equivalent handling. Do not silently round identifiers.

Each dive has source documents, queryable summary measurements, parsed notes,
profile samples, alerts, media, a country, and optional trip membership. Source
data is retained even when the UI has no widget for it.

`local_date` is a validated `YYYY-MM-DD` extracted from the beginning of the
stored local start time; `year` follows that date. Invalid/missing dates use an
empty local date and `year: null`. Do not convert a date-only value through the
browser's UTC timezone and accidentally move a dive to the previous day/year.
Date filter bounds and residence intervals are inclusive.

Year choices come only from stored dives: `/api/v1/dives/years` returns distinct
numeric years newest first, based on dive local dates across the entire logbook.
Missing dates contribute no choice, and gaps between years remain gaps. The list
is independent of active filters and pagination and updates as imports change
stored dive dates. Use a selected choice with the existing `year` dive filter.

| Measurement | API unit |
| --- | --- |
| Depth / next-stop depth | metres |
| Duration, stop time, NDL, time to surface, air remaining | seconds |
| Sample timestamp / elapsed time | Unix milliseconds / milliseconds |
| Temperature | degrees Celsius |
| Heart rate | beats per minute |
| CNS / N2 load | percent |
| FIT ascent rate | metres per second |
| Pressure SAC / volume SAC and RMV | bar/min / litres/min for FIT-derived data |

Missing optional measurements are absent or null according to OpenAPI. Zero is
a real measurement, not an empty-state test. Legacy summary imports have a known
zero-as-missing limitation; FIT can fill those gaps. Preserve raw fields and FIT
units for measurements outside the standardized profile.

## French notes and ratings

Notes remain intact in `notes` / `description`. Structured fields are derived:

```text
N:4/5
Commentaire:
Enfin pu faire la passe !
HL:
- Requins
Raies aigles
```

This gives `rating: 4`, `rating_max: 5`, `comment: "Enfin pu faire la passe !"`,
and `highlights: ["Requins", "Raies aigles"]`. Labels are case insensitive and may
appear in any order. `Commentaires` is accepted. Decimal comma is accepted.
`N:4` leaves `rating_max` null; never assume five or normalize the score silently.
Malformed ratings are null; zero is valid; no highlights is an empty array.
Ordinary unlabeled prose is retained as raw text, not promoted to `comment`.

Dive lists can filter by one or more exact stored scores using `ratings=4,5`.
Selected scores match with OR, and the rating filter intersects all other filters.
Unrated dives are excluded only when a rating filter is supplied; zero and decimal
scores are valid. Scores keep their imported scale: 8/10 matches rating 8, not 4.
Trip dive lists expose the same filter, and results remain newest first.

Legacy `notes` takes precedence separately for each explicitly present section;
missing sections fall back to `description`. An explicitly empty or invalid
section blocks fallback for that section. The precise grammar is in
[backend algorithms](backend.md#note-parsing).

These fields are imported, not locally editable through the API. An edit on
Garmin needs a targeted sync to update already imported notes.

## Countries and residence

Countries have `{code, name_fr, name_en}`. Codes are uppercase country/territory
codes from the supplied catalog. `FR`, `NC`, and `PF` are distinct destinations.
Country inputs accept a code or either catalog name, ignoring case/accents.
The catalog contains one entry per canonical code and requires nonempty French
and English labels. Deprecated aliases (for example FX/FR or UK/GB) are not
separate choices. Unambiguous legacy code inputs still resolve to their canonical
country; historical codes without a labeled replacement are rejected.
Persist codes; display the requested language's name.

There are two country-choice use cases: `/api/v1/countries` (and setup/countries)
supplies the worldwide residence selector, while `/api/v1/dives/countries`
supplies only countries present in stored dives for the dive filter. The latter
uses effective countries, respects manual overrides, and excludes unknowns. It
does not inherit a current dive page/year/date filter. Both lists use the same
bilingual country representation; fetching them performs no provider lookup.

A dive exposes its detected country, optional manual override, effective country,
and source (`auto`, `manual`, or `unknown`). An override wins without erasing
detected data. Clearing it with `country_code: null` restores detection. GPS lookup
uses valid entry coordinates, otherwise valid exit coordinates. No usable GPS or
no provider result leaves the country unknown. It does not assume the home country.

**Home-country period** records where the diver lived, independently of dive
destinations: country, `from`, and nullable `to`. Dates include both endpoints;
two periods cannot share a date. An open-ended period runs indefinitely. Periods
may have gaps. France until June 30 and New Caledonia from July 1 is valid.
No period covering a dive date means its residence is unknown, not France or the
most recently entered country. Setup requires at least one period, but ongoing
CRUD can later leave gaps or remove all periods.

Changing residence recalculates automatic trips immediately in the same database
transaction. There is no timer waiting for the owner to fill residence history.
Periodic reconciliation also notices changes from the separate sync process.

## Dive trips

A **trip** groups dives and travel editorial content. Its metadata is independent
from Garmin. A dive belongs to at most one trip. Membership has three modes:

| `trip_assignment` | Meaning |
| --- | --- |
| `auto` | Eligible for recalculation; `trip_id` can still be null while waiting |
| `manual` | Explicit membership; automatic grouping must preserve it |
| `excluded` | Explicitly removed from grouping; `trip_id` is null |

Automatic grouping walks dives in ascending `(local_date, start_time, id)` order.
Only foreign dives with a known destination and a matching home period are
eligible. A home-country dive, invalid date, unknown country, or uncovered
residence date breaks the current sequence. Eligible dives split on a country
change or a gap **strictly greater than three calendar months** from the previous
eligible dive. Month ends clamp: January 31 plus three months is April 30.

Exactly three months stays together. December/January can be one trip, named
using its starting year. With France as home, Egypt in March and Egypt in July
form two trips even though their generated names can both be `Égypte 2025` /
`Egypt 2025`. Names are not unique identifiers.

Excluded foreign dives are skipped and do not bridge the time gap. An excluded
home/unknown dive still breaks the sequence because eligibility is checked first.
Eligible manually linked dives participate in sequence boundaries but are never
reassigned automatically. See the detailed stable-ID algorithm in the backend guide.

The first chronologically linked dive supplies a trip's derived year/country when
available; an unknown first-dive country leaves the previous country seed in place.
The first/last linked dives supply start/end dates. Manual trips can span countries
and contain home-country dives. Empty trips have empty derived dates. A manual
trip requires an initial year and country even when `dive_ids` is supplied.

| Trip field | Meaning / editing |
| --- | --- |
| `name`, `name_en` | French/source name and English name; automatic names use country + year |
| `year`, `country` | Starting year and primary country, independently overridable |
| `subtitle`, `description` | French editorial text |
| `rating` | Independent optional score 0–5; no averaging of dive ratings |
| `cruise` | Boolean; default false |
| `operator_name`, `operator_website`, `operator_comment` | Cruise/operator information; retained even if cruise is toggled off |
| `price` | Free text describing approximate per-person cost, without currency arithmetic |
| `cover_media_id`, `cover_photo` | Optional photo owned by this trip |
| `media` / `favorite_dive_media` | Independent attachments / selected attachments of linked dives |

Trip lists support the same exact rating selection as dive lists:
`/api/v1/trips?ratings=4,5` selects trips whose own rating is 4 or 5, excluding
unrated and 4.5-rated trips. Zero and decimals are valid selections. Omit the
selection to include all ratings/unrated trips. Other trip filters intersect
the selection. Trip ratings
remain independent of member-dive ratings, and ordering stays newest first.

`name`, `year`, `country_code` null in PATCH removes the respective override.
`name_en` is only valid with a custom name. Changing the custom French name clears
the previous explicit English name unless a new one is supplied in that request.
With no linked dives, removing an override may retain the previous country/year
as a seed until a first dive supplies derived values.

Manual assignment moves a dive out of any prior trip. Removing it persists an
exclusion; `trip_mode: "auto"` restores automatic eligibility. Replacing
`dive_ids` marks the entire supplied set manual and excludes removed members.
Deleting a trip soft-deletes it and excludes its current dives. It retains dives,
original media, and media rows; the deleted trip and its assets disappear from
active APIs. There is no public undelete operation.

Rebuilds reuse trip IDs, favoring existing customized/media-bearing groups when
previously incomplete groups merge. Empty untouched automatic trips are hidden
unless they own media. Empty manual or edited trips remain visible. A trip's name
and date range can change after a backfill; its ID is the navigation identity.

## Media, covers, favorites, and 360 video

An attachment belongs either to a dive or a trip. Originals live on disk; SQL
stores metadata and editorial fields. The original filename and bytes are kept.
The same name in different owner directories is allowed; the same name within
one owner is a conflict. Attachment IDs survive rescans and replacements of the
same indexed filename. A filesystem rename is a removal plus a new attachment,
so it does not preserve its ID, caption, favorite, or cover selection automatically.

`caption` is short editorial text; `description` explains the scene. A favorite
is a boolean on a dive photo **or video**. Favoriting never copies files. A trip's
favorite gallery changes immediately when dives move in or out. Trip-owned media
cannot be marked favorite. A cover must be an owned **photo**, never a video or
an attachment belonging to a different owner. Deleting/removing a cover photo
clears the selection; no replacement is chosen automatically.

Both photos and videos get JPEG previews. Failure to generate a thumbnail does
not reject an otherwise accepted original. A browser may be unable to decode an
original format that the backend stores; provide a download/unsupported-format
state. The server does not convert uploads to browser-compatible video formats.

`projection` is the saved hint: `auto`, `flat`, `180`, or `360`.
`effective_projection` is the resolved presentation. Automatic detection needs
spherical metadata; width or filename alone is insufficient. An administrator
can set `360` for an already stitched export lacking metadata. Explicit 360
without a known format assumes equirectangular for preview purposes. Stereoscopic
layout is an independent optional hint; ordinary 360 content does not require it.
Raw dual-fisheye footage is not stitched by this application.

## English content

Country names and generated trip names are bilingual locally. Other eligible
French prose is translated asynchronously with DeepL: dive comments/highlights,
media captions/descriptions, trip custom names/subtitles/descriptions/operator
comments/prices. Ordinary dive names, raw notes, original files, ratings, operator
names, and websites are not translated automatically.

Responses retain French fields and add `translations.en` with status `ready`,
`pending`, `error`, or `disabled`. Display English only from the ready block,
falling back to French per field. Cached results can be ready without a provider
key; empty source is ready without a request. Source edits select another cache
revision immediately, so old English must never accompany new French. A missing
translation must not block browsing or editing.
