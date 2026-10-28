package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	garmin "github.com/se0wtf/go-garminconnect"
)

func TestFITBlobRoundTripAndOfflineDecode(t *testing.T) {
	ctx := context.Background()
	body := testFIT(t)
	chained := append(append([]byte{}, body...), body...)
	document, err := decodeFITDownload(ctx, testFITZIP(t, map[string][]byte{"original.fit": body, "nested/DIVE.FIT": chained}))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "dives.db")
	store, err := openSQLStore(ctx, outputSQLite, path)
	if err != nil {
		t.Fatal(err)
	}
	record := testSQLDive(t, 42)
	record.FIT = document
	if err := store.SaveDives(ctx, []DiveRecord{record, record}); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openSQLStore(ctx, outputSQLite, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	files, err := store.FITFiles(ctx, 42)
	if err != nil || len(files) != 2 {
		t.Fatalf("FITFiles() returned %d files: %v", len(files), err)
	}
	for i, file := range files {
		original, err := base64.StdEncoding.DecodeString(document.Files[i].OriginalBase64)
		if err != nil {
			t.Fatal(err)
		}
		if file.Index != i || file.Name != document.Files[i].Name || !bytes.Equal(file.Data, original) || file.SizeBytes != int64(len(original)) || file.SHA256 != fmt.Sprintf("%x", sha256.Sum256(original)) {
			t.Errorf("file %d did not preserve its original bytes or metadata", i)
		}
		var storageType string
		if err := store.db.QueryRow(`SELECT typeof(data) FROM dive_fit_files WHERE dive_id = 42 AND file_index = ?`, i).Scan(&storageType); err != nil || storageType != "blob" {
			t.Fatalf("original FIT storage type = %q, err=%v; want a BLOB", storageType, err)
		}
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM dive_fit_files WHERE dive_id = 42`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("duplicate saves changed the FIT file count: %d, %v", count, err)
	}

	// Prove that future decoding and sync decisions do not rely on cached JSON
	// or a Garmin connection once the original files have been retained.
	if _, err := store.db.Exec(`UPDATE dives SET fit_json = 'null' WHERE id = 42`); err != nil {
		t.Fatal(err)
	}
	decoded, err := store.DecodeStoredFIT(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(document)
	got, _ := json.Marshal(decoded)
	if !bytes.Equal(got, want) {
		t.Fatal("re-decoding stored BLOBs did not reproduce all original FIT data")
	}
	profile, err := withFITData(storedDive{}, decoded)
	if err != nil || len(profile.Samples) != 9 {
		t.Fatalf("cached files did not regenerate the profile: samples=%d, err=%v", len(profile.Samples), err)
	}
	destination := &sqlDestination{store: store}
	archive, _, err := destination.Load(ctx)
	if err != nil || len(archive.Dives) != 1 || !archive.Dives[0].fitComplete {
		t.Fatalf("stored BLOBs were not recognized as cached FIT data: %v", err)
	}
	client := &fitDiveClient{fakeDiveClient: &fakeDiveClient{activities: []garmin.Activity{
		{ID: 42, ActivityType: garmin.ActivityType{Key: "single_gas_diving"}},
	}}, err: errors.New("must not download a cached FIT file")}
	if changed, err := syncDives(ctx, client, &archive, nil); err != nil || changed != 0 || len(client.calls) != 0 {
		t.Fatalf("cached FIT file was downloaded again: changed=%d, err=%v", changed, err)
	}
}

type fitMigrationRecord struct {
	ID  int64
	Raw string
}

func seedFITVersionThree(t *testing.T, path string, records []fitMigrationRecord) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:3] {
		for _, statement := range migration.statements {
			if _, err := db.Exec(statement); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations (version) VALUES (?)`, migration.version); err != nil {
			t.Fatal(err)
		}
	}
	for _, record := range records {
		if _, err := db.Exec(`INSERT INTO dives (id, name, activity_type, dive_type, start_time, timezone,
			location, notes, description, activity_json, activity_details_json, dive_details_json, fit_json)
			VALUES (?, 'Retained dive', 'single_gas_diving', '', '', '', '', '', '', '{}', '{}', '{}', ?)`, record.ID, record.Raw); err != nil {
			t.Fatal(err)
		}
	}
}

