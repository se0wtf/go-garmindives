package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

const sqlSchemaVersion = 2

type sqlStore struct {
	db      *sql.DB
	dialect string
	path    string
}

type migration struct {
	version    int
	statements []string
}

var migrations = []migration{{
	version: 1,
	statements: []string{
		`CREATE TABLE dives (
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
		)`,
		`CREATE TABLE dive_record_metadata (
			dive_id BIGINT NOT NULL REFERENCES dives(id) ON DELETE CASCADE,
			position INTEGER NOT NULL,
			key TEXT NOT NULL,
			PRIMARY KEY (dive_id, position)
		)`,
		`CREATE TABLE dive_samples (
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
		)`,
		`CREATE INDEX dive_samples_timestamp_idx ON dive_samples (dive_id, timestamp_ms)`,
		`CREATE TABLE dive_alerts (
			dive_id BIGINT NOT NULL REFERENCES dives(id) ON DELETE CASCADE,
			alert_index INTEGER NOT NULL,
			timestamp_ms BIGINT,
			alert_type TEXT NOT NULL,
			data_json TEXT NOT NULL,
			PRIMARY KEY (dive_id, alert_index)
		)`,
	},
}, {
	version: 2,
	statements: []string{
		`ALTER TABLE dives ADD COLUMN local_date TEXT NOT NULL DEFAULT ''`,
		`UPDATE dives SET local_date = SUBSTR(start_time, 1, 10) WHERE start_time <> ''`,
		`CREATE INDEX dives_local_date_start_idx ON dives (local_date DESC, start_time DESC, id DESC)`,
		`CREATE INDEX dives_location_idx ON dives (location)`,
	},
}}

func openSQLStore(ctx context.Context, dialect, endpoint string) (*sqlStore, error) {
	dialect = strings.ToLower(strings.TrimSpace(dialect))
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		if dialect == outputSQLite {
			endpoint = "dives.db"
		} else {
			return nil, errors.New("PostgreSQL output DSN must not be empty")
		}
	}
	driver := "pgx"
	if dialect == outputSQLite {
		driver = "sqlite"
		path, err := sqliteDatabasePath(endpoint)
		if err != nil {
			return nil, err
		}
		if path != "" {
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return nil, fmt.Errorf("create SQLite directory: %w", err)
			}
		}
	} else if dialect != outputPostgres {
		return nil, fmt.Errorf("unsupported SQL dialect %q", dialect)
	}
	db, err := sql.Open(driver, endpoint)
	if err != nil {
		return nil, fmt.Errorf("open %s database: %w", dialect, err)
	}
	store := &sqlStore{db: db, dialect: dialect}
	if dialect == outputSQLite {
		store.path, _ = sqliteDatabasePath(endpoint)
		db.SetMaxOpenConns(1)
		if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
			db.Close()
			return nil, fmt.Errorf("enable SQLite foreign keys: %w", err)
		}
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect to %s database: %w", dialect, err)
	}
	if err := store.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if store.path != "" {
		if err := os.Chmod(store.path, 0o600); err != nil {
			db.Close()
			return nil, fmt.Errorf("secure SQLite database: %w", err)
		}
	}
	return store, nil
}

func sqliteDatabasePath(endpoint string) (string, error) {
	if endpoint == ":memory:" || strings.HasPrefix(endpoint, "file::memory:") {
		return "", nil
	}
	if !strings.HasPrefix(endpoint, "file:") {
		return endpoint, nil
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse SQLite DSN: %w", err)
	}
	path := parsed.Path
	if path == "" {
		path = parsed.Opaque
	}
	if path == "" {
		return "", errors.New("SQLite file DSN has no database path")
	}
	return path, nil
}

func (s *sqlStore) Close() error { return s.db.Close() }

func (s *sqlStore) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		return fmt.Errorf("create migration table: %w", err)
	}
	var current int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if current > sqlSchemaVersion {
		return fmt.Errorf("database schema version %d is newer than supported version %d", current, sqlSchemaVersion)
	}
	for _, migration := range migrations {
		if migration.version <= current {
			continue
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin schema migration %d: %w", migration.version, err)
		}
		for _, statement := range migration.statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				tx.Rollback()
				return fmt.Errorf("apply schema migration %d: %w", migration.version, err)
			}
		}
		if _, err := tx.ExecContext(ctx, s.bind(`INSERT INTO schema_migrations (version) VALUES (?)`), migration.version); err != nil {
			tx.Rollback()
			return fmt.Errorf("record schema migration %d: %w", migration.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit schema migration %d: %w", migration.version, err)
		}
	}
	return nil
}

