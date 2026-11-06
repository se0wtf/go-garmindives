package app

var resyncMigration = migration{version: 13, statements: []string{
	`CREATE TABLE resync_jobs (
		id TEXT PRIMARY KEY, scope TEXT NOT NULL CHECK(scope IN ('activity','all')),
		activity_id BIGINT NOT NULL DEFAULT 0,
		status TEXT NOT NULL CHECK(status IN ('queued','running','succeeded','partial','failed','canceled','interrupted')),
		phase TEXT NOT NULL DEFAULT 'queued', active INTEGER NOT NULL CHECK(active IN (0,1)),
		cancel_requested INTEGER NOT NULL DEFAULT 0, current_activity_id BIGINT NOT NULL DEFAULT 0,
		error_code TEXT NOT NULL DEFAULT '', error_message TEXT NOT NULL DEFAULT '',
		created_at_ms BIGINT NOT NULL, started_at_ms BIGINT NOT NULL DEFAULT 0, completed_at_ms BIGINT NOT NULL DEFAULT 0,
		CHECK(active=CASE WHEN status IN ('queued','running') THEN 1 ELSE 0 END)
	)`,
	`CREATE UNIQUE INDEX resync_jobs_active_idx ON resync_jobs(active) WHERE active=1`,
	`CREATE INDEX resync_jobs_order_idx ON resync_jobs(created_at_ms,id)`,
	`CREATE TABLE resync_items (
		job_id TEXT NOT NULL REFERENCES resync_jobs(id) ON DELETE CASCADE, activity_id BIGINT NOT NULL,
		status TEXT NOT NULL DEFAULT 'queued' CHECK(status IN ('queued','running','succeeded','failed','skipped')),
		error_code TEXT NOT NULL DEFAULT '', error_message TEXT NOT NULL DEFAULT '',
		completed_at_ms BIGINT NOT NULL DEFAULT 0, PRIMARY KEY(job_id,activity_id)
	)`,
}}