func migrationFITJSON(t *testing.T, body []byte) string {
	t.Helper()
	// Include an unknown field to verify migration leaves the old JSON intact.
	raw, err := json.Marshal(map[string]any{
		"version": 1, "future_field": uint64(18446744073709551614),
		"files": []map[string]any{{"name": "retained.fit", "original_base64": base64.StdEncoding.EncodeToString(body)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestFITBlobMigrationCopiesExistingBytesWithoutDownloads(t *testing.T) {
	ctx := context.Background()
	body := testFIT(t)
	raw := migrationFITJSON(t, body)
	records := make([]fitMigrationRecord, fitBlobMigrationBatchSize+2)
	for i := range records {
		records[i] = fitMigrationRecord{ID: int64(i + 1), Raw: raw}
	}
	records = append(records, fitMigrationRecord{ID: 999, Raw: "null"})
	path := filepath.Join(t.TempDir(), "old-dives.db")
	seedFITVersionThree(t, path, records)
	store, err := openSQLStore(ctx, outputSQLite, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, record := range records[:len(records)-1] {
		files, err := store.FITFiles(ctx, record.ID)
		if err != nil || len(files) != 1 || !bytes.Equal(files[0].Data, body) {
			t.Fatalf("migration lost the existing FIT for dive %d: %v", record.ID, err)
		}
		var retained string
		if err := store.db.QueryRow(`SELECT fit_json FROM dives WHERE id = ?`, record.ID).Scan(&retained); err != nil || retained != raw {
			t.Fatalf("migration changed existing decoded JSON for dive %d: %v", record.ID, err)
		}
	}
	if files, err := store.FITFiles(ctx, 999); err != nil || len(files) != 0 {
		t.Fatalf("migration fabricated a FIT file for a legacy row: %v", err)
	}
	if err := store.migrate(ctx); err != nil {
		t.Fatalf("migration is not idempotent: %v", err)
	}
	var count, version int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM dive_fit_files`).Scan(&count); err != nil || count != len(records)-1 {
		t.Fatalf("migrated file count = %d, err=%v", count, err)
	}
	if err := store.db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != 4 {
		t.Fatalf("migration version = %d, err=%v", version, err)
	}
}

func TestFITBlobMigrationFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	body := testFIT(t)
	raw := migrationFITJSON(t, body)
	records := make([]fitMigrationRecord, fitBlobMigrationBatchSize+1)
	for i := range records {
		records[i] = fitMigrationRecord{ID: int64(i + 1), Raw: raw}
	}
	records[len(records)-1].Raw = `{"version":1,"files":[{"name":"bad.fit","original_base64":"invalid!"}]}`
	path := filepath.Join(t.TempDir(), "old-dives.db")
	seedFITVersionThree(t, path, records)
	if store, err := openSQLStore(ctx, outputSQLite, path); err == nil {
		store.Close()
		t.Fatal("invalid stored bytes should fail the migration")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var tables, version int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'dive_fit_files'`).Scan(&tables); err != nil || tables != 0 {
		db.Close()
		t.Fatalf("failed migration left a partial file table: %v", err)
	}
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != 3 {
		db.Close()
		t.Fatalf("failed migration advanced the schema version: %v", err)
	}
	if _, err := db.Exec(`UPDATE dives SET fit_json = ? WHERE id = ?`, raw, records[len(records)-1].ID); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	store, err := openSQLStore(ctx, outputSQLite, path)
	if err != nil {
		t.Fatalf("could not retry repaired migration: %v", err)
	}
	defer store.Close()
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM dive_fit_files`).Scan(&count); err != nil || count != len(records) {
		t.Fatalf("repaired migration file count = %d, err=%v", count, err)
	}
}

func TestFITBlobTransactionIntegrityAndCascade(t *testing.T) {
	store, _ := openTestSQLStore(t)
	ctx := context.Background()
	body := testFIT(t)
	document, err := decodeFITDownload(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	record := testSQLDive(t, 42)
	record.FIT = document
	invalid := testSQLDive(t, 43)
	invalid.FIT = &fitDocument{Version: 1, Files: []fitFile{{Name: "bad.fit", OriginalBase64: "invalid!"}}}
	if err := store.SaveDives(ctx, []DiveRecord{record, invalid}); err == nil {
		t.Fatal("invalid original bytes were accepted")
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM dive_fit_files`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed save retained partial FIT BLOBs: count=%d, err=%v", count, err)
	}
	if ids, err := store.DiveIDs(ctx); err != nil || len(ids) != 0 {
		t.Fatalf("failed save retained partial dives: %v", err)
	}
	if err := store.SaveDives(ctx, []DiveRecord{record, testSQLDive(t, 44)}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FITFiles(ctx, 999); !errors.Is(err, errDiveNotFound) {
		t.Fatalf("missing dive error = %v", err)
	}
	if _, err := store.DecodeStoredFIT(ctx, 44); !errors.Is(err, errNoStoredFIT) {
		t.Fatalf("missing original FIT error = %v", err)
	}
	corrupt := append([]byte{}, body...)
	corrupt[0] ^= 1
	if _, err := store.db.Exec(`UPDATE dive_fit_files SET data = ? WHERE dive_id = 42`, corrupt); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FITFiles(ctx, 42); err == nil {
		t.Fatal("corrupt cached FIT bytes passed checksum verification")
	}
	if _, err := store.db.Exec(`DELETE FROM dives WHERE id = 42`); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM dive_fit_files`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("deleted dive left orphaned FIT files: count=%d, err=%v", count, err)
	}
}