func (s *sqlStore) bind(query string) string {
	if s.dialect != outputPostgres {
		return query
	}
	var result strings.Builder
	argument := 1
	for _, character := range query {
		if character == '?' {
			result.WriteByte('$')
			result.WriteString(strconv.Itoa(argument))
			argument++
		} else {
			result.WriteRune(character)
		}
	}
	return result.String()
}

func (s *sqlStore) DiveIDs(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM dives ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list stored dives: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("read stored dive ID: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list stored dives: %w", err)
	}
	return ids, nil
}

type storedDive struct {
	ID                  int64
	GarminDiveID        int64
	DiveNumber          int
	Name                string
	ActivityType        string
	DiveType            string
	StartTime           string
	LocalDate           string
	Timezone            string
	Location            string
	EntryLatitude       *float64
	EntryLongitude      *float64
	ExitLatitude        *float64
	ExitLongitude       *float64
	Notes               string
	Description         string
	Duration            *float64
	MaxDepth            *float64
	AverageDepth        *float64
	AverageHeartRate    *float64
	MaxHeartRate        *float64
	MinTemperature      *float64
	AverageTemperature  *float64
	MaxTemperature      *float64
	ActivityJSON        []byte
	ActivityDetailsJSON []byte
	DiveDetailsJSON     []byte
	RecordMetadata      []recordMetadata
	Samples             []diveSample
	Alerts              []diveAlert
}

type recordMetadata struct {
	Position int    `json:"position"`
	Key      string `json:"key"`
}

type diveSample struct {
	Index       int
	TimestampMS int64
	ElapsedMS   int64
	Values      map[string]*float64
	RawJSON     []byte
}

type diveAlert struct {
	Index       int
	TimestampMS *int64
	Type        string
	DataJSON    []byte
}

type diveDetailsDocument struct {
	ID        int64   `json:"id"`
	Number    int     `json:"number"`
	Name      string  `json:"name"`
	DiveType  string  `json:"diveType"`
	StartTime string  `json:"startTime"`
	Timezone  string  `json:"timezone"`
	TotalTime float64 `json:"totalTime"`
	Notes     string  `json:"notes"`
	Location  struct {
		Name     string `json:"location"`
		EntryLoc struct {
			Latitude  float64 `json:"latitude"`
			Longitude float64 `json:"longitude"`
		} `json:"entryLoc"`
		ExitLoc struct {
			Latitude  float64 `json:"latitude"`
			Longitude float64 `json:"longitude"`
		} `json:"exitLoc"`
	} `json:"location"`
	Environment struct {
		MinTemperature float64 `json:"minTemperature"`
		AvgTemperature float64 `json:"avgTemperature"`
		MaxTemperature float64 `json:"maxTemperature"`
	} `json:"environment"`
	Performance struct {
		AvgHR float64 `json:"avgHr"`
		MaxHR float64 `json:"maxHr"`
	} `json:"performance"`
	Profile struct {
		AverageDepth float64 `json:"averageDepth"`
		MaxDepth     float64 `json:"maxDepth"`
	} `json:"profile"`
	Records struct {
		Metadata []recordMetadata    `json:"recordMetaDatas"`
		Data     [][]json.RawMessage `json:"recordData"`
		Alerts   []struct {
			Timestamp json.RawMessage `json:"timestamp"`
			Type      string          `json:"alertType"`
			Data      json.RawMessage `json:"data"`
		} `json:"alerts"`
	} `json:"records"`
}

type activityDocument struct {
	Name         string  `json:"activityName"`
	Description  string  `json:"description"`
	StartTime    string  `json:"startTimeLocal"`
	Duration     float64 `json:"duration"`
	AverageHR    float64 `json:"averageHR"`
	MaxHR        float64 `json:"maxHR"`
	ActivityType struct {
		Key string `json:"typeKey"`
	} `json:"activityType"`
}

