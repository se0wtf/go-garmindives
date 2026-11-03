package app

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	httpx "github.com/se0wtf/go-garmindives/internal/httpx"
)

func seedCountryIssueDives(t *testing.T, store *sqlStore) {
	t.Helper()
	ctx := context.Background()
	var records []DiveRecord
	for id := int64(101); id <= 108; id++ {
		records = append(records, apiTestDive(t, id, "2026-01-02", "Offshore reef"))
	}
	if err := store.SaveDives(ctx, records); err != nil {
		t.Fatal(err)
	}
	tx, err := store.beginTravelTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for id, lat := range map[int64]float64{101: 10, 102: 10, 103: 11, 104: 12, 106: 13, 107: 10, 108: 14} {
		if _, err := tx.ExecContext(ctx, store.bind(`UPDATE dives SET entry_latitude=?,entry_longitude=20,
			exit_latitude=NULL,exit_longitude=NULL,country_code=NULL,country_override=NULL,country_lookup_key=? WHERE id=?`), lat, coordinatesKey(lat, 20), id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE dives SET entry_latitude=NULL,entry_longitude=NULL,exit_latitude=NULL,exit_longitude=NULL,
		country_code=NULL,country_override=NULL,country_lookup_key='' WHERE id=105`); err != nil {
		t.Fatal(err)
	}
	for _, job := range []struct {
		lat      float64
		status   string
		code     any
		error    string
		attempts int
	}{
		{10, "not_found", nil, "", 0},
		{11, "error", nil, "geocoding_http_500", 5},
		{12, "pending", nil, "geocoding_network_error", 1},
		{13, "ready", "EG", "", 0},
	} {
		if _, err := tx.ExecContext(ctx, store.bind(`INSERT INTO country_lookup_cache
			(lookup_key,latitude,longitude,status,country_code,last_error,attempts,updated_at) VALUES(?,?,20,?,?,?,?,?)`),
			coordinatesKey(job.lat, 20), job.lat, job.status, job.code, job.error, job.attempts, int64(1700000000000)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE dives SET country_code='EG' WHERE id=106`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE dives SET country_override='PF' WHERE id=107`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestAdminMissingCountryListRequiresAuthAndSupportsReviewFilters(t *testing.T) {
	a, config := bootstrapApplication(t)
	const path = "/api/v1/admin/dives/missing-country"
	appJSON[httpx.ErrorResponse](t, a, "GET", path, nil, nil, 503)
	setupApplication(t, a, config)
	for _, method := range []string{"GET", "HEAD"} {
		response := appRequest(t, a, method, path, nil, nil)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous %s returned %d", method, response.Code)
		}
	}
	cookie := loginApplication(t, a, testAdminPassword)
	empty := appJSON[missingCountryListResponse](t, a, "GET", path, nil, cookie, 200)
	if empty.Items == nil || len(empty.Items) != 0 || empty.NextCursor != "" {
		t.Fatalf("unexpected empty review list: %+v", empty)
	}
	seedCountryIssueDives(t, waitApplicationReady(t, a).store)
	for status, expected := range map[string][]int64{
		"all":                 {108, 105, 104, 103, 102, 101},
		"not_found":           {102, 101},
		"error":               {103},
		"pending":             {108, 104},
		"missing_coordinates": {105},
	} {
		response := appJSON[missingCountryListResponse](t, a, "GET", path+"?lookup_status="+status, nil, cookie, 200)
		var ids []int64
		for _, item := range response.Items {
			ids = append(ids, item.Dive.ID)
			if item.Dive.Country != nil || item.Dive.CountryOverride != nil || item.Dive.Name == "" || item.Dive.LocalDate != "2026-01-02" {
				t.Fatalf("review row lacks dive context or includes an already corrected dive: %+v", item)
			}
			if status != "all" && item.Lookup.Status != status {
				t.Fatalf("lookup filter %s returned %s", status, item.Lookup.Status)
			}
			if item.Lookup.Status == "missing_coordinates" {
				if item.Lookup.Latitude != nil || item.Lookup.Longitude != nil || item.Lookup.CheckedAtMS != nil {
					t.Fatal("missing GPS was presented as an attempted lookup")
				}
			}
			if item.Lookup.Status == "not_found" && (item.Lookup.Latitude == nil || *item.Lookup.Latitude != 10 || item.Lookup.Longitude == nil || *item.Lookup.Longitude != 20 || item.Lookup.CheckedAtMS == nil || *item.Lookup.CheckedAtMS != 1700000000000) {
				t.Fatalf("missing lookup location/timestamp: %+v", item.Lookup)
			}
			if item.Lookup.Status == "error" && (item.Lookup.ErrorCode != "geocoding_http_500" || item.Lookup.Attempts != 5) {
				t.Fatalf("missing failure details: %+v", item.Lookup)
			}
		}
		if !reflect.DeepEqual(ids, expected) {
			t.Fatalf("%s: IDs=%v want %v", status, ids, expected)
		}
	}
	var paginated []int64
	cursor := ""
	for page := 0; page < 4; page++ {
		response := appJSON[missingCountryListResponse](t, a, "GET", path+"?limit=2&cursor="+cursor, nil, cookie, 200)
		for _, item := range response.Items {
			paginated = append(paginated, item.Dive.ID)
		}
		cursor = response.NextCursor
		if cursor == "" {
			break
		}
	}
	if !reflect.DeepEqual(paginated, []int64{108, 105, 104, 103, 102, 101}) || cursor != "" {
		t.Fatalf("review pagination lost or duplicated dives: %v", paginated)
	}
	for _, query := range []string{"lookup_status=ready", "lookup_status=unknown", "limit=0", "limit=101", "cursor=invalid"} {
		appJSON[httpx.ErrorResponse](t, a, "GET", path+"?"+query, nil, cookie, 400)
	}
	appJSON[map[string]any](t, a, "POST", "/api/v1/auth/logout", nil, cookie, 204)
	appJSON[httpx.ErrorResponse](t, a, "GET", path, nil, cookie, 401)
}

func TestCorrectingMissingCountryRemovesOnlyThatDiveAndRebuildsTrips(t *testing.T) {
	a, config := bootstrapApplication(t)
	setupApplication(t, a, config)
	seedCountryIssueDives(t, waitApplicationReady(t, a).store)
	cookie := loginApplication(t, a, testAdminPassword)
	const path = "/api/v1/admin/dives/missing-country?lookup_status=not_found"
	updated := appJSON[diveSummary](t, a, "PATCH", "/api/v1/dives/101", map[string]string{"country_code": "EG"}, cookie, 200)
	if updated.Country == nil || updated.Country.Code != "EG" || updated.CountrySource != "manual" || updated.TripID == nil {
		t.Fatalf("manual correction did not apply country and travel reconciliation: %+v", updated)
	}
	response := appJSON[missingCountryListResponse](t, a, "GET", path, nil, cookie, 200)
	if len(response.Items) != 1 || response.Items[0].Dive.ID != 102 {
		t.Fatal("correction changed other dives sharing the same unresolved coordinates")
	}
	appJSON[diveSummary](t, a, "PATCH", "/api/v1/dives/101", map[string]any{"country_code": nil}, cookie, 200)
	response = appJSON[missingCountryListResponse](t, a, "GET", path, nil, cookie, 200)
	if len(response.Items) != 2 {
		t.Fatalf("clearing the correction did not restore review membership: %+v", response.Items)
	}
	// The country choice list and ordinary dive reads remain public.
	for _, path := range []string{"/api/v1/countries", "/api/v1/dives", "/api/v1/dives/countries"} {
		if response := appRequest(t, a, "GET", path, nil, nil); response.Code != 200 {
			t.Fatalf("public read %s unexpectedly requires admin: %d", path, response.Code)
		}
	}
}
