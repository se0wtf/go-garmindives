package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	charmLog "github.com/charmbracelet/log"
)

func apiTestDive(t *testing.T, id int64, date, location string) DiveRecord {
	t.Helper()
	record := testSQLDive(t, id)
	details := record.DiveDetails.(map[string]any)
	details["id"] = id + 1000
	details["name"] = "Dive " + date
	details["startTime"] = date + "T11:13:00+11:00"
	details["location"].(map[string]any)["location"] = location
	return record
}

func apiTestServer(t *testing.T) (*sqlStore, *httptest.Server) {
	t.Helper()
	store, _ := openTestSQLStore(t)
	records := []DiveRecord{
		apiTestDive(t, 10, "2026-08-10", "Noumea Lagoon"),
		apiTestDive(t, 20, "2026-08-20", "Tahiti West"),
		apiTestDive(t, 30, "2026-09-01", "Noumea Reef"),
	}
	if err := store.SaveDives(context.Background(), records); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(newAPIHandler(store, testLogger(t)))
	t.Cleanup(server.Close)
	return store, server
}

func getJSON[T any](t *testing.T, url string, wantStatus int) (T, http.Header) {
	t.Helper()
	response, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != wantStatus {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("GET %s status = %d, body = %s", url, response.StatusCode, body)
	}
	var value T
	if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value, response.Header
}

func TestAPIHealthAndCORS(t *testing.T) {
	_, server := apiTestServer(t)
	health, headers := getJSON[map[string]string](t, server.URL+"/api/v1/health", http.StatusOK)
	if health["status"] != "ok" || headers.Get("Access-Control-Allow-Origin") != "*" || headers.Get("Cache-Control") != "no-store" {
		t.Fatalf("health = %#v, headers = %#v", health, headers)
	}
	request, err := http.NewRequest(http.MethodOptions, server.URL+"/api/v1/dives", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent || response.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("OPTIONS status = %d, headers = %#v", response.StatusCode, response.Header)
	}
}

func TestAPIListsAndPaginatesDives(t *testing.T) {
	_, server := apiTestServer(t)
	first, _ := getJSON[diveListResponse](t, server.URL+"/api/v1/dives?limit=2", http.StatusOK)
	if len(first.Items) != 2 || first.Items[0].ID != 30 || first.Items[1].ID != 20 || first.NextCursor == "" {
		t.Fatalf("first page = %#v", first)
	}
	second, _ := getJSON[diveListResponse](t, server.URL+"/api/v1/dives?limit=2&cursor="+first.NextCursor, http.StatusOK)
	if len(second.Items) != 1 || second.Items[0].ID != 10 || second.NextCursor != "" {
		t.Fatalf("second page = %#v", second)
	}
}

func TestAPIFiltersDives(t *testing.T) {
	_, server := apiTestServer(t)
	tests := []struct {
		query string
		ids   []int64
	}{
		{"from=2026-08-15&to=2026-08-31", []int64{20}},
		{"location=nouMEA", []int64{30, 10}},
		{"from=2026-08-01&location=tahiti", []int64{20}},
		{"location=noumea%25", nil},
	}
	for _, test := range tests {
		page, _ := getJSON[diveListResponse](t, server.URL+"/api/v1/dives?"+test.query, http.StatusOK)
		var ids []int64
		for _, item := range page.Items {
			ids = append(ids, item.ID)
		}
		if !equalInt64s(ids, test.ids) {
			t.Errorf("query %q IDs = %v, want %v", test.query, ids, test.ids)
		}
	}
}

func equalInt64s(left, right []int64) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func TestAPIReturnsDiveDetailsAndPaginatedSamples(t *testing.T) {
	_, server := apiTestServer(t)
	detail, _ := getJSON[diveDetail](t, server.URL+"/api/v1/dives/20", http.StatusOK)
	if detail.Dive.ID != 20 || detail.Dive.Location != "Tahiti West" || len(detail.RecordMetadata) != 6 || len(detail.Alerts) != 1 {
		t.Fatalf("detail = %#v", detail)
	}
	if !json.Valid(detail.Activity) || !json.Valid(detail.ActivityDetails) || !json.Valid(detail.DiveDetails) {
		t.Fatalf("detail contains invalid source JSON: %#v", detail)
	}
	first, _ := getJSON[sampleListResponse](t, server.URL+"/api/v1/dives/20/samples?limit=1", http.StatusOK)
	if len(first.Items) != 1 || first.Items[0].Index != 0 || first.Items[0].HeartRate == nil || *first.Items[0].HeartRate != 76 || first.NextCursor == "" {
		t.Fatalf("first samples page = %#v", first)
	}
	second, _ := getJSON[sampleListResponse](t, server.URL+"/api/v1/dives/20/samples?limit=1&cursor="+first.NextCursor, http.StatusOK)
	if len(second.Items) != 1 || second.Items[0].Index != 1 || second.Items[0].ElapsedMS != 1000 || second.NextCursor != "" {
		t.Fatalf("second samples page = %#v", second)
	}
}

