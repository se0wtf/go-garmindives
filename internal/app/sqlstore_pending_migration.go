package app

var pendingMigration = migration{version: 12, statements: []string{
	`ALTER TABLE dive_media ADD COLUMN capture_json TEXT NOT NULL DEFAULT '{}'`,
	`ALTER TABLE trip_media ADD COLUMN capture_json TEXT NOT NULL DEFAULT '{}'`,
	`CREATE TABLE pending_media (
		id TEXT PRIMARY KEY, filename TEXT NOT NULL UNIQUE, size_bytes BIGINT NOT NULL,
		modified_ns BIGINT NOT NULL, sha256 TEXT NOT NULL DEFAULT '',
		capture_json TEXT NOT NULL DEFAULT '{}', metadata_ready INTEGER NOT NULL DEFAULT 0,
		reason TEXT NOT NULL DEFAULT 'waiting_for_file', last_error TEXT NOT NULL DEFAULT '',
		candidates_json TEXT NOT NULL DEFAULT '[]', stable_since BIGINT NOT NULL,
		state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','assigning')),
		target_dive_id BIGINT NOT NULL DEFAULT 0, media_id TEXT NOT NULL DEFAULT '',
		transfer_sha256 TEXT NOT NULL DEFAULT '', transfer_capture_json TEXT NOT NULL DEFAULT '{}',
		created_at TEXT NOT NULL, updated_at TEXT NOT NULL
	)`,
	`CREATE INDEX pending_media_order_idx ON pending_media (filename,id)`,
	`CREATE UNIQUE INDEX pending_media_transfer_idx ON pending_media (target_dive_id,filename) WHERE state='assigning'`,
}}
