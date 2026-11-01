package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	countrydata "github.com/se0wtf/go-garmindives/internal/country"
	httpx "github.com/se0wtf/go-garmindives/internal/httpx"
	identity "github.com/se0wtf/go-garmindives/internal/identity"
)

func travelRequest[T any](t *testing.T, h http.Handler, method, path string, body any, status int) T {
	t.Helper()
	var input *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		input = bytes.NewReader(raw)
	}
	var wBody *bytes.Reader = input
	var responseBytes []byte
	var code int
	if wBody == nil {
		w := mediaRequest(t, h, method, path, nil, "")
		responseBytes, code = w.Body.Bytes(), w.Code
	} else {
		w := mediaRequest(t, h, method, path, wBody, "application/json")
		responseBytes, code = w.Body.Bytes(), w.Code
	}
	if code != status {
		t.Fatalf("%s %s: status=%d body=%s", method, path, code, responseBytes)
	}
	var result T
	if len(responseBytes) > 0 {
		if err := json.Unmarshal(responseBytes, &result); err != nil {
			t.Fatal(err)
		}
	}
	return result
}

func addTravelDive(t *testing.T, s *sqlStore, id int64, date, code string) {
	t.Helper()
	record := testSQLDive(t, id)
	record.Activity.(map[string]any)["startTimeLocal"] = date + "T10:00:00"
	record.DiveDetails.(map[string]any)["startTime"] = date + "T10:00:00"
	if err := s.SaveDives(context.Background(), []DiveRecord{record}); err != nil {
		t.Fatal(err)
	}
	if code != "" {
		if _, err := s.db.Exec(`UPDATE dives SET country_override=? WHERE id=?`, code, id); err != nil {
			t.Fatal(err)
		}
	}
}
func residenceTest(t *testing.T, s *sqlStore, from string, to *string, code string) homeCountry {
	t.Helper()
	h, err := s.saveHomeCountry(context.Background(), identity.NewID(), code, from, to, true)
	if err != nil {
		t.Fatal(err)
	}
	return h
}
func travelSummary(t *testing.T, s *sqlStore, id int64) diveSummary {
	t.Helper()
	d, err := s.Dive(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return d.Dive
}

func TestCountryCatalogAndBilingualFilters(t *testing.T) {
	for value, code := range map[string]string{"Égypte": "EG", "egypte": "EG", "Egypt": "EG", "eg": "EG", "France": "FR", "Nouvelle-Calédonie": "NC", "French Polynesia": "PF"} {
		c, err := countrydata.Resolve(value)
		if err != nil || c.Code != code {
			t.Fatalf("country %q: %+v %v", value, c, err)
		}
	}
	if _, err := countrydata.Resolve("unknown_country"); err == nil {
		t.Fatal("unknown country accepted")
	}
	store, _ := openTestSQLStore(t)
	addTravelDive(t, store, 1, "2025-12-31", "EG")
	addTravelDive(t, store, 2, "2026-01-01", "PF")
	h := newAPIHandler(store, testLogger(t))
	for _, query := range []string{"year=2025&country=Egypte", "country=Egypt&year=2025", "country=eg&from=2025-01-01&to=2025-12-31"} {
		list := travelRequest[diveListResponse](t, h, "GET", "/api/v1/dives?"+query, nil, 200)
		if len(list.Items) != 1 || list.Items[0].ID != 1 || list.Items[0].Year == nil || *list.Items[0].Year != 2025 || list.Items[0].Country.NameEN != "Egypt" {
			t.Fatalf("filter %q: %+v", query, list)
		}
	}
	list := travelRequest[diveListResponse](t, h, "GET", "/api/v1/dives?year=2025&from=2026-01-01", nil, 200)
	if len(list.Items) != 0 {
		t.Fatal("year and date filters should intersect")
	}
	for _, query := range []string{"year=0", "year=10000", "year=x", "country=unknown"} {
		travelRequest[httpx.ErrorResponse](t, h, "GET", "/api/v1/dives?"+query, nil, 400)
	}
}

func TestAutomaticTripsBoundariesAndResidenceCoverage(t *testing.T) {
	s, _ := openTestSQLStore(t)
	for _, d := range []struct {
		id         int64
		date, code string
	}{{1, "2025-12-20", "EG"}, {2, "2026-01-05", "EG"}, {3, "2026-04-05", "EG"}, {4, "2026-07-06", "EG"}, {5, "2026-07-07", "MV"}, {6, "2026-07-08", "FR"}, {7, "2026-07-09", "MV"}, {8, "2027-01-01", "EG"}} {
		addTravelDive(t, s, d.id, d.date, d.code)
	}
	if err := s.reconcileTravel(context.Background()); err != nil {
		t.Fatal(err)
	}
	if travelSummary(t, s, 1).TripID != nil {
		t.Fatal("unknown residence must wait")
	}
	end := "2026-12-31"
	residenceTest(t, s, "2025-01-01", &end, "FR")
	first := travelSummary(t, s, 1)
	if first.TripID == nil {
		t.Fatal("foreign dive did not get a trip")
	}
	for _, id := range []int64{2, 3} {
		if d := travelSummary(t, s, id); d.TripID == nil || *d.TripID != *first.TripID {
			t.Fatalf("cross-year/exact-three-month boundary split dive %d", id)
		}
	}
	trip, err := s.trip(context.Background(), *first.TripID)
	if err != nil || trip.Year != 2025 || trip.Name != "Égypte 2025" || trip.NameEN != "Egypt 2025" || trip.StartDate != "2025-12-20" || trip.EndDate != "2026-04-05" || trip.DiveCount != 3 {
		t.Fatalf("trip %+v err=%v", trip, err)
	}
	ids := map[string]bool{*first.TripID: true}
	for _, id := range []int64{4, 5, 7} {
		d := travelSummary(t, s, id)
		if d.TripID == nil || ids[*d.TripID] {
			t.Fatalf("gap/country/home boundary failed for %d", id)
		}
		ids[*d.TripID] = true
	}
	for _, id := range []int64{6, 8} {
		if travelSummary(t, s, id).TripID != nil {
			t.Fatalf("home/unknown-residence dive %d assigned", id)
		}
	}
	if err := s.reconcileTravel(context.Background()); err != nil {
		t.Fatal(err)
	}
	if *travelSummary(t, s, 1).TripID != *first.TripID {
		t.Fatal("rebuild changed trip identity")
	}
	for input, want := range map[string]string{"2026-01-31": "2026-04-30", "2025-11-30": "2026-02-28", "2027-11-30": "2028-02-29"} {
		date, _ := time.Parse(time.DateOnly, input)
		if got := threeMonthsAfter(date).Format(time.DateOnly); got != want {
			t.Fatalf("three months after %s: %s", input, got)
		}
	}
}

func TestHomeCountriesCRUDOverlapAndRegrouping(t *testing.T) {
	s, _ := openTestSQLStore(t)
	addTravelDive(t, s, 1, "2026-01-01", "EG")
	h := newAPIHandler(s, testLogger(t))
	first := travelRequest[homeCountry](t, h, "POST", "/api/v1/home-countries", map[string]any{"country_code": "France", "from": "2025-01-01", "to": "2025-12-31"}, 201)
	if travelSummary(t, s, 1).TripID != nil {
		t.Fatal("uncovered dive was assigned")
	}
	second := travelRequest[homeCountry](t, h, "POST", "/api/v1/home-countries", map[string]any{"country_code": "Nouvelle-Calédonie", "from": "2026-01-01", "to": nil}, 201)
	if second.Country.Code != "NC" || second.To != nil || travelSummary(t, s, 1).TripID == nil {
		t.Fatal("open-ended residence did not trigger backfill")
	}
	travelRequest[httpx.ErrorResponse](t, h, "POST", "/api/v1/home-countries", map[string]string{"country_code": "PF", "from": "2025-12-31", "to": "2026-01-01"}, 409)
	travelRequest[httpx.ErrorResponse](t, h, "PATCH", "/api/v1/home-countries/"+first.ID, map[string]string{"to": "2026-01-01"}, 409)
	travelRequest[homeCountry](t, h, "PATCH", "/api/v1/home-countries/"+second.ID, map[string]string{"country_code": "Egypt"}, 200)
	if travelSummary(t, s, 1).TripID != nil {
		t.Fatal("new home country did not remove automatic foreign assignment")
	}
	travelRequest[map[string]any](t, h, "DELETE", "/api/v1/home-countries/"+second.ID, nil, 204)
	travelRequest[httpx.ErrorResponse](t, h, "GET", "/api/v1/home-countries/"+second.ID, nil, 404)
	travelRequest[httpx.ErrorResponse](t, h, "POST", "/api/v1/home-countries", map[string]string{"country_code": "FR", "from": "2026-02-30"}, 400)
}

func TestTripCRUDOverridesManualMembershipAndFavorites(t *testing.T) {
	library, _ := mediaTestLibrary(t)
	s := library.store
	addTravelDive(t, s, 1, "2025-12-20", "EG")
	addTravelDive(t, s, 2, "2026-01-05", "MV")
	residenceTest(t, s, "2020-01-01", nil, "FR")
	h := newAPIHandler(s, testLogger(t))
	tripID := *travelSummary(t, s, 1).TripID
	typeFields := map[string]any{"name": "Croisière entre amis", "name_en": "Cruise with friends", "subtitle": "Requins et récifs", "description": "Deux pays sur le même bateau.", "rating": 0, "cruise": true, "operator_name": "Dive Boat", "operator_website": "https://example.com", "operator_comment": "Très bon équipage", "price": "Environ 1 500 € par personne"}
	trip := travelRequest[diveTrip](t, h, "PATCH", "/api/v1/trips/"+tripID, typeFields, 200)
	if trip.Name != typeFields["name"] || trip.NameEN != "Cruise with friends" || trip.Rating == nil || *trip.Rating != 0 || !trip.Cruise || trip.Price != typeFields["price"] {
		t.Fatalf("trip fields: %+v", trip)
	}
	travelRequest[map[string]any](t, h, "PUT", "/api/v1/trips/"+tripID+"/dives/2", nil, 204)
	if d := travelSummary(t, s, 2); d.TripID == nil || *d.TripID != tripID || d.TripAssignment != "manual" {
		t.Fatal("manual multi-country link failed")
	}
	if err := s.reconcileTravel(context.Background()); err != nil {
		t.Fatal(err)
	}
	trip, err := s.trip(context.Background(), tripID)
	if err != nil || trip.Country.Code != "EG" || trip.Year != 2025 || trip.Name != "Croisière entre amis" || trip.DiveCount != 2 {
		t.Fatalf("manual overrides lost: %+v %v", trip, err)
	}
	photo, err := library.Upload(context.Background(), 1, "Shark.jpg", strings.NewReader("photo"), nil)
	if err != nil {
		t.Fatal(err)
	}
	travelRequest[mediaItem](t, h, "PATCH", "/api/v1/dives/1/media/"+photo.ID, map[string]bool{"favorite": true}, 200)
	favorites := travelRequest[mediaListResponse](t, h, "GET", "/api/v1/media/favorites?trip_id="+tripID, nil, 200)
	if len(favorites.Items) != 1 || !favorites.Items[0].Favorite {
		t.Fatal("trip favorites missing")
	}
	detail := travelRequest[tripDetail](t, h, "GET", "/api/v1/trips/"+tripID, nil, 200)
	if len(detail.FavoriteDiveMedia) != 1 || len(detail.Media) != 0 || len(detail.DiveIDs) != 2 {
		t.Fatalf("trip collections: %+v", detail)
	}
	travelRequest[map[string]any](t, h, "DELETE", "/api/v1/trips/"+tripID+"/dives/1", nil, 204)
	travelRequest[map[string]any](t, h, "POST", "/api/v1/trips/rebuild", nil, 200)
	if d := travelSummary(t, s, 1); d.TripID != nil || d.TripAssignment != "excluded" {
		t.Fatal("excluded dive was automatically re-added")
	}
	favorites = travelRequest[mediaListResponse](t, h, "GET", "/api/v1/media/favorites?trip_id="+tripID, nil, 200)
	if len(favorites.Items) != 0 {
		t.Fatal("removed dive remained in trip favorites")
	}
	travelRequest[diveSummary](t, h, "PATCH", "/api/v1/dives/1", map[string]string{"trip_mode": "auto"}, 200)
	if travelSummary(t, s, 1).TripID == nil {
		t.Fatal("automatic assignment could not be restored")
	}
	for _, filter := range []string{"name=cruise", "name=Croisiere", "year=2025", "country=Egypt", "min_rating=0&max_rating=0", "from=2025-12-25&to=2026-01-02"} {
		list := travelRequest[tripListResponse](t, h, "GET", "/api/v1/trips?"+filter, nil, 200)
		found := false
		for _, item := range list.Items {
			if item.ID == tripID {
				found = true
			}
		}
		if !found {
			t.Fatalf("trip filter %s did not find customized trip", filter)
		}
	}
	travelRequest[httpx.ErrorResponse](t, h, "PATCH", "/api/v1/trips/"+tripID, map[string]int{"rating": 6}, 400)
	travelRequest[httpx.ErrorResponse](t, h, "GET", "/api/v1/trips?min_rating=NaN", nil, 400)
	travelRequest[map[string]any](t, h, "DELETE", "/api/v1/trips/"+tripID, nil, 204)
	travelRequest[httpx.ErrorResponse](t, h, "GET", "/api/v1/trips/"+tripID, nil, 404)
	if d := travelSummary(t, s, 2); d.TripID != nil || d.TripAssignment != "excluded" {
		t.Fatal("deleting a trip did not preserve/exclude its dives")
	}
	global := travelRequest[mediaListResponse](t, h, "GET", "/api/v1/media/favorites", nil, 200)
	if len(global.Items) != 1 || global.Items[0].ID != photo.ID {
		t.Fatal("deleting trip deleted a favorite dive attachment")
	}
}

func TestCountryOverrideAndAtomicDivePatch(t *testing.T) {
	library, _ := mediaTestLibrary(t)
	s := library.store
	addTravelDive(t, s, 1, "2026-01-01", "EG")
	h := newAPIHandler(s, testLogger(t))
	photo, err := library.Upload(context.Background(), 1, "Photo.jpg", strings.NewReader("photo"), nil)
	if err != nil {
		t.Fatal(err)
	}
	d := travelRequest[diveSummary](t, h, "PATCH", "/api/v1/dives/1", map[string]string{"country_code": "French Polynesia", "cover_media_id": photo.ID}, 200)
	if d.Country.Code != "PF" || d.CountrySource != "manual" || d.CoverMediaID == nil {
		t.Fatal("country override missing")
	}
	travelRequest[httpx.ErrorResponse](t, h, "PATCH", "/api/v1/dives/1", map[string]any{"country_code": "Egypt", "cover_media_id": "wrong"}, 400)
	current := travelSummary(t, s, 1)
	if !reflect.DeepEqual(d.Country, current.Country) {
		t.Fatal("invalid cover partially changed country")
	}
	d = travelRequest[diveSummary](t, h, "PATCH", "/api/v1/dives/1", map[string]any{"country_code": nil}, 200)
	if d.CountryOverride != nil || d.CountrySource == "manual" {
		t.Fatal("country override did not reset")
	}
}

func TestTripBackfillPreservesCustomizedTripAndItsMedia(t *testing.T) {
	for _, customName := range []bool{false, true} {
		t.Run(map[bool]string{false: "media only", true: "custom name"}[customName], func(t *testing.T) {
			library, _ := mediaTestLibrary(t)
			s := library.store
			addTravelDive(t, s, 1, "2025-12-31", "EG")
			addTravelDive(t, s, 2, "2026-01-01", "")
			addTravelDive(t, s, 3, "2026-01-02", "EG")
			residenceTest(t, s, "2020-01-01", nil, "FR")
			earlier, later := *travelSummary(t, s, 1).TripID, *travelSummary(t, s, 3).TripID
			if earlier == later {
				t.Fatal("unknown middle location should wait before grouping")
			}
			h := newAPIHandler(s, testLogger(t))
			if customName {
				travelRequest[diveTrip](t, h, "PATCH", "/api/v1/trips/"+later, map[string]string{"name": "Notre croisière"}, 200)
			}
			if _, err := library.uploadOwned(context.Background(), mediaOwner{TripID: later}, "Bateau.jpg", strings.NewReader("photo"), nil); err != nil {
				t.Fatal(err)
			}
			travelRequest[diveSummary](t, h, "PATCH", "/api/v1/dives/2", map[string]string{"country_code": "EG"}, 200)
			for _, id := range []int64{1, 2, 3} {
				d := travelSummary(t, s, id)
				if d.TripID == nil || *d.TripID != later {
					t.Fatalf("backfill orphaned the customized trip for dive %d", id)
				}
			}
			detail := travelRequest[tripDetail](t, h, "GET", "/api/v1/trips/"+later, nil, 200)
			if detail.Trip.Year != 2025 || len(detail.Media) != 1 || (customName && detail.Trip.Name != "Notre croisière") {
				t.Fatal("backfill lost trip metadata or media")
			}
			travelRequest[diveSummary](t, h, "PATCH", "/api/v1/dives/1", map[string]any{"trip_id": nil}, 200)
			travelRequest[diveSummary](t, h, "PATCH", "/api/v1/dives/2", map[string]any{"trip_id": nil}, 200)
			travelRequest[diveSummary](t, h, "PATCH", "/api/v1/dives/3", map[string]any{"trip_id": nil}, 200)
			list := travelRequest[tripListResponse](t, h, "GET", "/api/v1/trips", nil, 200)
			found := false
			for _, trip := range list.Items {
				if trip.ID == later {
					found = true
				}
			}
			if !found {
				t.Fatal("empty automatic trip containing user media disappeared from the list")
			}
		})
	}
}
