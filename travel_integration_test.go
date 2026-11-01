package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	charmLog "github.com/charmbracelet/log"
)

func TestTravelMigrationPreservesExistingDiveFITAndMedia(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	s := &sqlStore{db: db, dialect: outputSQLite}
	if _, err := db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY,applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations {
		if migration.version >= 10 {
			break
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, statement := range migration.statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
		}
		if migration.run != nil {
			if err := migration.run(ctx, s, tx); err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version) VALUES(?)`, migration.version); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	record := testSQLDive(t, 42)
	record.FIT, err = decodeFITDownload(ctx, testFIT(t))
	if err != nil {
		t.Fatal(err)
	}
	stored, err := prepareStoredDive(record)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.insertDive(ctx, tx, stored); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := s.insertDiveChildren(ctx, tx, stored); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO dive_media(id,dive_id,filename,kind,content_type,size_bytes,modified_ns,sha256,thumbnail_status,created_at,updated_at,description) VALUES('photo',42,'Requin.jpg','photo','image/jpeg',5,1,'hash','ready','created','updated','Un requin')`); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE dives SET cover_media_id='photo' WHERE id=42`); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var originalFIT string
	if err := db.QueryRow(`SELECT fit_json FROM dives WHERE id=42`).Scan(&originalFIT); err != nil {
		t.Fatal(err)
	}
	files, err := s.FITFiles(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openSQLStore(ctx, outputSQLite, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	detail, err := reopened.Dive(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if string(detail.FIT) != originalFIT || detail.Dive.CoverPhoto == nil || detail.Dive.CoverPhoto.ID != "photo" || len(detail.Media) != 1 || detail.Media[0].Description != "Un requin" || detail.Media[0].Favorite {
		t.Fatal("migration changed existing FIT/media/cover metadata")
	}
	newFiles, err := reopened.FITFiles(ctx, 42)
	if err != nil || !reflect.DeepEqual(files, newFiles) {
		t.Fatal("migration changed original FIT bytes")
	}
	var pending, trips int
	if err := reopened.db.QueryRow(`SELECT COUNT(*) FROM country_lookup_cache`).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("migration did not queue existing coordinates: %d %v", pending, err)
	}
	if err := reopened.db.QueryRow(`SELECT COUNT(*) FROM dive_trips`).Scan(&trips); err != nil || trips != 0 {
		t.Fatal("migration assumed residence and created trips")
	}
	var violations int
	rows, err := reopened.db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		violations++
	}
	rows.Close()
	if violations != 0 {
		t.Fatal("migration broke foreign keys")
	}
}

func TestRunServeBackfillsCountriesAndTripsWithMockProvider(t *testing.T) {
	clearDeepLEnvironment(t)
	s, _ := openTestSQLStore(t)
	geoDive(t, s, 1)
	residenceTest(t, s, "2000-01-01", nil, "FR")
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"address":{"country_code":"eg"}}`))
	}))
	defer provider.Close()
	t.Setenv("GARMIN_GEOCODING_ENABLED", "true")
	t.Setenv("GARMIN_GEOCODING_URL", provider.URL)
	t.Setenv("GARMIN_GEOCODING_INTERVAL", "1ms")
	t.Setenv("GARMIN_GEOCODING_USER_AGENT", "go-garmindives/test")
	err := runServeWithDependencies(context.Background(), configuredServeArgs(t, s, []string{"-output-type", "sqlite", "-assets-dir", t.TempDir()}), io.Discard, testLogger(t), func(context.Context, string, string) (*sqlStore, error) { return s, nil }, func(ctx context.Context, _ string, app *application, _ *charmLog.Logger) error {
		store := waitApplicationReady(t, app).store
		h := newAPIHandler(store, testLogger(t))
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			w := mediaRequest(t, h, "GET", "/api/v1/dives/1", nil, "")
			var d diveDetail
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &d) != nil {
				return errors.New("country startup broke dive reads")
			}
			if d.Dive.Country != nil && d.Dive.Country.Code == "EG" && d.Dive.TripID != nil {
				return nil
			}
			time.Sleep(5 * time.Millisecond)
		}
		return errors.New("startup did not backfill country and trip")
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCountryWorkerCancellationLeavesAPIResponsive(t *testing.T) {
	s, _ := openTestSQLStore(t)
	geoDive(t, s, 1)
	started := make(chan struct{})
	service := newCountryService(s, geocodingConfig{interval: 15 * time.Second}, countryLookupFunc(func(ctx context.Context, _ float64, _ float64) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}), testLogger(t))
	service.Start(context.Background())
	defer service.Close()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("country lookup did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	request := httptest.NewRequest("GET", "/api/v1/dives/1", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	newAPIHandler(s, testLogger(t)).ServeHTTP(w, request)
	if w.Code != 200 {
		t.Fatalf("country lookup blocked API: %d", w.Code)
	}
	service.Close()
	var leases int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM country_lookup_cache WHERE lease_until<>0`).Scan(&leases); err != nil || leases != 0 {
		t.Fatalf("country shutdown leaked a work lease: %d %v", leases, err)
	}
}

func TestTripAndFavoritePagination(t *testing.T) {
	library, _ := mediaTestLibrary(t)
	h := newAPIHandler(library.store, testLogger(t))
	expected := map[string]bool{}
	for _, code := range []string{"EG", "NC", "PF"} {
		trip := createMediaTrip(t, h, code)
		expected[trip.ID] = true
	}
	cursor := ""
	seen := map[string]bool{}
	for page := 0; page < 4; page++ {
		path := "/api/v1/trips?limit=1"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		result := travelRequest[tripListResponse](t, h, "GET", path, nil, 200)
		if len(result.Items) != 1 || !expected[result.Items[0].ID] || seen[result.Items[0].ID] {
			t.Fatalf("bad trip page: %+v", result)
		}
		seen[result.Items[0].ID] = true
		cursor = result.NextCursor
		if cursor == "" {
			break
		}
	}
	if len(seen) != 3 || cursor != "" {
		t.Fatal("trip pagination missed or duplicated items")
	}
	for _, id := range []int64{42, 99} {
		photo, err := library.Upload(context.Background(), id, "Same name.jpg", strings.NewReader("original"), nil)
		if err != nil {
			t.Fatal(err)
		}
		favorite := true
		if _, err := library.Patch(context.Background(), id, photo.ID, mediaPatch{Favorite: &favorite}); err != nil {
			t.Fatal(err)
		}
	}
	first := travelRequest[mediaListResponse](t, h, "GET", "/api/v1/media/favorites?limit=1", nil, 200)
	if len(first.Items) != 1 || first.NextCursor == "" {
		t.Fatal("favorite first page missing cursor")
	}
	second := travelRequest[mediaListResponse](t, h, "GET", "/api/v1/media/favorites?limit=1&cursor="+first.NextCursor, nil, 200)
	if len(second.Items) != 1 || second.NextCursor != "" || first.Items[0].ID == second.Items[0].ID {
		t.Fatal("favorite filename ties broke pagination")
	}
}
