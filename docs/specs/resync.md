# Administrator activity data resync

[Specification index](../../SPECS.md) · [OpenAPI](../openapi.yaml) · [Frontend flows](frontend.md)

Administrators can refresh Garmin website edits on one stored dive or all stored
dives. Resync is a background job owned by the serving process. It refreshes source
activity/dive documents, imported summaries, notes, parsed rating/comment/highlights,
dates, names and available coordinates/measurements. It keeps local media, captions,
favorites, covers, country overrides and manual trip membership.

FIT files, decoded FIT JSON, cached profile documents, indexed samples, record
metadata and alerts are never downloaded, decoded or rewritten by these jobs.
This applies even when a target has no FIT file. Missing summary measurements keep
previously stored values, including those originally derived from FIT. Structured
note fields are replaced and can be cleared by clearing the Garmin source note.

## Scope and asynchronous behavior

| Action | Endpoint | Result |
| --- | --- | --- |
| Refresh one existing dive | POST `/api/v1/admin/dives/{id}/resync` | 202 job, Location job URL |
| Refresh all existing dives | POST `/api/v1/admin/dives/resync` | 202 job, Location job URL |
| Read availability/current/latest work | GET `/api/v1/admin/resync/status` | Status with nullable active/latest job |
| Read job history | GET `/api/v1/admin/resync/jobs` | Cursor page, newest first |
| Read one job | GET `/api/v1/admin/resync/jobs/{jobID}` | Job progress/outcome |
| Read target results | GET `/api/v1/admin/resync/jobs/{jobID}/items` | Cursor page, IDs ascending, optional status filter |
| Request cancellation | POST `/api/v1/admin/resync/jobs/{jobID}/cancel` | 202 if active when requested, otherwise 200 |

All routes, including GET/HEAD, require an administrator session. Existing
same-origin cookie/CSRF rules apply. Before setup the usual not_configured gate
returns 503. Requests have no JSON body, Garmin credentials or include-FIT option.
Replies use Cache-Control no-store and origin-relative URLs.

Single resync requires a stored dive (404 otherwise). All resync snapshots current
SQL dive IDs during enqueue. It does not list Garmin history, import new dives,
delete Garmin-missing activities or follow later additions to the database.
Regular CLI sync remains responsible for discovery and FIT backfill. The CLI's
existing `-activity-id` behavior is separate and can still backfill missing FIT;
the HTTP resync feature always skips FIT work.

Only one queued/running resync exists at a time, including single-activity jobs.
Another start returns 409 resync_in_progress with Location identifying the active
job. The frontend can adopt that job rather than starting duplicate work. A new
job may be queued after the active one reaches a terminal state. An empty bulk
snapshot succeeds with total=0 and progress_percent=100 without contacting Garmin.

The HTTP enqueue returns after the durable job/target snapshot is committed.
Garmin requests run outside the HTTP request context. Closing a browser tab,
disconnecting, logging out, or canceling the enqueue request after it succeeds
does not cancel the job. Cancellation has its own endpoint. Public dive reads
remain available during paced Garmin requests/cooldowns.

## Progress and frontend flow

On an admin page, fetch status once and poll every **2 seconds** while progress is
visible. Stop polling when leaving the page. `available` means Garmin client
configuration exists on serve; the first request can still fail authentication.
`unavailable_code` explains missing/invalid configuration. History remains readable
even when credentials are absent. `busy` and `active_job` reflect current SQL state;
`latest_job` lets a page reload show the last outcome. Null active/latest fields
mean there is no corresponding job.

After a POST, retain the returned ID/URL and poll that URL. `status` is authoritative:

| Status | Meaning |
| --- | --- |
| queued | Target snapshot saved, awaiting the worker |
| running | Worker preparing its Garmin client or refreshing activities |
| succeeded | Every target saved successfully |
| partial | Some succeeded and others failed or were skipped after a fatal error |
| failed | No successful refresh, or a fatal job/storage error |
| canceled | Explicit cancellation acknowledged; earlier successful commits remain |
| interrupted | Serve stopped/restarted or worker state was interrupted; earlier successful commits remain |

The last five statuses are terminal. Informational phases are queued,
authenticating, fetching, saving and idle. Fetching can include token exchange,
renewal, request pacing and provider cooldown waits; phase does not measure precise
network substeps. `current_activity_id` is present during per-activity work.

Counters:

- total: the original target snapshot size.
- succeeded: committed metadata updates.
- failed: attempted updates with a persisted error.
- completed: succeeded + failed.
- skipped: targets not completed because the job stopped.
- remaining: total − succeeded − failed − skipped.
- progress_percent: 100 × completed / total; an empty successful job is 100.

