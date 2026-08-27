package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func testSQLDive(t *testing.T, id int64) DiveRecord {
	t.Helper()
	details, err := rawDocument(json.RawMessage(`{
		"id":314159,"number":232,"name":"Lagoon","diveType":"SINGLE_GAS","startTime":"2026-08-15T11:13:00+11:00","timezone":"Pacific/Noumea","totalTime":61.5,"notes":"Clear water",
		"location":{"location":"Noumea","entryLoc":{"latitude":-22.1,"longitude":166.4},"exitLoc":{"latitude":-22.2,"longitude":166.5}},
		"environment":{"minTemperature":24,"avgTemperature":25,"maxTemperature":26},
		"performance":{"avgHr":81,"maxHr":112},"profile":{"averageDepth":15,"maxDepth":22},
		"records":{
			"recordMetaDatas":[
				{"position":0,"key":"TIMESTAMP"},{"position":1,"key":"HR"},{"position":2,"key":"TEMPERATURE"},
				{"position":3,"key":"DEPTH"},{"position":4,"key":"PO2"},{"position":5,"key":"NEW_GARMIN_METRIC"}
			],
			"recordData":[
				[{"source":"1.78675283E12","parsedValue":1786752830000},{"source":"76.0","parsedValue":76},25,1.586,0.41,99],
				[1786752831000,77,24,2.5,{"source":"0.5"},100]
			],
			"alerts":[{"alertType":"MAX_DEPTH","timestamp":1786752830500,"data":["22.0"]}]
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	return DiveRecord{
		ID: id,
		Activity: map[string]any{
			"activityName": "Fallback name", "description": "Boat dive", "duration": 60.0, "averageHR": 80.0, "maxHR": 110.0,
			"activityType": map[string]any{"typeKey": "single_gas_diving"},
		},
		ActivityDetails: map[string]any{"activityId": id},
		DiveDetails:     details,
	}
}

func openTestSQLStore(t *testing.T) (*sqlStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "private", "dives.db")
	store, err := openSQLStore(context.Background(), outputSQLite, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, path
}

func TestSQLiteStoreMigratesAndSavesDiveRows(t *testing.T) {
	store, path := openTestSQLStore(t)
	ctx := context.Background()
	if err := store.SaveDives(ctx, []DiveRecord{testSQLDive(t, 42)}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveDives(ctx, []DiveRecord{testSQLDive(t, 42)}); err != nil {
		t.Fatal(err)
	}
	ids, err := store.DiveIDs(ctx)
	if err != nil || !reflect.DeepEqual(ids, []int64{42}) {
		t.Fatalf("DiveIDs() = %v, %v", ids, err)
	}
	for table, want := range map[string]int{"dives": 1, "dive_record_metadata": 6, "dive_samples": 2, "dive_alerts": 1, "schema_migrations": 1} {
		var count int
		if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil || count != want {
			t.Fatalf("%s count = %d, err=%v, want %d", table, count, err, want)
		}
	}
	var timestamp, elapsed int64
	var heartRate, temperature, depth, po2 float64
	var raw string
	if err := store.db.QueryRowContext(ctx, `SELECT timestamp_ms, elapsed_ms, heart_rate, temperature_celsius, depth_meters, po2, raw_json
		FROM dive_samples WHERE dive_id = 42 AND sample_index = 1`).Scan(&timestamp, &elapsed, &heartRate, &temperature, &depth, &po2, &raw); err != nil {
		t.Fatal(err)
	}
	if timestamp != 1786752831000 || elapsed != 1000 || heartRate != 77 || temperature != 24 || depth != 2.5 || po2 != 0.5 || !json.Valid([]byte(raw)) {
		t.Fatalf("sample = %d %d %v %v %v %v %q", timestamp, elapsed, heartRate, temperature, depth, po2, raw)
	}
	var garminDiveID int64
	var diveNumber int
	var name, activityType, diveType, timezone, location, notes, description string
	if err := store.db.QueryRowContext(ctx, `SELECT garmin_dive_id, dive_number, name, activity_type, dive_type, timezone, location, notes, description
		FROM dives WHERE id = 42`).Scan(&garminDiveID, &diveNumber, &name, &activityType, &diveType, &timezone, &location, &notes, &description); err != nil {
		t.Fatal(err)
	}
	if garminDiveID != 314159 || diveNumber != 232 || name != "Lagoon" || activityType != "single_gas_diving" || diveType != "SINGLE_GAS" || timezone != "Pacific/Noumea" || location != "Noumea" || notes != "Clear water" || description != "Boat dive" {
		t.Fatalf("dive columns = %d %d %q %q %q %q %q %q %q", garminDiveID, diveNumber, name, activityType, diveType, timezone, location, notes, description)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("SQLite mode = %v, err=%v", info.Mode().Perm(), err)
	}
}

func TestSQLiteStoreSaveIsTransactional(t *testing.T) {
	store, _ := openTestSQLStore(t)
	invalid := testSQLDive(t, 2)
	invalid.DiveDetails = map[string]any{
		"records": map[string]any{
			"recordMetaDatas": []any{map[string]any{"position": 0, "key": "HR"}},
			"recordData":      []any{[]any{80}},
		},
	}
	err := store.SaveDives(context.Background(), []DiveRecord{testSQLDive(t, 1), invalid})
	if err == nil || !strings.Contains(err.Error(), "TIMESTAMP") {
		t.Fatalf("SaveDives() error = %v", err)
	}
	ids, queryErr := store.DiveIDs(context.Background())
	if queryErr != nil || len(ids) != 0 {
		t.Fatalf("transaction left IDs %v: %v", ids, queryErr)
	}
}

func TestSQLiteStoreBatchesLargeProfiles(t *testing.T) {
	store, _ := openTestSQLStore(t)
	dive, err := prepareStoredDive(testSQLDive(t, 77))
	if err != nil {
		t.Fatal(err)
	}
	template := dive.Samples[0]
	dive.Samples = make([]diveSample, sampleInsertBatchSize+1)
	for index := range dive.Samples {
		sample := template
		sample.Index = index
		sample.TimestampMS += int64(index * 1000)
		sample.ElapsedMS = int64(index * 1000)
		dive.Samples[index] = sample
	}
	tx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	inserted, err := store.insertDive(context.Background(), tx, dive)
	if err != nil || !inserted {
		tx.Rollback()
		t.Fatalf("insertDive() = %v, %v", inserted, err)
	}
	if err := store.insertDiveChildren(context.Background(), tx, dive); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM dive_samples WHERE dive_id = 77`).Scan(&count); err != nil || count != sampleInsertBatchSize+1 {
		t.Fatalf("sample count = %d, err=%v", count, err)
	}
}

func TestSQLiteStoreRejectsNewerSchema(t *testing.T) {
	store, _ := openTestSQLStore(t)
	if _, err := store.db.Exec(`INSERT INTO schema_migrations (version) VALUES (?)`, sqlSchemaVersion+1); err != nil {
		t.Fatal(err)
	}
	if err := store.migrate(context.Background()); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("migrate() error = %v", err)
	}
}

func TestSQLiteMigrationIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dives.db")
	first, err := openSQLStore(context.Background(), outputSQLite, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.SaveDives(context.Background(), []DiveRecord{testSQLDive(t, 42)}); err != nil {
		first.Close()
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := openSQLStore(context.Background(), outputSQLite, path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	ids, err := second.DiveIDs(context.Background())
	if err != nil || !reflect.DeepEqual(ids, []int64{42}) {
		t.Fatalf("reopened IDs = %v, %v", ids, err)
	}
}

func TestDecodeSamplesMapsEveryKnownMetric(t *testing.T) {
	positions := map[string]int{"TIMESTAMP": 0}
	record := make([]json.RawMessage, len(sampleKeys)+1)
	record[0] = json.RawMessage(`1000`)
	for index, key := range sampleKeys {
		positions[key] = index + 1
		record[index+1] = json.RawMessage(strconv.Itoa(index + 1))
	}
	samples, err := decodeSamples([][]json.RawMessage{record}, positions)
	if err != nil || len(samples) != 1 {
		t.Fatalf("decodeSamples() = %#v, %v", samples, err)
	}
	for index, key := range sampleKeys {
		value := samples[0].Values[key]
		if value == nil || *value != float64(index+1) {
			t.Errorf("metric %s = %v", key, value)
		}
	}
}

func TestSQLDestinationLoadsIDsAndSavesOnlyNewRecords(t *testing.T) {
	store, _ := openTestSQLStore(t)
	destination := &sqlDestination{store: store}
	archive := newArchive()
	archive.Dives = append(archive.Dives, testSQLDive(t, 42))
	if err := destination.Save(context.Background(), archive, 1); err != nil {
		t.Fatal(err)
	}
	loaded, existed, err := destination.Load(context.Background())
	if err != nil || !existed || len(loaded.Dives) != 1 || loaded.Dives[0].ID != 42 || loaded.Dives[0].DiveDetails != nil {
		t.Fatalf("Load() = %#v, existed=%v, err=%v", loaded, existed, err)
	}
	if err := destination.Save(context.Background(), loaded, 2); err == nil {
		t.Fatal("expected invalid added count")
	}
}

func TestPostgresPlaceholders(t *testing.T) {
	store := &sqlStore{dialect: outputPostgres}
	if got, want := store.bind("VALUES (?, ?, ?)"), "VALUES ($1, $2, $3)"; got != want {
		t.Fatalf("bind() = %q, want %q", got, want)
	}
	store.dialect = outputSQLite
	if got := store.bind("VALUES (?)"); got != "VALUES (?)" {
		t.Fatalf("SQLite bind() = %q", got)
	}
}

func TestSQLiteDatabasePath(t *testing.T) {
	tests := []struct {
		dsn, want string
	}{
		{"dives.db", "dives.db"},
		{"/tmp/dives.db", "/tmp/dives.db"},
		{"file:/tmp/dives.db?mode=rwc", "/tmp/dives.db"},
		{"file:dives.db?mode=rwc", "dives.db"},
		{":memory:", ""},
		{"file::memory:?cache=shared", ""},
	}
	for _, test := range tests {
		got, err := sqliteDatabasePath(test.dsn)
		if err != nil || got != test.want {
			t.Errorf("sqliteDatabasePath(%q) = %q, %v; want %q", test.dsn, got, err, test.want)
		}
	}
}

func TestNumericMetric(t *testing.T) {
	tests := []struct {
		raw  string
		want float64
		ok   bool
	}{
		{`12.5`, 12.5, true},
		{`{"source":"1.2E3"}`, 1200, true},
		{`{"source":"bad","parsedValue":42}`, 42, true},
		{`null`, 0, false},
		{`"text"`, 0, false},
	}
	for _, test := range tests {
		got, ok := numericMetric(json.RawMessage(test.raw))
		if got != test.want || ok != test.ok {
			t.Errorf("numericMetric(%s) = %v, %v", test.raw, got, ok)
		}
	}
}

func TestOpenDestinationValidation(t *testing.T) {
	if _, err := openDestination(context.Background(), outputConfig{Type: "unknown"}); err == nil {
		t.Fatal("expected unsupported output error")
	}
	if _, err := openDestination(context.Background(), outputConfig{Type: outputYAML}); err == nil {
		t.Fatal("expected missing YAML path error")
	}
	if _, err := openSQLStore(context.Background(), outputPostgres, ""); err == nil {
		t.Fatal("expected missing PostgreSQL DSN error")
	}
}

func TestSQLStoreClosedDatabaseErrors(t *testing.T) {
	store, _ := openTestSQLStore(t)
	if err := store.db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DiveIDs(context.Background()); err == nil {
		t.Fatal("expected closed database error")
	}
}
