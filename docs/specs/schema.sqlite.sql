-- Reference schema exported from a fresh schema-version-11 database at 54aefed.
-- Documentation snapshot, not an upgrade migration. See storage.md.
PRAGMA foreign_keys = ON;
CREATE TABLE admin_credentials(id INTEGER PRIMARY KEY CHECK(id=1),instance_id TEXT NOT NULL,password_hash TEXT NOT NULL,auth_version BIGINT NOT NULL DEFAULT 1,initialized_at TEXT NOT NULL);

CREATE TABLE admin_sessions(token_hash TEXT PRIMARY KEY,data BLOB NOT NULL,expires_at BIGINT NOT NULL);

CREATE TABLE country_lookup_cache (
		lookup_key TEXT PRIMARY KEY, latitude DOUBLE PRECISION NOT NULL, longitude DOUBLE PRECISION NOT NULL,
		country_code TEXT, status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','ready','not_found','error')),
		attempts INTEGER NOT NULL DEFAULT 0, retry_at BIGINT NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '',
		lease_owner TEXT NOT NULL DEFAULT '', lease_until BIGINT NOT NULL DEFAULT 0, updated_at BIGINT NOT NULL DEFAULT 0
	);

CREATE TABLE country_lookup_pacing (id INTEGER PRIMARY KEY CHECK (id = 1), next_request_at BIGINT NOT NULL DEFAULT 0);

CREATE TABLE dive_alerts (
			dive_id BIGINT NOT NULL REFERENCES dives(id) ON DELETE CASCADE,
			alert_index INTEGER NOT NULL,
			timestamp_ms BIGINT,
			alert_type TEXT NOT NULL,
			data_json TEXT NOT NULL,
			PRIMARY KEY (dive_id, alert_index)
		);

CREATE TABLE dive_fit_files (
		dive_id BIGINT NOT NULL REFERENCES dives(id) ON DELETE CASCADE,
		file_index INTEGER NOT NULL CHECK (file_index >= 0),
		filename TEXT NOT NULL,
		size_bytes BIGINT NOT NULL CHECK (size_bytes > 0 AND size_bytes = length(data)),
		sha256 TEXT NOT NULL CHECK (length(sha256) = 64),
		data BLOB NOT NULL,
		stored_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (dive_id, file_index)
	);

CREATE TABLE dive_media (
		id TEXT PRIMARY KEY,
		dive_id BIGINT NOT NULL REFERENCES dives(id) ON DELETE CASCADE,
		filename TEXT NOT NULL,
		kind TEXT NOT NULL,
		content_type TEXT NOT NULL,
		size_bytes BIGINT NOT NULL,
		modified_ns BIGINT NOT NULL,
		sha256 TEXT NOT NULL,
		width INTEGER NOT NULL DEFAULT 0,
		height INTEGER NOT NULL DEFAULT 0,
		duration_seconds DOUBLE PRECISION NOT NULL DEFAULT 0,
		stereo_layout TEXT NOT NULL DEFAULT 'auto',
		detected_stereo_layout TEXT NOT NULL DEFAULT '',
		projection TEXT NOT NULL DEFAULT 'auto',
		caption TEXT NOT NULL DEFAULT '',
		thumbnail_status TEXT NOT NULL,
		thumbnail_error TEXT NOT NULL DEFAULT '',
		thumbnail_key TEXT NOT NULL DEFAULT '',
		probe_json TEXT NOT NULL DEFAULT '{}',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL, description TEXT NOT NULL DEFAULT '', favorite INTEGER NOT NULL DEFAULT 0,
		UNIQUE (dive_id, filename)
	);

CREATE TABLE dive_record_metadata (
			dive_id BIGINT NOT NULL REFERENCES dives(id) ON DELETE CASCADE,
			position INTEGER NOT NULL,
			key TEXT NOT NULL,
			PRIMARY KEY (dive_id, position)
		);

