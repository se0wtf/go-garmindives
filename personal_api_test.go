package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	garmin "github.com/se0wtf/go-garminconnect"
)

func personalDiveFixture(t *testing.T) json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile("testdata/personal-dive.json")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Exercise the published client against an offline Garmin server, including
// authentication, incremental sync, both storage formats, and the REST API.
func TestPersonalAPISync(t *testing.T) {
	for _, kind := range []string{outputYAML, outputSQLite} {
		t.Run(kind, func(t *testing.T) {
			fixture := personalDiveFixture(t)
			fitBody := testFITZIP(t, map[string][]byte{"dive.fit": testFIT(t)})
			const key = "synthetic-personal-key"
			token := "e30." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(time.Hour).Unix()))) + ".signature"
			tokenRequests, listRequests, detailRequests, fitRequests := 0, 0, 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/personalapitoken-service/personal-api/personal-api-key/token" {
					tokenRequests++
					if r.Method != http.MethodPost || r.Header.Get("Authorization") != "ApiKey "+key {
						t.Error("incorrect Personal API key exchange")
					}
					_, _ = io.WriteString(w, token)
					return
				}
				if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("Cookie") != "" {
					t.Error("Personal API data request must use only the bearer token")
				}
				switch r.URL.Path {
				case "/activitylist-service/activities/search/activities":
					listRequests++
					if r.URL.Query().Get("start") != "0" || r.URL.Query().Get("limit") != "100" {
						t.Error("incorrect activity pagination")
					}
					_, _ = io.WriteString(w, `[{"activityId":42,"activityName":"List name","activityType":{"typeKey":"single_gas_diving"}},{"activityId":99,"activityType":{"typeKey":"running"}}]`)
				case "/activity-service/activity/42":
					detailRequests++
					_, _ = w.Write(fixture)
				case "/download-service/files/activity/42":
					fitRequests++
					_, _ = w.Write(fitBody)
				default:
					t.Errorf("unexpected endpoint: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			rateClient, clock := rateTestClient(t, server.Client().Transport)
			directory := t.TempDir()
			legacyPath := filepath.Join(directory, "go-garmindives", "tokens.json")
			if err := garmin.SaveTokens(legacyPath, garmin.Tokens{AccessToken: "legacy-token"}); err != nil {
				t.Fatal(err)
			}
			personalPath := filepath.Join(directory, "go-garmindives", "personal-tokens.json")
			dependencies := appDependencies{
				personalAPIKey:  key,
				configDirectory: func() (string, error) { return directory, nil },
				openDestination: openDestination,
				// Leaving legacy loaders nil proves an API key bypasses them.
				newPersonal: func(gotKey, path string) (diveClient, error) {
					if gotKey != key || path != personalPath {
						t.Fatal("incorrect Personal API configuration")
					}
					return garmin.NewPersonalClient(gotKey, garmin.WithTokenFile(path), garmin.WithBaseURL(server.URL), garmin.WithHTTPClient(rateClient))
				},
			}
			var logs bytes.Buffer
			logger, err := newAppLogger(&logs, "debug")
			if err != nil {
				t.Fatal(err)
			}
			outputPath := filepath.Join(directory, "dives.yaml")
			args := []string{"-output", outputPath}
			if kind == outputSQLite {
				outputPath = filepath.Join(directory, "dives.db")
				args = []string{"-output-type", kind, "-output-dsn", outputPath}
			}
			for i := 0; i < 2; i++ {
				if err := runWithDependencies(context.Background(), args, io.Discard, logger, dependencies); err != nil {
					t.Fatal(err)
				}
			}
			if tokenRequests != 2 || listRequests != 2 || detailRequests != 1 || fitRequests != 1 {
				t.Fatalf("requests: token=%d list=%d detail=%d FIT=%d; want 2/2/1/1", tokenRequests, listRequests, detailRequests, fitRequests)
			}
			if len(clock.waits) != 5 {
				t.Fatalf("authentication, listings, and FIT/summary requests must share one limiter; waits=%v", clock.waits)
			}
			for _, delay := range clock.waits {
				if delay != defaultRequestInterval {
					t.Fatalf("incorrect Garmin request pacing: %v", delay)
				}
			}
			stored, err := garmin.LoadTokens(personalPath)
			if err != nil || !stored.PersonalAPI || stored.AccessToken != token {
				t.Fatalf("invalid Personal API token cache: %v", err)
			}
			legacy, err := garmin.LoadTokens(legacyPath)
			if err != nil || legacy.PersonalAPI || legacy.AccessToken != "legacy-token" {
				t.Fatal("Personal API sync changed the legacy cache")
			}
			serialized, err := os.ReadFile(personalPath)
			if err != nil || strings.Contains(string(serialized), key) || strings.Contains(logs.String(), key) || strings.Contains(logs.String(), token) {
				t.Fatal("Personal API key or token leaked to logs or key persisted")
			}
			if kind == outputYAML {
				archive, existed, err := loadArchive(outputPath)
				if err != nil || !existed || len(archive.Dives) != 1 || archive.Dives[0].ActivityDetails != nil || archive.Dives[0].FIT == nil {
					t.Fatalf("invalid Personal API YAML archive: %v", err)
				}
				want, err := rawDocument(fixture)
				got, gotErr := document(archive.Dives[0].DiveDetails)
				if err != nil || gotErr != nil || !reflect.DeepEqual(got, want) {
					t.Fatal("YAML archive lost Personal API fields")
				}
				return
			}
			store, err := openSQLStore(context.Background(), kind, outputPath)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			api := httptest.NewServer(newAPIHandler(store, testLogger(t)))
			defer api.Close()
			page, _ := getJSON[diveListResponse](t, api.URL+"/api/v1/dives?from=2026-08-01&to=2026-08-31&location=noumea", http.StatusOK)
			if len(page.Items) != 1 {
				t.Fatal("Personal API dive not found by date/location filters")
			}
			dive := page.Items[0]
			if dive.ID != 42 || dive.Name != "Lagoon dive" || dive.ActivityType != "single_gas_diving" || dive.Timezone != "Pacific/Noumea" || dive.LocalDate != "2026-08-15" || dive.Description != "Boat dive" {
				t.Fatalf("incorrect Personal API dive metadata: %#v", dive)
			}
			for name, metric := range map[string]struct {
				got  *float64
				want float64
			}{
				"duration": {dive.DurationSeconds, 3690}, "max depth": {dive.MaxDepthMeters, 22},
				"average depth": {dive.AverageDepthMeters, 15}, "average HR": {dive.AverageHeartRate, 81},
				"max HR": {dive.MaxHeartRate, 112}, "min temperature": {dive.MinTemperatureCelsius, 0},
				"average temperature": {dive.AverageTemperatureCelsius, 25}, "max temperature": {dive.MaxTemperatureCelsius, 26},
				"entry latitude": {dive.EntryLatitude, 0}, "entry longitude": {dive.EntryLongitude, 166.4},
			} {
				if metric.got == nil || *metric.got != metric.want {
					t.Errorf("%s was lost or converted to incorrect units", name)
				}
			}
			if dive.ExitLatitude != nil || dive.ExitLongitude != nil || dive.GarminDiveID != nil || dive.DiveNumber != nil {
				t.Error("unavailable Personal API fields must remain absent")
			}
			detail, _ := getJSON[diveDetail](t, api.URL+"/api/v1/dives/42", http.StatusOK)
			want, _ := rawDocument(fixture)
			got, err := rawDocument(detail.DiveDetails)
			if err != nil || !reflect.DeepEqual(got, want) || string(detail.ActivityDetails) != "null" || len(detail.RecordMetadata) == 0 || len(detail.Alerts) != 1 || string(detail.FIT) == "null" {
				t.Fatal("REST API lost Personal API documents or decoded FIT data")
			}
			samples, _ := getJSON[sampleListResponse](t, api.URL+"/api/v1/dives/42/samples", http.StatusOK)
			if len(samples.Items) != 3 || samples.Items[0].DepthMeters == nil || *samples.Items[0].DepthMeters != 0 || samples.Items[1].DepthMeters == nil || *samples.Items[1].DepthMeters != 1.586 || samples.Items[1].ElapsedMS != 1000 || samples.Items[2].DepthMeters != nil {
				t.Fatal("Personal API FIT profile was not returned correctly")
			}
		})
	}
}

