package app

import (
	"reflect"
	"strings"
	"testing"

	countrydata "github.com/se0wtf/go-garmindives/internal/country"
	httpx "github.com/se0wtf/go-garmindives/internal/httpx"
)

func TestCountryChoiceEndpointsOmitBlankAndDuplicateLabels(t *testing.T) {
	a, config := bootstrapApplication(t)
	type countryChoices struct {
		Items []countrydata.Country `json:"items"`
	}
	check := func(path string) {
		t.Helper()
		response := appJSON[countryChoices](t, a, "GET", path, nil, nil, 200)
		if len(response.Items) < 240 {
			t.Fatalf("%s omitted worldwide country choices", path)
		}
		for _, label := range []func(countrydata.Country) string{
			func(c countrydata.Country) string { return c.Code },
			func(c countrydata.Country) string { return c.NameFR },
			func(c countrydata.Country) string { return c.NameEN },
		} {
			seen := map[string]bool{}
			for _, c := range response.Items {
				value := strings.TrimSpace(label(c))
				if value == "" || seen[value] {
					t.Fatalf("%s returned empty/duplicate choice %q", path, value)
				}
				seen[value] = true
			}
		}
	}
	check("/api/v1/setup/countries")
	setupApplication(t, a, config)
	check("/api/v1/countries")
	for _, query := range []string{"France", "United+Kingdom"} {
		response := appJSON[countryChoices](t, a, "GET", "/api/v1/countries?q="+query, nil, nil, 200)
		if len(response.Items) != 1 {
			t.Fatalf("country search %s returned %d choices; want one", query, len(response.Items))
		}
	}
}

func TestDiveCountryChoicesUseStoredEffectiveCountries(t *testing.T) {
	a, config := bootstrapApplication(t)
	gate := appJSON[httpx.ErrorResponse](t, a, "GET", "/api/v1/dives/countries", nil, nil, 503)
	if gate.Error.Code != "not_configured" {
		t.Fatal("dive-country choices bypassed initial setup")
	}
	setupApplication(t, a, config)
	type countryChoices struct {
		Items []countrydata.Country `json:"items"`
	}
	empty := appJSON[countryChoices](t, a, "GET", "/api/v1/dives/countries", nil, nil, 200)
	if empty.Items == nil || len(empty.Items) != 0 {
		t.Fatalf("empty logbook must return items: [], got %+v", empty)
	}
	store := waitApplicationReady(t, a).store
	for id := int64(1); id <= 5; id++ {
		addTravelDive(t, store, id, "2026-01-01", "")
	}
	for id, code := range map[int64]string{1: "EG", 2: "EG", 3: "FR", 5: "NC"} {
		if _, err := store.db.Exec(`UPDATE dives SET country_code=? WHERE id=?`, code, id); err != nil {
			t.Fatal(err)
		}
	}
	// A residence and a cached lookup without a dive must not add choices.
	if _, err := store.db.Exec(`INSERT INTO country_lookup_cache(lookup_key,latitude,longitude,country_code,status) VALUES('unused',10,20,'JP','ready')`); err != nil {
		t.Fatal(err)
	}
	cookie := loginApplication(t, a, testAdminPassword)
	appJSON[diveSummary](t, a, "PATCH", "/api/v1/dives/3", map[string]string{"country_code": "PF"}, cookie, 200)
	response := appJSON[countryChoices](t, a, "GET", "/api/v1/dives/countries", nil, nil, 200)
	want := []countrydata.Country{*countrydata.ByCode("EG"), *countrydata.ByCode("NC"), *countrydata.ByCode("PF")}
	if !reflect.DeepEqual(response.Items, want) {
		t.Fatalf("country choices included duplicates, unresolved or overridden countries: %+v", response.Items)
	}
	for query, code := range map[string]string{"egypte": "EG", "French+Polynesia": "PF", "Nouvelle-Caledonie": "NC"} {
		found := appJSON[countryChoices](t, a, "GET", "/api/v1/dives/countries?q="+query, nil, nil, 200)
		if len(found.Items) != 1 || found.Items[0].Code != code {
			t.Fatalf("dive-country search %s: %+v", query, found.Items)
		}
	}
	for _, query := range []string{"France", "Japan", "no_such_country"} {
		found := appJSON[countryChoices](t, a, "GET", "/api/v1/dives/countries?q="+query, nil, nil, 200)
		if found.Items == nil || len(found.Items) != 0 {
			t.Fatalf("search included a country outside effective dive destinations: %+v", found.Items)
		}
	}
	// Home-country selectors keep the complete worldwide catalog.
	for _, path := range []string{"/api/v1/countries", "/api/v1/setup/countries"} {
		all := appJSON[countryChoices](t, a, "GET", path, nil, nil, 200)
		if len(all.Items) < 240 {
			t.Fatalf("dive filters restricted home-country choices at %s", path)
		}
	}
	appJSON[diveSummary](t, a, "PATCH", "/api/v1/dives/3", map[string]any{"country_code": nil}, cookie, 200)
	response = appJSON[countryChoices](t, a, "GET", "/api/v1/dives/countries", nil, nil, 200)
	if !reflect.DeepEqual(response.Items, []countrydata.Country{*countrydata.ByCode("EG"), *countrydata.ByCode("FR"), *countrydata.ByCode("NC")}) {
		t.Fatalf("country choices did not update when override was cleared: %+v", response.Items)
	}
	// Old alias codes must still produce a usable canonical filter choice.
	if _, err := store.db.Exec(`UPDATE dives SET country_override='FX' WHERE id=4`); err != nil {
		t.Fatal(err)
	}
	response = appJSON[countryChoices](t, a, "GET", "/api/v1/dives/countries?q=France", nil, nil, 200)
	if len(response.Items) != 1 || response.Items[0].Code != "FR" {
		t.Fatalf("alias produced a duplicate filter choice: %+v", response.Items)
	}
	filtered := appJSON[diveListResponse](t, a, "GET", "/api/v1/dives?country=FR", nil, nil, 200)
	if len(filtered.Items) != 2 || filtered.Items[0].Country.Code != "FR" || filtered.Items[1].Country.Code != "FR" {
		t.Fatalf("canonical filter did not match stored aliases: %+v", filtered.Items)
	}
}