CREATE TABLE dive_samples (
			dive_id BIGINT NOT NULL REFERENCES dives(id) ON DELETE CASCADE,
			sample_index INTEGER NOT NULL,
			timestamp_ms BIGINT NOT NULL,
			elapsed_ms BIGINT NOT NULL,
			heart_rate DOUBLE PRECISION,
			temperature_celsius DOUBLE PRECISION,
			depth_meters DOUBLE PRECISION,
			next_stop_depth_meters DOUBLE PRECISION,
			next_stop_time_seconds DOUBLE PRECISION,
			time_to_surface_seconds DOUBLE PRECISION,
			ndl_time_seconds DOUBLE PRECISION,
			percent_cns_load DOUBLE PRECISION,
			percent_n2_load DOUBLE PRECISION,
			psac DOUBLE PRECISION,
			rmv DOUBLE PRECISION,
			vsac DOUBLE PRECISION,
			air_time_remaining_seconds DOUBLE PRECISION,
			po2 DOUBLE PRECISION,
			ascent_rate DOUBLE PRECISION,
			raw_json TEXT NOT NULL,
			PRIMARY KEY (dive_id, sample_index)
		);

CREATE TABLE dive_trips (
		id TEXT PRIMARY KEY, name TEXT NOT NULL DEFAULT '', name_en TEXT NOT NULL DEFAULT '',
		name_override INTEGER NOT NULL DEFAULT 0, search_name TEXT NOT NULL DEFAULT '', year INTEGER NOT NULL CHECK (year BETWEEN 1 AND 9999),
		year_override INTEGER NOT NULL DEFAULT 0, country_code TEXT, country_override INTEGER NOT NULL DEFAULT 0,
		subtitle TEXT NOT NULL DEFAULT '', description TEXT NOT NULL DEFAULT '', rating DOUBLE PRECISION CHECK (rating BETWEEN 0 AND 5),
		cruise INTEGER NOT NULL DEFAULT 0, operator_name TEXT NOT NULL DEFAULT '', operator_website TEXT NOT NULL DEFAULT '',
		operator_comment TEXT NOT NULL DEFAULT '', price TEXT NOT NULL DEFAULT '', start_date TEXT NOT NULL DEFAULT '',
		end_date TEXT NOT NULL DEFAULT '', automatic INTEGER NOT NULL DEFAULT 0, edited INTEGER NOT NULL DEFAULT 0,
		deleted INTEGER NOT NULL DEFAULT 0, anchor_dive_id BIGINT REFERENCES dives(id) ON DELETE SET NULL,
		created_at TEXT NOT NULL, updated_at TEXT NOT NULL
	, cover_media_id TEXT REFERENCES trip_media(id) ON DELETE SET NULL);

CREATE TABLE dives (
			id BIGINT PRIMARY KEY,
			garmin_dive_id BIGINT,
			dive_number INTEGER,
			name TEXT NOT NULL,
			activity_type TEXT NOT NULL,
			dive_type TEXT NOT NULL,
			start_time TEXT NOT NULL,
			timezone TEXT NOT NULL,
			location TEXT NOT NULL,
			entry_latitude DOUBLE PRECISION,
			entry_longitude DOUBLE PRECISION,
			exit_latitude DOUBLE PRECISION,
			exit_longitude DOUBLE PRECISION,
			notes TEXT NOT NULL,
			description TEXT NOT NULL,
			duration_seconds DOUBLE PRECISION,
			max_depth_meters DOUBLE PRECISION,
			average_depth_meters DOUBLE PRECISION,
			average_heart_rate DOUBLE PRECISION,
			max_heart_rate DOUBLE PRECISION,
			min_temperature_celsius DOUBLE PRECISION,
			average_temperature_celsius DOUBLE PRECISION,
			max_temperature_celsius DOUBLE PRECISION,
			activity_json TEXT NOT NULL,
			activity_details_json TEXT NOT NULL,
			dive_details_json TEXT NOT NULL,
			downloaded_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		, local_date TEXT NOT NULL DEFAULT '', fit_json TEXT NOT NULL DEFAULT 'null', cover_media_id TEXT REFERENCES dive_media(id) ON DELETE SET NULL, rating DOUBLE PRECISION CHECK (rating >= 0), rating_max DOUBLE PRECISION CHECK (rating_max > 0 AND rating <= rating_max), note_comment TEXT NOT NULL DEFAULT '', highlights_json TEXT NOT NULL DEFAULT '[]', country_code TEXT, country_override TEXT, country_lookup_key TEXT NOT NULL DEFAULT '', trip_id TEXT REFERENCES dive_trips(id) ON DELETE SET NULL, trip_assignment TEXT NOT NULL DEFAULT 'auto' CHECK (trip_assignment IN ('auto', 'manual', 'excluded')));

