package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type countryLookupFunc func(context.Context, float64, float64) (string, error)

func (f countryLookupFunc) Lookup(ctx context.Context, lat, lon float64) (string, error) {
	return f(ctx, lat, lon)
}
func disableGeocodingForTest(t *testing.T) { t.Helper(); t.Setenv("GARMIN_GEOCODING_ENABLED", "false") }

func TestGeocodingConfigurationAndProtocol(t *testing.T) {
	t.Setenv("GARMIN_GEOCODING_ENABLED", "true")
	t.Setenv("GARMIN_GEOCODING_URL", "")
	t.Setenv("GARMIN_GEOCODING_INTERVAL", "")
	t.Setenv("GARMIN_GEOCODING_USER_AGENT", "")
	c, err := geocodingConfigFromEnvironment()
	if err != nil || !c.enabled || c.interval != 15*time.Second || c.endpoint != "https://nominatim.openstreetmap.org/reverse" {
		t.Fatalf("geocoding defaults: %+v %v", c, err)
	}
	t.Setenv("GARMIN_GEOCODING_INTERVAL", "1s")
	if _, err := geocodingConfigFromEnvironment(); err == nil {
		t.Fatal("public recurring geocoder allowed excessive pacing")
	}
	t.Setenv("GARMIN_GEOCODING_INTERVAL", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/reverse" || r.URL.Query().Get("lat") != "26.300000" || r.URL.Query().Get("lon") != "34.860000" || r.URL.Query().Get("zoom") != "5" || r.URL.Query().Get("accept-language") != "en" || !strings.Contains(r.Header.Get("User-Agent"), "go-garmindives") || r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected country request: %s", r.URL.String())
		}
		_, _ = w.Write([]byte(`{"address":{"country_code":"eg","country":"Egypt"},"display_name":"Egypt"}`))
	}))
	defer server.Close()
	t.Setenv("GARMIN_GEOCODING_URL", server.URL)
	t.Setenv("GARMIN_GEOCODING_INTERVAL", "1ms")
	c, err = geocodingConfigFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	code, err := newNominatimClient(c).Lookup(context.Background(), 26.3, 34.86)
	if err != nil || code != "EG" {
		t.Fatalf("offshore country result=%s err=%v", code, err)
	}
	for _, origin := range []string{"http://example.com", "https://user:secret@example.com", "https://example.com/path", "https://example.com/?key=secret"} {
		t.Setenv("GARMIN_GEOCODING_URL", origin)
		if _, err := geocodingConfigFromEnvironment(); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("invalid/credential-bearing geocoder URL accepted or logged")
		}
	}
}

func TestGeocoderErrorsAndRedirects(t *testing.T) {
	for _, status := range []int{404, 429, 503, 200} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "75")
				w.WriteHeader(status)
				_, _ = w.Write([]byte("invalid response body"))
			}))
			defer server.Close()
			client := newNominatimClient(geocodingConfig{endpoint: server.URL, userAgent: "test-go-garmindives"})
			code, err := client.Lookup(context.Background(), 26, 34)
			if status == 404 {
				if err != nil || code != "" {
					t.Fatal("offshore no-result should remain unknown")
				}
				return
			}
			var failure *geocodingFailure
			if !errors.As(err, &failure) || !failure.retryable {
				t.Fatalf("unexpected error %v", err)
			}
			if status != 200 && failure.delay != 75*time.Second {
				t.Fatal("Retry-After ignored")
			}
		})
	}
	var leaked atomic.Bool
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { leaked.Store(true) }))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 302) }))
	defer redirect.Close()
	_, err := newNominatimClient(geocodingConfig{endpoint: redirect.URL, userAgent: "test-go-garmindives"}).Lookup(context.Background(), 26, 34)
	if err == nil || leaked.Load() {
		t.Fatal("geocoder followed redirect and disclosed coordinates to another host")
	}
}

func TestGeocoderDistinguishesFrenchTerritories(t *testing.T) {
	for subdivision, want := range map[string]string{"FR-PF": "PF", "FR-NC": "NC", "FR-GP": "GP", "FR-IDF": "FR"} {
		t.Run(subdivision, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprintf(w, `{"address":{"country_code":"fr","country":"France","ISO3166-2-lvl3":%q}}`, subdivision)
			}))
			defer server.Close()
			got, err := newNominatimClient(geocodingConfig{endpoint: server.URL, userAgent: "test-go-garmindives"}).Lookup(context.Background(), 0, 0)
			if err != nil || got != want {
				t.Fatalf("territory %s: got=%s err=%v", subdivision, got, err)
			}
		})
	}
}

func geoDive(t *testing.T, s *sqlStore, id int64) {
	t.Helper()
	record := testSQLDive(t, id)
	location := record.DiveDetails.(map[string]any)["location"].(map[string]any)
	entry := location["entryLoc"].(map[string]any)
	entry["latitude"], entry["longitude"] = 26.3, 34.86
	if err := s.SaveDives(context.Background(), []DiveRecord{record}); err != nil {
		t.Fatal(err)
	}
}

