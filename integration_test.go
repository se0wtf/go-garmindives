package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	garmin "github.com/se0wtf/go-garminconnect"
)

func TestGarminDiveIntegration(t *testing.T) {
	email, password := os.Getenv("GARMIN_EMAIL"), os.Getenv("GARMIN_PASSWORD")
	if email == "" || password == "" {
		t.Skip("set GARMIN_EMAIL and GARMIN_PASSWORD to run the live integration test")
	}
	directory := t.TempDir()
	tokenPath := filepath.Join(directory, "tokens", "tokens.json")
	mfa := func(context.Context) (string, error) { return os.Getenv("GARMIN_MFA_CODE"), nil }
	client, err := garmin.Login(context.Background(), email, password, mfa, garmin.WithTokenFile(tokenPath))
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
	if err := store.SaveDives(ctx, []DiveRecord{testSQLDive(t, 42)}); err != nil {
		store.Close()
		t.Fatal(err)
	}
	ids, err := store.DiveIDs(ctx)
	dives, hasMore, listErr := store.ListDives(ctx, diveListOptions{Limit: 10, From: "2026-08-01", To: "2026-08-31", Location: "noumea"})
	detail, detailErr := store.Dive(ctx, 42)
	samples, samplesHaveMore, samplesErr := store.DiveSamples(ctx, 42, -1, 10)
	if closeErr := store.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
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