func TestAPIValidationAndMissingDives(t *testing.T) {
	_, server := apiTestServer(t)
	tests := []struct {
		path   string
		status int
		code   string
	}{
		{"/api/v1/dives?limit=0", http.StatusBadRequest, "invalid_request"},
		{"/api/v1/dives?limit=101", http.StatusBadRequest, "invalid_request"},
		{"/api/v1/dives?from=08-01-2026", http.StatusBadRequest, "invalid_request"},
		{"/api/v1/dives?from=2026-09-01&to=2026-08-01", http.StatusBadRequest, "invalid_request"},
		{"/api/v1/dives?cursor=garbage", http.StatusBadRequest, "invalid_cursor"},
		{"/api/v1/dives/not-a-number", http.StatusBadRequest, "invalid_request"},
		{"/api/v1/dives/999", http.StatusNotFound, "not_found"},
		{"/api/v1/dives/999/samples", http.StatusNotFound, "not_found"},
		{"/api/v1/dives/20/samples?cursor=garbage", http.StatusBadRequest, "invalid_cursor"},
	}
	for _, test := range tests {
		response, _ := getJSON[errorResponse](t, server.URL+test.path, test.status)
		if response.Error.Code != test.code || response.Error.Message == "" {
			t.Errorf("GET %s error = %#v", test.path, response)
		}
	}
	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/dives", bytes.NewReader(nil))
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d", response.StatusCode)
	}
}

func TestAPIHidesInternalDatabaseErrors(t *testing.T) {
	store, server := apiTestServer(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	response, _ := getJSON[errorResponse](t, server.URL+"/api/v1/health", http.StatusInternalServerError)
	if response.Error.Code != "internal_error" || response.Error.Message != "internal server error" {
		t.Fatalf("internal error response = %#v", response)
	}
}

func TestCursorRejectsUnknownFieldsAndTrailingData(t *testing.T) {
	for _, raw := range []string{`{"id":1,"unknown":true}`, `{"id":1}{"id":2}`} {
		cursor := base64.RawURLEncoding.EncodeToString([]byte(raw))
		var decoded diveCursor
		if err := decodeCursor(cursor, &decoded); err == nil {
			t.Fatalf("decodeCursor(%q) unexpectedly succeeded", raw)
		}
	}
}

func TestServeAPIStopsWithContext(t *testing.T) {
	store, _ := openTestSQLStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := serveAPI(ctx, "127.0.0.1:0", store, testLogger(t))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("serveAPI() error = %v", err)
	}
}

func TestAPIListenAddressSafety(t *testing.T) {
	if !isLoopbackAddress("127.0.0.1:8080") || !isLoopbackAddress("[::1]:8080") || !isLoopbackAddress("localhost:8080") {
		t.Fatal("loopback address was not recognized")
	}
	if isLoopbackAddress("0.0.0.0:8080") {
		t.Fatal("public bind was recognized as loopback")
	}
	store, _ := openTestSQLStore(t)
	if err := serveAPI(context.Background(), "", store, testLogger(t)); err == nil {
		t.Fatal("expected empty listen address error")
	}
}

func TestRunServeConfigurationAndHelp(t *testing.T) {
	t.Setenv("GARMIN_ASSETS_DIR", t.TempDir())
	var help bytes.Buffer
	if err := runServe(context.Background(), []string{"-h"}, &help, testLogger(t)); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"GARMIN_OUTPUT_TYPE", "GARMIN_OUTPUT_DSN", "GARMIN_API_ADDR", "-listen"} {
		if !strings.Contains(help.String(), expected) {
			t.Errorf("serve help does not contain %q", expected)
		}
	}
	if err := runServeWithDependencies(context.Background(), []string{"-output-type", "yaml"}, io.Discard, testLogger(t), nil, nil); err == nil {
		t.Fatal("expected YAML rejection")
	}

	store, _ := openTestSQLStore(t)
	var gotKind, gotDSN, gotAddress string
	openStore := func(_ context.Context, kind, dsn string) (*sqlStore, error) {
		gotKind, gotDSN = kind, dsn
		return store, nil
	}
	if err := runServeWithDependencies(context.Background(), []string{"-output-type", "sqlite", "-output-dsn", "custom.db", "-listen", "127.0.0.1:9090"}, io.Discard, testLogger(t), openStore,
		func(_ context.Context, address string, _ *sqlStore, _ *charmLog.Logger) error {
			gotAddress = address
			return nil
		}); err != nil {
		t.Fatal(err)
	}
	if gotKind != outputSQLite || gotDSN != "custom.db" || gotAddress != "127.0.0.1:9090" {
		t.Fatalf("serve configuration = %q, %q, %q", gotKind, gotDSN, gotAddress)
	}
}

func TestRunServeUsesEnvironment(t *testing.T) {
	t.Setenv("GARMIN_ASSETS_DIR", t.TempDir())
	t.Setenv("GARMIN_OUTPUT_TYPE", outputSQLite)
	t.Setenv("GARMIN_OUTPUT_DSN", "environment.db")
	t.Setenv("GARMIN_API_ADDR", "localhost:9191")
	store, _ := openTestSQLStore(t)
	var gotKind, gotDSN, gotAddress string
	err := runServeWithDependencies(context.Background(), nil, io.Discard, testLogger(t),
		func(_ context.Context, kind, dsn string) (*sqlStore, error) {
			gotKind, gotDSN = kind, dsn
			return store, nil
		},
		func(_ context.Context, address string, _ *sqlStore, _ *charmLog.Logger) error {
			gotAddress = address
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if gotKind != outputSQLite || gotDSN != "environment.db" || gotAddress != "localhost:9191" {
		t.Fatalf("environment serve configuration = %q, %q, %q", gotKind, gotDSN, gotAddress)
	}
}
