package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	garmin "github.com/se0wtf/go-garminconnect"
)

// This test reads one real dive and uses only temporary local output. Secrets
// and account documents are never printed or used as fixtures.
func TestGarminPersonalDiveIntegration(t *testing.T) {
	key := strings.TrimSpace(os.Getenv("GARMIN_PERSONAL_API_KEY"))
	if key == "" {
		t.Skip("set GARMIN_PERSONAL_API_KEY to run the live Personal API integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	directory := t.TempDir()
	httpClient, err := newGarminHTTPClientFromEnvironment(testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	defer httpClient.CloseIdleConnections()
	client, err := garmin.NewPersonalClient(key, garmin.WithTokenFile(filepath.Join(directory, "tokens.json")), garmin.WithHTTPClient(httpClient))
	if err != nil {
		t.Fatal(err)
	}
	dives, err := client.ListActivities(ctx, garmin.ListOptions{Limit: 1, ActivityType: "diving"})
	if err != nil {
		t.Fatal(err)
	}
	if len(dives) == 0 {
		t.Skip("live account has no dives to validate")
	}
	selected := selectedDiveClient{Client: client, activities: dives}
	archive := newArchive()
	added, err := syncDives(ctx, selected, &archive, nil)
	if err != nil || added != 1 || len(archive.Dives) != 1 || archive.Dives[0].ActivityDetails != nil || archive.Dives[0].FIT == nil {
		t.Fatalf("Personal API sync: added=%d, err=%v", added, err)
	}
	path := filepath.Join(directory, "dives.yaml")
	if err := saveArchive(path, archive); err != nil {
		t.Fatal(err)
	}
	loaded, existed, err := loadArchive(path)
	if err != nil || !existed || len(loaded.Dives) != 1 {
		t.Fatalf("Personal API YAML round trip failed: %v", err)
	}
	if added, err := syncDives(ctx, selected, &loaded, nil); err != nil || added != 0 {
		t.Fatalf("second Personal API sync: added=%d, err=%v", added, err)
	}
	store, err := openSQLStore(ctx, outputSQLite, filepath.Join(directory, "dives.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.SaveDives(ctx, loaded.Dives); err != nil {
		t.Fatal(err)
	}
	detail, err := store.Dive(ctx, dives[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	var source personalDiveDocument
	if err := json.Unmarshal(detail.DiveDetails, &source); err != nil {
		t.Fatal(err)
	}
	if source.ID != dives[0].ID || source.Name != detail.Dive.Name || !reflect.DeepEqual(source.Summary.MaxDepth, detail.Dive.MaxDepthMeters) || !reflect.DeepEqual(source.Summary.Duration, detail.Dive.DurationSeconds) {
		t.Fatal("Personal API dive summary was not preserved in SQLite")
	}
	samples, _, err := store.DiveSamples(ctx, dives[0].ID, -1, 10000)
	if err != nil {
		t.Fatal(err)
	}
	withDepth := 0
	for _, sample := range samples {
		if sample.DepthMeters != nil {
			withDepth++
		}
	}
	if withDepth == 0 || string(detail.FIT) == "null" {
		t.Fatal("live dive FIT did not produce a depth-over-time profile")
	}
	t.Logf("verified Personal API FIT decoding, %d depth samples, YAML/SQLite output, and incremental synchronization", withDepth)
}

type selectedDiveClient struct {
	*garmin.Client
	activities []garmin.Activity
}

func (c selectedDiveClient) Activities(context.Context, garmin.ListOptions) ([]garmin.Activity, error) {
	return c.activities, nil
}

func TestGarminDiveIntegration(t *testing.T) {
	email, password := os.Getenv("GARMIN_EMAIL"), os.Getenv("GARMIN_PASSWORD")
	if email == "" || password == "" {
		t.Skip("set GARMIN_EMAIL and GARMIN_PASSWORD to run the live integration test")
	}
	directory := t.TempDir()
	tokenPath := filepath.Join(directory, "tokens", "tokens.json")
	mfa := func(context.Context) (string, error) { return os.Getenv("GARMIN_MFA_CODE"), nil }
	httpClient, err := newGarminHTTPClientFromEnvironment(testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	defer httpClient.CloseIdleConnections()
	client, err := garmin.Login(context.Background(), email, password, mfa, garmin.WithTokenFile(tokenPath), garmin.WithHTTPClient(httpClient))
	if err != nil {
		t.Fatal(err)
	}
	archive := newArchive()
	added, err := syncDives(context.Background(), client, &archive, nil)
	if err != nil {
		t.Fatal(err)
	}
	if added == 0 {
		t.Fatal("live account returned no dives")
	}
	archivePath := filepath.Join(directory, "dives.yaml")
	if err := saveArchive(archivePath, archive); err != nil {
		t.Fatal(err)
	}
	loaded, existed, err := loadArchive(archivePath)
	if err != nil || !existed || len(loaded.Dives) != added {
		t.Fatalf("loaded %d dives, existed=%v, err=%v", len(loaded.Dives), existed, err)
	}
	secondAdded, err := syncDives(context.Background(), client, &loaded, nil)
	if err != nil || secondAdded != 0 {
		t.Fatalf("second sync added %d dives: %v", secondAdded, err)
	}
}

func TestPostgresStoreIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TEST_POSTGRES_DSN to run the PostgreSQL integration test")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	ctx := context.Background()
	if err := admin.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("go_garmindives_test_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Errorf("drop test schema: %v", err)
		}
	}()
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Scheme == "" {
		t.Fatal("TEST_POSTGRES_DSN must be a PostgreSQL connection URL")
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	store, err := openSQLStore(ctx, outputPostgres, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.SaveDives(ctx, []DiveRecord{testSQLDive(t, 42)}); err != nil {
		store.Close()
		t.Fatal(err)
	}
	ids, err := store.DiveIDs(ctx)
	dives, hasMore, listErr := store.ListDives(ctx, diveListOptions{Limit: 10, From: "2026-08-01", To: "2026-08-31", Location: "noumea"})
	detail, detailErr := store.Dive(ctx, 42)
	samples, samplesHaveMore, samplesErr := store.DiveSamples(ctx, 42, -1, 10)
	if err != nil || len(ids) != 1 || ids[0] != 42 {
		t.Fatalf("PostgreSQL IDs = %v, err=%v", ids, err)
	}
	if listErr != nil || hasMore || len(dives) != 1 || dives[0].ID != 42 {
		t.Fatalf("PostgreSQL ListDives = %#v, hasMore=%v, err=%v", dives, hasMore, listErr)
	}
	if detailErr != nil || detail.Dive.ID != 42 || len(detail.RecordMetadata) == 0 {
		t.Fatalf("PostgreSQL Dive = %#v, err=%v", detail, detailErr)
	}
	if samplesErr != nil || samplesHaveMore || len(samples) != 2 {
		t.Fatalf("PostgreSQL DiveSamples = %#v, hasMore=%v, err=%v", samples, samplesHaveMore, samplesErr)
	}
	t.Run("original FIT bytea storage", func(t *testing.T) {
		body := testFIT(t)
		document, err := decodeFITDownload(ctx, body)
		if err != nil {
			t.Fatal(err)
		}
		record := testSQLDive(t, 43)
		record.FIT = document
		if err := store.SaveDives(ctx, []DiveRecord{record, record}); err != nil {
			t.Fatal(err)
		}
		files, err := store.FITFiles(ctx, 43)
		if err != nil || len(files) != 1 || string(files[0].Data) != string(body) {
			t.Fatalf("PostgreSQL original FIT bytes were not preserved: %v", err)
		}
		var storageType string
		if err := store.db.QueryRowContext(ctx, `SELECT pg_typeof(data)::text FROM dive_fit_files WHERE dive_id = 43 AND file_index = 0`).Scan(&storageType); err != nil || storageType != "bytea" {
			t.Fatalf("PostgreSQL FIT storage type = %q, err=%v", storageType, err)
		}
		if _, err := store.DecodeStoredFIT(ctx, 43); err != nil {
			t.Fatalf("PostgreSQL original FIT could not be decoded locally: %v", err)
		}
	})
}

func TestDiveArchiveSQLIntegration(t *testing.T) {
	archivePath := os.Getenv("TEST_DIVE_ARCHIVE")
	if archivePath == "" {
		t.Skip("set TEST_DIVE_ARCHIVE to validate a complete YAML archive against SQLite")
	}
	archive, existed, err := loadArchive(archivePath)
	if err != nil || !existed || len(archive.Dives) == 0 {
		t.Fatalf("load archive: dives=%d, existed=%v, err=%v", len(archive.Dives), existed, err)
	}
	store, err := openSQLStore(context.Background(), outputSQLite, filepath.Join(t.TempDir(), "dives.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.SaveDives(context.Background(), archive.Dives); err != nil {
		t.Fatal(err)
	}
	ids, err := store.DiveIDs(context.Background())
	if err != nil || len(ids) != len(archive.Dives) {
		t.Fatalf("stored IDs=%d, archive dives=%d, err=%v", len(ids), len(archive.Dives), err)
	}
	var samples int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM dive_samples`).Scan(&samples); err != nil || samples == 0 {
		t.Fatalf("stored samples=%d, err=%v", samples, err)
	}
}