func prepareStoredDive(record DiveRecord) (storedDive, error) {
	activityJSON, err := json.Marshal(record.Activity)
	if err != nil {
		return storedDive{}, fmt.Errorf("encode activity: %w", err)
	}
	activityDetailsJSON, err := json.Marshal(record.ActivityDetails)
	if err != nil {
		return storedDive{}, fmt.Errorf("encode activity details: %w", err)
	}
	diveDetailsJSON, err := json.Marshal(record.DiveDetails)
	if err != nil {
		return storedDive{}, fmt.Errorf("encode dive details: %w", err)
	}
	var activity activityDocument
	if err := json.Unmarshal(activityJSON, &activity); err != nil {
		return storedDive{}, fmt.Errorf("decode activity: %w", err)
	}
	var details diveDetailsDocument
	if err := json.Unmarshal(diveDetailsJSON, &details); err != nil {
		return storedDive{}, fmt.Errorf("decode dive details: %w", err)
	}
	result := storedDive{
		ID: record.ID, GarminDiveID: details.ID, DiveNumber: details.Number,
		Name: firstNonEmpty(details.Name, activity.Name), ActivityType: activity.ActivityType.Key, DiveType: details.DiveType,
		StartTime: firstNonEmpty(details.StartTime, activity.StartTime), Timezone: details.Timezone,
		Location: details.Location.Name, EntryLatitude: optionalFloat(details.Location.EntryLoc.Latitude),
		EntryLongitude: optionalFloat(details.Location.EntryLoc.Longitude), ExitLatitude: optionalFloat(details.Location.ExitLoc.Latitude),
		ExitLongitude: optionalFloat(details.Location.ExitLoc.Longitude), Notes: details.Notes, Description: activity.Description,
		ActivityJSON: activityJSON, ActivityDetailsJSON: activityDetailsJSON, DiveDetailsJSON: diveDetailsJSON,
		RecordMetadata: details.Records.Metadata,
	}
	result.LocalDate = diveLocalDate(result.StartTime)
	result.Duration = nonZeroFloat(details.TotalTime, activity.Duration)
	result.MaxDepth = optionalFloat(details.Profile.MaxDepth)
	result.AverageDepth = optionalFloat(details.Profile.AverageDepth)
	result.AverageHeartRate = nonZeroFloat(details.Performance.AvgHR, activity.AverageHR)
	result.MaxHeartRate = nonZeroFloat(details.Performance.MaxHR, activity.MaxHR)
	result.MinTemperature = optionalFloat(details.Environment.MinTemperature)
	result.AverageTemperature = optionalFloat(details.Environment.AvgTemperature)
	result.MaxTemperature = optionalFloat(details.Environment.MaxTemperature)
	positions := make(map[string]int, len(result.RecordMetadata))
	for _, metadata := range result.RecordMetadata {
		positions[strings.ToUpper(metadata.Key)] = metadata.Position
	}
	result.Samples, err = decodeSamples(details.Records.Data, positions)
	if err != nil {
		return storedDive{}, err
	}
	for index, alert := range details.Records.Alerts {
		stored := diveAlert{Index: index, Type: alert.Type, DataJSON: alert.Data}
		if len(stored.DataJSON) == 0 || bytes.Equal(stored.DataJSON, []byte("null")) {
			stored.DataJSON = []byte("[]")
		}
		if value, ok := numericMetric(alert.Timestamp); ok {
			timestamp := int64(math.Round(value))
			stored.TimestampMS = &timestamp
		}
		result.Alerts = append(result.Alerts, stored)
	}
	return result, nil
}

var sampleKeys = []string{
	"HR", "TEMPERATURE", "DEPTH", "NEXT_STOP_DEPTH", "NEXT_STOP_TIME",
	"TIME_TO_SURFACE", "NDL_TIME", "PERCENT_CNS_LOAD", "PERCENT_N2_LOAD",
	"PSAC", "RMV", "VSAC", "AIR_TIME_REMAINING", "PO2", "ASCENT_RATE",
}

