package main

var travelMigration = migration{version: 10, statements: []string{
	`CREATE TABLE home_countries (
		id TEXT PRIMARY KEY, country_code TEXT NOT NULL, from_date TEXT NOT NULL, to_date TEXT,
		CHECK (to_date IS NULL OR to_date >= from_date)
	)`,
	`CREATE INDEX home_countries_dates_idx ON home_countries (from_date, to_date)`,
	`CREATE TABLE dive_trips (
		id TEXT PRIMARY KEY, name TEXT NOT NULL DEFAULT '', name_en TEXT NOT NULL DEFAULT '',
		name_override INTEGER NOT NULL DEFAULT 0, search_name TEXT NOT NULL DEFAULT '', year INTEGER NOT NULL CHECK (year BETWEEN 1 AND 9999),
		year_override INTEGER NOT NULL DEFAULT 0, country_code TEXT, country_override INTEGER NOT NULL DEFAULT 0,
		subtitle TEXT NOT NULL DEFAULT '', description TEXT NOT NULL DEFAULT '', rating DOUBLE PRECISION CHECK (rating BETWEEN 0 AND 5),
		cruise INTEGER NOT NULL DEFAULT 0, operator_name TEXT NOT NULL DEFAULT '', operator_website TEXT NOT NULL DEFAULT '',
		operator_comment TEXT NOT NULL DEFAULT '', price TEXT NOT NULL DEFAULT '', start_date TEXT NOT NULL DEFAULT '',
		end_date TEXT NOT NULL DEFAULT '', automatic INTEGER NOT NULL DEFAULT 0, edited INTEGER NOT NULL DEFAULT 0,
		deleted INTEGER NOT NULL DEFAULT 0, anchor_dive_id BIGINT REFERENCES dives(id) ON DELETE SET NULL,
		created_at TEXT NOT NULL, updated_at TEXT NOT NULL
	)`,
	`CREATE INDEX dive_trips_dates_idx ON dive_trips (start_date, end_date, id)`,
	`CREATE INDEX dive_trips_country_year_idx ON dive_trips (country_code, year)`,
	`CREATE TABLE trip_media (
		id TEXT PRIMARY KEY, trip_id TEXT NOT NULL REFERENCES dive_trips(id) ON DELETE CASCADE,
		filename TEXT NOT NULL, kind TEXT NOT NULL, content_type TEXT NOT NULL, size_bytes BIGINT NOT NULL,
		modified_ns BIGINT NOT NULL, sha256 TEXT NOT NULL, width INTEGER NOT NULL DEFAULT 0, height INTEGER NOT NULL DEFAULT 0,
		duration_seconds DOUBLE PRECISION NOT NULL DEFAULT 0, stereo_layout TEXT NOT NULL DEFAULT 'auto',
		detected_stereo_layout TEXT NOT NULL DEFAULT '', projection TEXT NOT NULL DEFAULT 'auto', caption TEXT NOT NULL DEFAULT '',
		description TEXT NOT NULL DEFAULT '', thumbnail_status TEXT NOT NULL, thumbnail_error TEXT NOT NULL DEFAULT '',
		thumbnail_key TEXT NOT NULL DEFAULT '', probe_json TEXT NOT NULL DEFAULT '{}', created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
		favorite INTEGER NOT NULL DEFAULT 0, UNIQUE (trip_id, filename)
	)`,
	`CREATE INDEX trip_media_trip_idx ON trip_media (trip_id, filename, id)`,
	`ALTER TABLE dive_trips ADD COLUMN cover_media_id TEXT REFERENCES trip_media(id) ON DELETE SET NULL`,
	`ALTER TABLE dives ADD COLUMN country_code TEXT`,
	`ALTER TABLE dives ADD COLUMN country_override TEXT`,
	`ALTER TABLE dives ADD COLUMN country_lookup_key TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE dives ADD COLUMN trip_id TEXT REFERENCES dive_trips(id) ON DELETE SET NULL`,
	`ALTER TABLE dives ADD COLUMN trip_assignment TEXT NOT NULL DEFAULT 'auto' CHECK (trip_assignment IN ('auto', 'manual', 'excluded'))`,
	`CREATE INDEX dives_country_idx ON dives (country_code, country_override, local_date)`,
	`CREATE INDEX dives_trip_idx ON dives (trip_id, local_date, id)`,
	`ALTER TABLE dive_media ADD COLUMN favorite INTEGER NOT NULL DEFAULT 0`,
	`CREATE INDEX dive_media_favorite_idx ON dive_media (favorite, dive_id, id)`,
	`CREATE TABLE country_lookup_cache (
		lookup_key TEXT PRIMARY KEY, latitude DOUBLE PRECISION NOT NULL, longitude DOUBLE PRECISION NOT NULL,
		country_code TEXT, status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','ready','not_found','error')),
		attempts INTEGER NOT NULL DEFAULT 0, retry_at BIGINT NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '',
		lease_owner TEXT NOT NULL DEFAULT '', lease_until BIGINT NOT NULL DEFAULT 0, updated_at BIGINT NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX country_lookup_work_idx ON country_lookup_cache (status, retry_at, lease_until)`,
	`CREATE TABLE country_lookup_pacing (id INTEGER PRIMARY KEY CHECK (id = 1), next_request_at BIGINT NOT NULL DEFAULT 0)`,
	`INSERT INTO country_lookup_pacing (id) VALUES (1)`,
}, run: reconcileCountriesTx}
