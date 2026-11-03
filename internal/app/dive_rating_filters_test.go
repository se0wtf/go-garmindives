package app

import (
	"context"
	"net/url"
	"strings"
	"testing"

	httpx "github.com/se0wtf/go-garmindives/internal/httpx"
)

func TestDiveRatingFilters(t *testing.T) {
	store, _ := openTestSQLStore(t)
	var records []DiveRecord
	for _, fixture := range []struct {
		id                    int64
		date, location, notes string
	}{
		{201, "2026-06-01", "Rating Reef", "N:4/5"},
		{202, "2026-06-02", "Rating Reef", "N:5/5"},
		{203, "2026-06-03", "Rating Reef", "N:3/5"},
		{204, "2026-06-04", "Rating Reef", "N:4,5/5"},
		{205, "2026-06-05", "Rating Reef", "N:0/5"},
		{206, "2026-06-06", "Rating Reef", "Unrated dive"},
		{207, "2026-06-07", "Rating Reef", "N:4"},
		{208, "2026-06-08", "Rating Reef", "N:8/10"},
		{209, "2025-06-01", "Rating Reef", "N:4/10"},
		{210, "2026-06-10", "Rating Reef", "N:4/5"},
		{211, "2026-06-11", "Rating Lagoon", "N:5/5"},
	} {
		record := apiTestDive(t, fixture.id, fixture.date, fixture.location)
		record.DiveDetails.(map[string]any)["notes"] = fixture.notes
		records = append(records, record)
	}
	if err := store.SaveDives(context.Background(), records); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE dives SET country_override=CASE WHEN id=210 THEN 'FR' ELSE 'EG' END`); err != nil {
		t.Fatal(err)
	}
	handler := newAPIHandler(store, testLogger(t))
	trip := travelRequest[diveTrip](t, handler, "POST", "/api/v1/trips", map[string]any{
		"name": "Rating trip", "year": 2026, "country_code": "EG", "dive_ids": []int64{201, 202, 203, 204, 206},
	}, 201)
	selected := []int64{211, 210, 207, 202, 201, 209}
	for _, tc := range []struct {
		query string
		ids   []int64
	}{
		{"", []int64{211, 210, 208, 207, 206, 205, 204, 203, 202, 201, 209}},
		{"ratings=4,5", selected},
		{"ratings=4", []int64{210, 207, 201, 209}},
		{"ratings=4.5", []int64{204}},
		{"ratings=0", []int64{205}},
		{"ratings=8", []int64{208}},
		{"ratings=4,4.5", []int64{210, 207, 204, 201, 209}},
		{"ratings=99", nil},
		{"ratings=4&ratings=5", selected},
		{"ratings=4,4,5,4", selected},
		{"ratings=%204%20,%205%20", selected},
		{"ratings=" + strings.Repeat("4,", 99) + "4", []int64{210, 207, 201, 209}},
		{"ratings=4,5&year=2026&country=EG&from=2026-06-01&to=2026-06-06&location=reef", []int64{202, 201}},
		{"ratings=4,5&trip_id=" + trip.ID, []int64{202, 201}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			page := travelRequest[diveListResponse](t, handler, "GET", "/api/v1/dives?"+tc.query, nil, 200)
			var ids []int64
			for _, item := range page.Items {
				ids = append(ids, item.ID)
			}
			if !equalInt64s(ids, tc.ids) || page.Items == nil || page.NextCursor != "" {
				t.Fatalf("rating filter returned IDs %v, cursor %q; want %v", ids, page.NextCursor, tc.ids)
			}
		})
	}
	t.Run("invalid ratings", func(t *testing.T) {
		for _, query := range []string{
			"ratings=", "ratings=4,", "ratings=,4", "ratings=4,,5", "ratings=bad", "ratings=-1",
			"ratings=NaN", "ratings=Inf", "ratings=-Inf", "ratings=1e999", "ratings=4&ratings=NaN",
			"ratings=%20", "ratings=4%29+OR+1%3D1--", "ratings=" + strings.Repeat("4,", 100) + "4",
		} {
			response := travelRequest[httpx.ErrorResponse](t, handler, "GET", "/api/v1/dives?"+query, nil, 400)
			if response.Error.Code != "invalid_request" {
				t.Fatalf("query %q: error code %s", query, response.Error.Code)
			}
		}
	})
	t.Run("pagination keeps rating selection", func(t *testing.T) {
		var ids []int64
		cursor := ""
		for pageNumber := 0; pageNumber < 4; pageNumber++ {
			page := travelRequest[diveListResponse](t, handler, "GET", "/api/v1/dives?ratings=4,5&limit=2&cursor="+url.QueryEscape(cursor), nil, 200)
			for _, item := range page.Items {
				ids = append(ids, item.ID)
			}
			cursor = page.NextCursor
			if cursor == "" {
				break
			}
		}
		if !equalInt64s(ids, selected) || cursor != "" {
			t.Fatalf("pagination returned %v with cursor %q; want %v", ids, cursor, selected)
		}
	})
	t.Run("trip dive lists share rating filter", func(t *testing.T) {
		path := "/api/v1/trips/" + trip.ID + "/dives?ratings=4,5&limit=1"
		first := travelRequest[diveListResponse](t, handler, "GET", path, nil, 200)
		if len(first.Items) != 1 || first.Items[0].ID != 202 || first.NextCursor == "" {
			t.Fatalf("first trip page: %+v", first)
		}
		second := travelRequest[diveListResponse](t, handler, "GET", path+"&cursor="+url.QueryEscape(first.NextCursor), nil, 200)
		if len(second.Items) != 1 || second.Items[0].ID != 201 || second.NextCursor != "" {
			t.Fatalf("second trip page: %+v", second)
		}
		travelRequest[httpx.ErrorResponse](t, handler, "GET", "/api/v1/trips/"+trip.ID+"/dives?ratings=NaN", nil, 400)
	})
}