func decodeSamples(records [][]json.RawMessage, positions map[string]int) ([]diveSample, error) {
	timestampPosition, found := positions["TIMESTAMP"]
	if !found && len(records) > 0 {
		return nil, errors.New("dive records have no TIMESTAMP metadata")
	}
	result := make([]diveSample, 0, len(records))
	var firstTimestamp int64
	for index, record := range records {
		if timestampPosition < 0 || timestampPosition >= len(record) {
			return nil, fmt.Errorf("dive sample %d has no timestamp value", index)
		}
		timestampValue, ok := numericMetric(record[timestampPosition])
		if !ok {
			return nil, fmt.Errorf("dive sample %d has an invalid timestamp", index)
		}
		timestamp := int64(math.Round(timestampValue))
		if index == 0 {
			firstTimestamp = timestamp
		}
		raw, err := json.Marshal(record)
		if err != nil {
			return nil, fmt.Errorf("encode dive sample %d: %w", index, err)
		}
		sample := diveSample{Index: index, TimestampMS: timestamp, ElapsedMS: timestamp - firstTimestamp, Values: make(map[string]*float64), RawJSON: raw}
		for _, key := range sampleKeys {
			position, found := positions[key]
			if !found || position < 0 || position >= len(record) {
				continue
			}
			if value, ok := numericMetric(record[position]); ok {
				copy := value
				sample.Values[key] = &copy
			}
		}
		result = append(result, sample)
	}
	return result, nil
}

func numericMetric(raw json.RawMessage) (float64, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return 0, false
	}
	if raw[0] == '{' {
		var wrapped struct {
			ParsedValue json.RawMessage `json:"parsedValue"`
			Source      string          `json:"source"`
		}
		if json.Unmarshal(raw, &wrapped) != nil {
			return 0, false
		}
		if len(bytes.TrimSpace(wrapped.ParsedValue)) != 0 && !bytes.Equal(bytes.TrimSpace(wrapped.ParsedValue), []byte("null")) {
			return numericMetric(wrapped.ParsedValue)
		}
		value, err := strconv.ParseFloat(wrapped.Source, 64)
		return value, err == nil
	}
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&number) != nil {
		return 0, false
	}
	value, err := number.Float64()
	return value, err == nil
}

func (s *sqlStore) SaveDives(ctx context.Context, records []DiveRecord) error {
	if len(records) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin saving dives: %w", err)
	}
	defer tx.Rollback()
	for _, record := range records {
		dive, err := prepareStoredDive(record)
		if err != nil {
			return fmt.Errorf("prepare dive %d for SQL: %w", record.ID, err)
		}
		inserted, err := s.insertDive(ctx, tx, dive)
		if err != nil {
			return err
		}
		if !inserted {
			continue
		}
		if err := s.insertDiveChildren(ctx, tx, dive); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit dives: %w", err)
	}
	return nil
}

