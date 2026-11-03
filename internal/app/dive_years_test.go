package app

import (
	"context"
	"reflect"
	"strconv"
	"testing"

	httpx "github.com/se0wtf/go-garmindives/internal/httpx"
)

func TestDiveYearChoicesUseWholeLogbookAndLocalDates(t *testing.T) {
	a, config := bootstrapApplication(t)
	const path = "/api/v1/dives/years"
	gate := appJSON[httpx.ErrorResponse](t, a, "GET", path, nil, nil, 503)
	if gate.Error.Code != "not_configured" {
		t.Fatal("year choices bypassed initial setup")
	}
	setupApplication(t, a, config)
	empty := appJSON[diveYearsResponse](t, a, "GET", path, nil, nil, 200)
	if empty.Items == nil || len(empty.Items) != 0 {
		t.Fatalf("empty logbook must return items: [], got %+v", empty)
	}
	store := waitApplicationReady(t, a).store
	var records []DiveRecord
	// An old year must remain discoverable even beyond the first dive-list page.
	for id := int64(1); id <= 55; id++ {
		records = append(records, apiTestDive(t, id, "2026-06-01", "Reef"))
	}
	for _, fixture := range []struct {
		id    int64
		start string
	}{
		{56, "2025-01-01T00:30:00+14:00"}, // UTC is still 2024.
		{57, "2023-12-31T23:30:00-10:00"}, // UTC is already 2024.
		{58, "2019-06-01T12:00:00+02:00"},
		{59, ""},
		{60, "invalid-date"},
		{61, "0000-01-01T12:00:00Z"},
	} {
		record := apiTestDive(t, fixture.id, "2026-06-01", "Reef")
		record.DiveDetails.(map[string]any)["startTime"] = fixture.start
		records = append(records, record)
	}
	if err := store.SaveDives(context.Background(), records); err != nil {
		t.Fatal(err)
	}
	want := []int{2026, 2025, 2023, 2019}
	for _, query := range []string{"", "?year=2026&country=EG&ratings=4,5&from=2026-01-01&limit=1&cursor=ignored"} {
		response := appJSON[diveYearsResponse](t, a, "GET", path+query, nil, nil, 200)
		if !reflect.DeepEqual(response.Items, want) {
			t.Fatalf("year choices %q: got %v, want %v", query, response.Items, want)
		}
	}
	page := appJSON[diveListResponse](t, a, "GET", "/api/v1/dives?limit=50", nil, nil, 200)
	if len(page.Items) != 50 || page.NextCursor == "" {
		t.Fatal("fixture does not extend beyond the first page")
	}
	for _, year := range want {
		page := appJSON[diveListResponse](t, a, "GET", "/api/v1/dives?year="+strconv.Itoa(year), nil, nil, 200)
		if len(page.Items) == 0 {
			t.Fatalf("offered year %d has no matching dives", year)
		}
		for _, dive := range page.Items {
			if dive.Year == nil || *dive.Year != year {
				t.Fatalf("year selection %d returned dive %d from %v", year, dive.ID, dive.Year)
			}
		}
	}
	// New imports become choices on the next read, with no restart or manual scan.
	if err := store.SaveDives(context.Background(), []DiveRecord{apiTestDive(t, 62, "2027-01-01", "Reef")}); err != nil {
		t.Fatal(err)
	}
	response := appJSON[diveYearsResponse](t, a, "GET", path, nil, nil, 200)
	if !reflect.DeepEqual(response.Items, append([]int{2027}, want...)) {
		t.Fatalf("new import did not update year choices: %v", response.Items)
	}
}