CREATE TABLE home_countries (
		id TEXT PRIMARY KEY, country_code TEXT NOT NULL, from_date TEXT NOT NULL, to_date TEXT,
		CHECK (to_date IS NULL OR to_date >= from_date)
	);

CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	);

CREATE TABLE translation_cache (
			source_hash TEXT PRIMARY KEY,
			profile TEXT NOT NULL,
			source_json TEXT NOT NULL,
			result_json TEXT NOT NULL DEFAULT '[]',
			status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'ready', 'error')),
			active INTEGER NOT NULL DEFAULT 1,
			attempts INTEGER NOT NULL DEFAULT 0,
			retry_at BIGINT NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			lease_owner TEXT NOT NULL DEFAULT '',
			lease_until BIGINT NOT NULL DEFAULT 0,
			created_at BIGINT NOT NULL,
			updated_at BIGINT NOT NULL
		);

CREATE TABLE trip_media (
		id TEXT PRIMARY KEY, trip_id TEXT NOT NULL REFERENCES dive_trips(id) ON DELETE CASCADE,
		filename TEXT NOT NULL, kind TEXT NOT NULL, content_type TEXT NOT NULL, size_bytes BIGINT NOT NULL,
		modified_ns BIGINT NOT NULL, sha256 TEXT NOT NULL, width INTEGER NOT NULL DEFAULT 0, height INTEGER NOT NULL DEFAULT 0,
		duration_seconds DOUBLE PRECISION NOT NULL DEFAULT 0, stereo_layout TEXT NOT NULL DEFAULT 'auto',
		detected_stereo_layout TEXT NOT NULL DEFAULT '', projection TEXT NOT NULL DEFAULT 'auto', caption TEXT NOT NULL DEFAULT '',
		description TEXT NOT NULL DEFAULT '', thumbnail_status TEXT NOT NULL, thumbnail_error TEXT NOT NULL DEFAULT '',
		thumbnail_key TEXT NOT NULL DEFAULT '', probe_json TEXT NOT NULL DEFAULT '{}', created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
		favorite INTEGER NOT NULL DEFAULT 0, UNIQUE (trip_id, filename)
	);

CREATE INDEX admin_sessions_expiry_idx ON admin_sessions(expires_at);

CREATE INDEX country_lookup_work_idx ON country_lookup_cache (status, retry_at, lease_until);

CREATE INDEX dive_media_dive_idx ON dive_media (dive_id, filename, id);

CREATE INDEX dive_media_favorite_idx ON dive_media (favorite, dive_id, id);

CREATE INDEX dive_samples_timestamp_idx ON dive_samples (dive_id, timestamp_ms);

CREATE INDEX dive_trips_country_year_idx ON dive_trips (country_code, year);

CREATE INDEX dive_trips_dates_idx ON dive_trips (start_date, end_date, id);

CREATE INDEX dives_country_idx ON dives (country_code, country_override, local_date);

CREATE INDEX dives_cover_media_idx ON dives (cover_media_id);

CREATE INDEX dives_local_date_start_idx ON dives (local_date DESC, start_time DESC, id DESC);

CREATE INDEX dives_location_idx ON dives (location);

CREATE INDEX dives_trip_idx ON dives (trip_id, local_date, id);

CREATE INDEX home_countries_dates_idx ON home_countries (from_date, to_date);

CREATE INDEX translation_cache_work_idx ON translation_cache (profile, active, status, retry_at, lease_until);

CREATE INDEX trip_media_trip_idx ON trip_media (trip_id, filename, id);

INSERT INTO country_lookup_pacing(id,next_request_at) VALUES(1,0);
INSERT INTO schema_migrations(version) VALUES(1);
INSERT INTO schema_migrations(version) VALUES(2);
INSERT INTO schema_migrations(version) VALUES(3);
INSERT INTO schema_migrations(version) VALUES(4);
INSERT INTO schema_migrations(version) VALUES(5);
INSERT INTO schema_migrations(version) VALUES(6);
INSERT INTO schema_migrations(version) VALUES(7);
INSERT INTO schema_migrations(version) VALUES(8);
INSERT INTO schema_migrations(version) VALUES(9);
INSERT INTO schema_migrations(version) VALUES(10);
INSERT INTO schema_migrations(version) VALUES(11);