func (s *sqlStore) insertDive(ctx context.Context, tx *sql.Tx, dive storedDive) (bool, error) {
	query := s.bind(`INSERT INTO dives (
		id, garmin_dive_id, dive_number, name, activity_type, dive_type, start_time, local_date, timezone,
		location, entry_latitude, entry_longitude, exit_latitude, exit_longitude, notes, description, duration_seconds,
		max_depth_meters, average_depth_meters, average_heart_rate, max_heart_rate,
		min_temperature_celsius, average_temperature_celsius, max_temperature_celsius,
		activity_json, activity_details_json, dive_details_json
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT (id) DO NOTHING`)
	result, err := tx.ExecContext(ctx, query,
		dive.ID, nullableInt64(dive.GarminDiveID), nullableInt(dive.DiveNumber), dive.Name, dive.ActivityType, dive.DiveType,
		dive.StartTime, dive.LocalDate, dive.Timezone, dive.Location, dive.EntryLatitude, dive.EntryLongitude, dive.ExitLatitude, dive.ExitLongitude,
		dive.Notes, dive.Description, dive.Duration,
		dive.MaxDepth, dive.AverageDepth, dive.AverageHeartRate, dive.MaxHeartRate,
		dive.MinTemperature, dive.AverageTemperature, dive.MaxTemperature,
		string(dive.ActivityJSON), string(dive.ActivityDetailsJSON), string(dive.DiveDetailsJSON),
	)
	if err != nil {
		return false, fmt.Errorf("insert dive %d: %w", dive.ID, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("check inserted dive %d: %w", dive.ID, err)
	}
	return count > 0, nil
}

func (s *sqlStore) insertDiveChildren(ctx context.Context, tx *sql.Tx, dive storedDive) error {
	metadataQuery := s.bind(`INSERT INTO dive_record_metadata (dive_id, position, key) VALUES (?, ?, ?)`)
	for _, metadata := range dive.RecordMetadata {
		if _, err := tx.ExecContext(ctx, metadataQuery, dive.ID, metadata.Position, metadata.Key); err != nil {
			return fmt.Errorf("insert metadata for dive %d: %w", dive.ID, err)
		}
	}
	if err := s.insertSamples(ctx, tx, dive); err != nil {
		return err
	}
	alertQuery := s.bind(`INSERT INTO dive_alerts (dive_id, alert_index, timestamp_ms, alert_type, data_json) VALUES (?, ?, ?, ?, ?)`)
	for _, alert := range dive.Alerts {
		if _, err := tx.ExecContext(ctx, alertQuery, dive.ID, alert.Index, alert.TimestampMS, alert.Type, string(alert.DataJSON)); err != nil {
			return fmt.Errorf("insert alert %d for dive %d: %w", alert.Index, dive.ID, err)
		}
	}
	return nil
}

const sampleInsertBatchSize = 500

func (s *sqlStore) insertSamples(ctx context.Context, tx *sql.Tx, dive storedDive) error {
	const prefix = `INSERT INTO dive_samples (
		dive_id, sample_index, timestamp_ms, elapsed_ms, heart_rate, temperature_celsius,
		depth_meters, next_stop_depth_meters, next_stop_time_seconds, time_to_surface_seconds,
		ndl_time_seconds, percent_cns_load, percent_n2_load, psac, rmv, vsac,
		air_time_remaining_seconds, po2, ascent_rate, raw_json
	) VALUES `
	const placeholders = `(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	for start := 0; start < len(dive.Samples); start += sampleInsertBatchSize {
		end := min(start+sampleInsertBatchSize, len(dive.Samples))
		var query strings.Builder
		query.WriteString(prefix)
		arguments := make([]any, 0, (end-start)*20)
		for index, sample := range dive.Samples[start:end] {
			if index > 0 {
				query.WriteString(", ")
			}
			query.WriteString(placeholders)
			arguments = append(arguments,
				dive.ID, sample.Index, sample.TimestampMS, sample.ElapsedMS,
				sample.Values["HR"], sample.Values["TEMPERATURE"], sample.Values["DEPTH"],
				sample.Values["NEXT_STOP_DEPTH"], sample.Values["NEXT_STOP_TIME"], sample.Values["TIME_TO_SURFACE"],
				sample.Values["NDL_TIME"], sample.Values["PERCENT_CNS_LOAD"], sample.Values["PERCENT_N2_LOAD"],
				sample.Values["PSAC"], sample.Values["RMV"], sample.Values["VSAC"], sample.Values["AIR_TIME_REMAINING"],
				sample.Values["PO2"], sample.Values["ASCENT_RATE"], string(sample.RawJSON),
			)
		}
		if _, err := tx.ExecContext(ctx, s.bind(query.String()), arguments...); err != nil {
			return fmt.Errorf("insert samples %d-%d for dive %d: %w", start, end-1, dive.ID, err)
		}
	}
	return nil
}

func optionalFloat(value float64) *float64 {
	if value == 0 {
		return nil
	}
	return &value
}

func nonZeroFloat(values ...float64) *float64 {
	for _, value := range values {
		if value != 0 {
			return &value
		}
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func diveLocalDate(startTime string) string {
	if len(startTime) < len("2006-01-02") {
		return ""
	}
	date := startTime[:len("2006-01-02")]
	if _, err := time.Parse("2006-01-02", date); err != nil {
		return ""
	}
	return date
}

func nullableInt64(value int64) any {
	if value == 0 {
		return nil
	}
	return value
}

func nullableInt(value int) any {
	if value == 0 {
		return nil
	}
	return value
}