func TestCountryCacheReuseOverridesAndGPSChanges(t *testing.T) {
	s, _ := openTestSQLStore(t)
	geoDive(t, s, 1)
	geoDive(t, s, 2)
	var calls int
	service := newCountryService(s, geocodingConfig{interval: 15 * time.Second}, countryLookupFunc(func(context.Context, float64, float64) (string, error) { calls++; return "EG", nil }), testLogger(t))
	now := time.Now()
	service.now = func() time.Time { return now }
	worked, _, err := service.workOnce(context.Background())
	if err != nil || !worked || calls != 1 {
		t.Fatalf("country lookup: worked=%v calls=%d err=%v", worked, calls, err)
	}
	for _, id := range []int64{1, 2} {
		d := travelSummary(t, s, id)
		if d.Country == nil || d.Country.Code != "EG" || d.CountrySource != "auto" {
			t.Fatalf("cached country not applied: %+v", d)
		}
	}
	geoDive(t, s, 3)
	if d := travelSummary(t, s, 3); d.Country == nil || d.Country.Code != "EG" {
		t.Fatal("new dive did not reuse persistent geocoder cache")
	}
	h := newAPIHandler(s, testLogger(t))
	travelRequest[diveSummary](t, h, "PATCH", "/api/v1/dives/1", map[string]string{"country_code": "French Polynesia"}, 200)
	if err := s.reconcileTravel(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := travelSummary(t, s, 1); d.Country.Code != "PF" || d.DetectedCountry.Code != "EG" {
		t.Fatal("automatic country overwrote a manual override")
	}
	travelRequest[diveSummary](t, h, "PATCH", "/api/v1/dives/1", map[string]any{"country_code": nil}, 200)
	if travelSummary(t, s, 1).Country.Code != "EG" {
		t.Fatal("clearing override did not reuse detected country")
	}
	now = now.Add(time.Minute)
	if worked, _, err := service.workOnce(context.Background()); err != nil || worked || calls != 1 {
		t.Fatal("cached coordinates were requested again")
	}
	if _, err := s.db.Exec(`UPDATE dives SET entry_latitude=0,entry_longitude=0 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcileTravel(context.Background()); err != nil {
		t.Fatal(err)
	}
	if travelSummary(t, s, 1).Country != nil {
		t.Fatal("GPS change reused the country from old coordinates")
	}
	// Zero latitude/longitude are valid coordinates; no result remains unknown.
	service.client = countryLookupFunc(func(_ context.Context, lat, lon float64) (string, error) {
		calls++
		if lat != 0 || lon != 0 {
			t.Error("wrong new coordinates")
		}
		return "", nil
	})
	if worked, _, err := service.workOnce(context.Background()); err != nil || !worked {
		t.Fatalf("new-position lookup: %v", err)
	}
	if travelSummary(t, s, 1).Country != nil {
		t.Fatal("offshore no-result invented a country")
	}
}

func TestCountryRetriesAndDatabaseWidePacing(t *testing.T) {
	s, _ := openTestSQLStore(t)
	geoDive(t, s, 1)
	var calls int
	fail := true
	client := countryLookupFunc(func(context.Context, float64, float64) (string, error) {
		calls++
		if fail {
			return "", &geocodingFailure{code: "geocoding_http_429", retryable: true, delay: time.Minute}
		}
		return "EG", nil
	})
	service := newCountryService(s, geocodingConfig{interval: 15 * time.Second}, client, testLogger(t))
	other := newCountryService(s, service.config, client, nil)
	now := time.Now()
	service.now = func() time.Time { return now }
	other.now = service.now
	if worked, delay, err := service.workOnce(context.Background()); err != nil || !worked || delay < time.Minute {
		t.Fatalf("rate limiting: %v %v %v", worked, delay, err)
	}
	if worked, delay, err := other.workOnce(context.Background()); err != nil || worked || delay <= 0 || calls != 1 {
		t.Fatal("second worker bypassed persisted cooldown")
	}
	now = now.Add(time.Minute)
	fail = false
	if worked, _, err := other.workOnce(context.Background()); err != nil || !worked || calls != 2 {
		t.Fatalf("rate-limit recovery: %v", err)
	}
}

func TestCountryRetryExhaustionAndForbiddenPause(t *testing.T) {
	for _, forbidden := range []bool{false, true} {
		t.Run(fmt.Sprint(forbidden), func(t *testing.T) {
			s, _ := openTestSQLStore(t)
			geoDive(t, s, 1)
			calls := 0
			recover := false
			service := newCountryService(s, geocodingConfig{interval: 15 * time.Second}, countryLookupFunc(func(context.Context, float64, float64) (string, error) {
				calls++
				if recover {
					return "EG", nil
				}
				if forbidden {
					return "", &geocodingFailure{code: "geocoding_http_403"}
				}
				return "", &geocodingFailure{code: "geocoding_http_503", retryable: true}
			}), testLogger(t))
			s.geocoder = service
			now := time.Now()
			service.now = func() time.Time { return now }
			attempts := 5
			if forbidden {
				attempts = 1
			}
			for i := 0; i < attempts; i++ {
				worked, delay, err := service.workOnce(context.Background())
				if err != nil || !worked {
					t.Fatalf("attempt %d: %v", i, err)
				}
				now = now.Add(delay + time.Minute)
			}
			if forbidden {
				geoDive(t, s, 2)
				if _, err := s.db.Exec(`UPDATE dives SET entry_latitude=27 WHERE id=2`); err != nil {
					t.Fatal(err)
				}
				if err := s.reconcileTravel(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if worked, _, err := service.workOnce(context.Background()); err != nil || worked || calls != attempts {
				t.Fatal("failed/blocked country work continued hammering provider")
			}
			recover = true
			h := newAPIHandler(s, testLogger(t))
			travelRequest[map[string]string](t, h, "POST", "/api/v1/countries/retry", nil, 202)
			if worked, _, err := service.workOnce(context.Background()); err != nil || !worked || calls != attempts+1 {
				t.Fatalf("manual country retry failed: %v", err)
			}
		})
	}
}