A canceled/interrupted/fatal job may finish below 100 percent. Do not infer success
from the percentage or from remaining=0. Show status and succeeded/failed/skipped
counts. Item statuses are queued, running, succeeded, failed or skipped. The item
endpoint accepts `status` to show failures separately, default limit 50/max 100;
history defaults to limit 20/max 100. Treat cursors as opaque.

Each successful activity is visible immediately after its own SQL commit; a bulk
job is not all-or-nothing. On completion, invalidate the affected dive detail/list,
rating/year/country filters, trip detail/list/favorite galleries and translation
status. Country lookup and English translation may continue asynchronously after
metadata resync; use their existing status endpoints for those operations.

Cancellation is best effort. It persists a flag and cancels the in-flight request
or rate-limit wait. The worker marks unfinished targets skipped and retains prior
commits. A request can finish just before cancellation is observed. Repeating cancel
on an already terminal job returns 200 without changing its outcome.

## Errors and authentication

Expected safe result codes include garmin_not_found, invalid_garmin_data,
garmin_unavailable, garmin_authentication_failed, garmin_rate_limited, save_failed,
stored_dive_missing, canceled and interrupted. Errors expose no raw Garmin bodies,
URLs, tokens, passwords, API keys or SQL connection strings.

An ordinary per-activity failure keeps the stored activity unchanged and continues
other targets. An authentication failure or persistent 429 after the existing
retries stops the job and skips remaining requests. A database save failure stops
the job. Parent status is partial if earlier targets succeeded, failed otherwise.
The parent can also report activity_errors when individual failures occurred.
Inspect item results for the affected IDs; retry a single ID or start another bulk
job after resolving the cause. There is no destructive removal for Garmin 404.

The application administrator session only authorizes the action. The **serve
container** also needs Garmin access:

See the [settings matrix](settings.md) for equivalent YAML keys and flags;
all three sources configure the same resolved Garmin settings.

| ENV alias | Behavior |
| --- | --- |
| DIVELOG_GARMIN_PERSONAL_API_KEY | Preferred; same official Personal API key mechanism as CLI sync; no email/password needed |
| DIVELOG_GARMIN_RESYNC_TOKEN_FILE | Optional explicit private token-cache path; without a key, reuse this cache only |
| DIVELOG_GARMIN_REQUEST_INTERVAL | Existing paced client setting, default 2s |

With a key and no token path, use
`<user-config-dir>/go-garmindives/resync-personal-tokens.json`, separate from CLI
token caches. Docker resolves this under `/tokens`. ENV/flag keys are held in memory and
never copied into YAML, SQL or token files. An explicitly configured YAML key is
preserved in the private settings file. Token files contain private access tokens.
Without a key or explicit token path, resync is unavailable; normal serving and job
history still work. Invalid effective configuration fails command startup; a valid higher-priority
flag can override invalid ENV values. Missing credentials only disables resync. A cached Personal session eventually needs
its key to renew; a cached legacy session may also expire. Serve never prompts for
credentials or MFA and does not fall back to browser/email/password login.

One paced HTTP client is shared across jobs in the serving process, including token
exchanges and retries. It retains cooldown state across jobs. It does not share its
in-memory pacing state with the separate CLI sync container. No Garmin requests
occur merely from starting serve or polling job status/history.

## Persistence, lifecycle and backend reimplementation

Schema **13** adds resync_jobs and resync_items. Jobs store scope, target selection,
state/phase, cancellation flag, current ID, safe error and timestamps. Target items
store per-activity state/errors/timestamps. Aggregate counters are derived from the
items, not maintained as independently drifting counters. A partial unique index
on active=1 allows only one active job. SQL checks tie active to queued/running.

Enqueue job and target snapshot in one transaction. The worker claims a queued job,
creates its client and requests each target sequentially. Personal API refresh uses
one complete activity document; legacy sessions additionally fetch the activity
document. Validate returned activity ID and supported dive type before mutation.
Do not call history, original-download, profile-download or FIT decode functions.

Fetch before opening a data-save transaction. For an existing targeted dive,
refresh only imported metadata, reconcile country/trip data, and mark the item
succeeded in the **same transaction**. On commit failure, both metadata and result
roll back. Preserve FIT/profile rows and local editorial/relationship columns.
Signal translation refresh after commit. Persist failures separately without
mutating the stored source document.

The deployment supports one serve process, alongside the separate CLI sync
container. On shutdown, cancel work and persist interrupted state with a fresh
bounded cleanup context. On startup, mark leftover active jobs interrupted (or
canceled if already requested), preserve successful items and skip unfinished ones.
Never silently restart network work. A later admin request can start a new job.
History survives restart; there is no automatic retention deletion or periodic
resync cron. Update both serve and sync binaries together for schema 13.

Evidence: [worker/preservation/pacing tests](../../internal/app/resync_test.go),
[API and PostgreSQL tests](../../internal/app/resync_http_test.go),
[migration](../../internal/app/sqlstore_resync_migration.go).
