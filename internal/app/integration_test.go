package app

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

	appconfig "github.com/se0wtf/go-garmindives/internal/config"
	fitdata "github.com/se0wtf/go-garmindives/internal/fit"
	garminhttp "github.com/se0wtf/go-garmindives/internal/garminhttp"
	httpx "github.com/se0wtf/go-garmindives/internal/httpx"

	// This test reads one real dive and uses only temporary local output. Secrets
	// and account documents are never printed or used as fixtures.

	garmin "github.com/se0wtf/go-garminconnect"
)

func TestGarminPersonalDiveIntegration(t *testing.T) {
	key := strings.TrimSpace(os.Getenv("GARMIN_PERSONAL_API_KEY"))
	if key == "" {
		t.Skip("set GARMIN_PERSONAL_API_KEY to run the live Personal API integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	directory := t.TempDir()
	httpClient, err := garminhttp.FromEnvironment(testLogger(t))
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
	archive := newSyncState()
	added, err := syncDives(ctx, selected, &archive, nil)
	if err != nil || added != 1 || len(archive.Dives) != 1 || archive.Dives[0].ActivityDetails != nil || archive.Dives[0].FIT == nil {
		t.Fatalf("Personal API sync: added=%d, err=%v", added, err)
	}
	loaded := archive

	if added, err := syncDives(ctx, selected, &loaded, nil); err != nil || added != 0 {
		t.Fatalf("second Personal API sync: added=%d, err=%v", added, err)
	}
	store, err := openSQLStore(ctx, appconfig.SQLite, filepath.Join(directory, "dives.db"))
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
	t.Logf("verified Personal API FIT decoding, %d depth samples, SQLite output, and incremental synchronization", withDepth)
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
	httpClient, err := garminhttp.FromEnvironment(testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	defer httpClient.CloseIdleConnections()
	client, err := garmin.Login(context.Background(), email, password, mfa, garmin.WithTokenFile(tokenPath), garmin.WithHTTPClient(httpClient))
	if err != nil {
		t.Fatal(err)
	}
	archive := newSyncState()
	added, err := syncDives(context.Background(), client, &archive, nil)
	if err != nil {
		t.Fatal(err)
	}
	if added == 0 {
		t.Fatal("live account returned no dives")
	}
	store, err := openSQLStore(context.Background(), appconfig.SQLite, filepath.Join(directory, "dives.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.SaveDives(context.Background(), archive.Dives); err != nil {
		t.Fatal(err)
	}
	state, err := store.DiveSyncState(context.Background())
	if err != nil || len(state) != added {
		t.Fatalf("SQL sync state: %v", err)
	}
	loaded := newSyncState()
	loaded.Dives = state

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
	store, err := openSQLStore(ctx, appconfig.Postgres, parsed.String())
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
		document, err := fitdata.DecodeDownload(ctx, body)
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
	t.Run("setup and administrator sessions", func(t *testing.T) {
		clearApplicationEnvironment(t)
		directory := t.TempDir()
		file := appconfig.Default()
		file.Database = appconfig.Database{Type: appconfig.Postgres, URI: parsed.String()}
		file.Media.Directory = filepath.Join(directory, "assets")
		c, err := appconfig.Resolve(filepath.Join(directory, "config.yaml"), file)
		if err != nil {
			t.Fatal(err)
		}
		a := newApplication(ctx, c, testLogger(t), openSQLStore)
		defer a.Close()
		setupApplication(t, a, c)
		cookie := loginApplication(t, a, testAdminPassword)
		appJSON[configurationResponse](t, a, "GET", "/api/v1/config", nil, cookie, 200)
		var storageType, tokenHash string
		if err := store.db.QueryRowContext(ctx, `SELECT pg_typeof(data)::text,token_hash FROM admin_sessions`).Scan(&storageType, &tokenHash); err != nil || storageType != "bytea" || tokenHash != sessionDigest(cookie.Value) {
			t.Fatalf("PostgreSQL session persistence: type=%s err=%v", storageType, err)
		}
		periods := appJSON[struct {
			Items []homeCountry `json:"items"`
		}](t, a, "GET", "/api/v1/home-countries", nil, nil, 200)
		if len(periods.Items) != 1 || periods.Items[0].Country.Code != "FR" || periods.Items[0].To != nil {
			t.Fatal("PostgreSQL setup lost open-ended residence period")
		}
		appJSON[authSessionResponse](t, a, "POST", "/api/v1/auth/password", map[string]string{"current_password": testAdminPassword, "new_password": "Another wonderful coral garden!"}, cookie, 200)
		appJSON[httpx.ErrorResponse](t, a, "GET", "/api/v1/config", nil, cookie, 401)
		loginApplication(t, a, "Another wonderful coral garden!")
		if _, err := store.Dive(ctx, 42); err != nil {
			t.Fatalf("PostgreSQL setup changed existing dive: %v", err)
		}
	})
	t.Run("effective dive country choices", func(t *testing.T) {
		if _, err := store.db.ExecContext(ctx, `UPDATE dives SET country_code='FR',country_override='EG' WHERE id=42`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.ExecContext(ctx, `UPDATE dives SET country_override='FX' WHERE id=43`); err != nil {
			t.Fatal(err)
		}
		choices, err := store.DiveCountries(ctx)
		if err != nil || len(choices) != 2 || choices[0].Code != "EG" || choices[1].Code != "FR" {
			t.Fatalf("PostgreSQL country choices: %+v, %v", choices, err)
		}
		dives, _, err := store.ListDives(ctx, diveListOptions{Country: "FR", Limit: 10})
		if err != nil || len(dives) != 1 || dives[0].ID != 43 {
			t.Fatalf("PostgreSQL canonical country filter: %+v, %v", dives, err)
		}
	})
	t.Run("missing country review", func(t *testing.T) {
		if err := store.SaveDives(ctx, []DiveRecord{testSQLDive(t, 44)}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.ExecContext(ctx, `UPDATE dives SET country_code=NULL,country_override=NULL,country_lookup_key='review-test' WHERE id=44`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.ExecContext(ctx, `INSERT INTO country_lookup_cache(lookup_key,latitude,longitude,status,updated_at) VALUES('review-test',10,20,'not_found',1700000000000)`); err != nil {
			t.Fatal(err)
		}
		items, more, err := store.DivesMissingCountry(ctx, missingCountryOptions{Limit: 1, LookupStatus: "not_found"})
		if err != nil || more || len(items) != 1 || items[0].Dive.ID != 44 || items[0].Lookup.Status != "not_found" || items[0].Lookup.Latitude == nil || *items[0].Lookup.Latitude != 10 {
			t.Fatalf("PostgreSQL missing-country query: %+v, more=%v, err=%v", items, more, err)
		}
	})
}
