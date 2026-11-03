package app

import (
	"context"
	"net/url"
	"reflect"
	"strings"
	"testing"

	httpx "github.com/se0wtf/go-garmindives/internal/httpx"
)

func TestTripRatingFilters(t *testing.T) {
	store, _ := openTestSQLStore(t)
	exerciseTripRatingFilters(t, store)
}

func exerciseTripRatingFilters(t *testing.T, store *sqlStore) {
	t.Helper()
	handler := newAPIHandler(store, testLogger(t))
	tripIDs := map[string]string{}
	for index, fixture := range []struct {
		key, date, country string
		year               int
		rating             *float64
	}{
		{"four", "2026-06-01", "EG", 2026, noteTestFloat(4)},
		{"five", "2026-06-02", "EG", 2026, noteTestFloat(5)},
		{"three", "2026-06-03", "EG", 2026, noteTestFloat(3)},
		{"decimal", "2026-06-04", "EG", 2026, noteTestFloat(4.5)},
		{"zero", "2026-06-05", "EG", 2026, noteTestFloat(0)},
		{"unrated", "2026-06-06", "EG", 2026, nil},
		{"old_four", "2025-06-01", "PF", 2025, noteTestFloat(4)},
		{"five_lagoon", "2026-06-08", "EG", 2026, noteTestFloat(5)},
		{"four_new", "2026-06-09", "FR", 2026, noteTestFloat(4)},
	} {
		diveID := int64(501 + index)
		record := apiTestDive(t, diveID, fixture.date, "Reef")
		// A highly rated member must not make an unrated/low-rated trip match.
		record.DiveDetails.(map[string]any)["notes"] = "N:5/5"
		if err := store.SaveDives(context.Background(), []DiveRecord{record}); err != nil {
			t.Fatal(err)
		}
		name := "Rating reef " + fixture.key
		if fixture.key == "five_lagoon" {
			name = "Rating lagoon"
		}
		trip := travelRequest[diveTrip](t, handler, "POST", "/api/v1/trips", map[string]any{
			"name": name, "year": fixture.year, "country_code": fixture.country,
			"rating": fixture.rating, "dive_ids": []int64{diveID},
		}, 201)
		tripIDs[fixture.key] = trip.ID
	}
	selected := []string{"four_new", "five_lagoon", "five", "four", "old_four"}
	assertPage := func(t *testing.T, page tripListResponse, keys []string) {
		t.Helper()
		var got, want []string
		for _, trip := range page.Items {
			got = append(got, trip.ID)
		}
		for _, key := range keys {
			want = append(want, tripIDs[key])
		}
		if !reflect.DeepEqual(got, want) || page.Items == nil {
			t.Fatalf("trip filter returned %v, want %v (keys %v)", got, want, keys)
		}
	}
	for _, tc := range []struct {
		query string
		keys  []string
	}{
		{"", []string{"four_new", "five_lagoon", "unrated", "zero", "decimal", "three", "five", "four", "old_four"}},
		{"ratings=4,5", selected},
		{"ratings=4", []string{"four_new", "four", "old_four"}},
		{"ratings=4.5", []string{"decimal"}},
		{"ratings=0", []string{"zero"}},
		{"ratings=6", nil},
		{"ratings=4,4.5", []string{"four_new", "decimal", "four", "old_four"}},
		{"ratings=4&ratings=5", selected},
		{"ratings=4,4,5", selected},
		{"ratings=%204%20,%205%20", selected},
		{"ratings=" + strings.Repeat("4,", 99) + "4", []string{"four_new", "four", "old_four"}},
		{"ratings=4,5&name=Rating+reef&year=2026&country=Egypt&from=2026-06-01&to=2026-06-03", []string{"five", "four"}},
		{"ratings=4,4.5,5", []string{"four_new", "five_lagoon", "decimal", "five", "four", "old_four"}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			query, err := url.ParseQuery(tc.query)
			if err != nil {
				t.Fatal(err)
			}
			if !query.Has("name") {
				query.Set("name", "Rating")
			}
			page := travelRequest[tripListResponse](t, handler, "GET", "/api/v1/trips?"+query.Encode(), nil, 200)
			assertPage(t, page, tc.keys)
			if page.NextCursor != "" {
				t.Fatal("unexpected cursor on a complete filtered list")
			}
		})
	}
	t.Run("invalid ratings", func(t *testing.T) {
		for _, query := range []string{
			"ratings=", "ratings=4,", "ratings=4,,5", "ratings=-1", "ratings=bad", "ratings=NaN",
			"ratings=Inf", "ratings=-Inf", "ratings=1e999", "ratings=4&ratings=NaN",
			"ratings=" + strings.Repeat("4,", 100) + "4",
		} {
			response := travelRequest[httpx.ErrorResponse](t, handler, "GET", "/api/v1/trips?"+query, nil, 400)
			if response.Error.Code != "invalid_request" {
				t.Fatalf("query %q returned error %s", query, response.Error.Code)
			}
		}
	})
	t.Run("removed rating bounds", func(t *testing.T) {
		for _, query := range []string{
			"min_rating=4", "max_rating=5", "min_rating=4&max_rating=5",
			"min_rating", "max_rating", "min_rating=&max_rating=",
			"ratings=4,5&min_rating=4", "ratings=4,5&max_rating=5",
		} {
			response := travelRequest[httpx.ErrorResponse](t, handler, "GET", "/api/v1/trips?"+query, nil, 400)
			if response.Error.Code != "invalid_request" || !strings.Contains(response.Error.Message, "use ratings") {
				t.Fatalf("query %q did not explain the replacement: %+v", query, response.Error)
			}
		}
	})
	t.Run("pagination keeps rating selection", func(t *testing.T) {
		all := tripListResponse{Items: []diveTrip{}}
		cursor := ""
		for pageNumber := 0; pageNumber < 4; pageNumber++ {
			page := travelRequest[tripListResponse](t, handler, "GET", "/api/v1/trips?name=Rating&ratings=4,5&limit=2&cursor="+url.QueryEscape(cursor), nil, 200)
			all.Items = append(all.Items, page.Items...)
			cursor = page.NextCursor
			if cursor == "" {
				break
			}
		}
		assertPage(t, all, selected)
		if cursor != "" {
			t.Fatal("pagination did not reach the last matching trip")
		}
	})
}