func TestPersonalAPIAuthenticationFailureDoesNotFallBack(t *testing.T) {
	for _, constructorFailure := range []bool{false, true} {
		t.Run(fmt.Sprint(constructorFailure), func(t *testing.T) {
			calls := 0
			dependencies := appDependencies{
				personalAPIKey:  "synthetic-key",
				configDirectory: func() (string, error) { return t.TempDir(), nil },
				openDestination: func(context.Context, outputConfig) (diveDestination, error) {
					return &fakeDestination{archive: newArchive()}, nil
				},
				newPersonal: func(_, path string) (diveClient, error) {
					calls++
					if path != "custom-tokens.json" {
						t.Fatal("explicit token path was ignored")
					}
					if constructorFailure {
						return nil, garmin.ErrSessionExpired
					}
					return &fakeDiveClient{activitiesErr: garmin.ErrSessionExpired}, nil
				},
			}
			err := runWithDependencies(context.Background(), []string{"-token-file", "custom-tokens.json"}, io.Discard, testLogger(t), dependencies)
			if !errors.Is(err, garmin.ErrSessionExpired) || calls != 1 {
				t.Fatalf("calls=%d, err=%v; want one Personal API attempt and no legacy login", calls, err)
			}
		})
	}
}

func TestPersonalAPINullSummaryMeasurements(t *testing.T) {
	store, _ := openTestSQLStore(t)
	record := DiveRecord{ID: 43, Activity: map[string]any{"activityName": "Fallback name"},
		DiveDetails: json.RawMessage(`{"activityId":43,"timeZoneUnitDTO":{"unitKey":"Pacific/Noumea"},"summaryDTO":{"duration":null,"maxDepth":null,"averageDepth":0,"minTemperature":null}}`)}
	if err := store.SaveDives(context.Background(), []DiveRecord{record}); err != nil {
		t.Fatal(err)
	}
	detail, err := store.Dive(context.Background(), 43)
	if err != nil {
		t.Fatal(err)
	}
	dive := detail.Dive
	if dive.Name != "Fallback name" || dive.Timezone != "Pacific/Noumea" || dive.DurationSeconds != nil || dive.MaxDepthMeters != nil || dive.MinTemperatureCelsius != nil || dive.AverageDepthMeters == nil || *dive.AverageDepthMeters != 0 {
		t.Fatalf("null/zero measurements were not preserved: %#v", dive)
	}
}
