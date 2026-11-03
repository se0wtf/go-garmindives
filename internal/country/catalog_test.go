package country

import (
	"strings"
	"testing"

	"golang.org/x/text/language"
)

func TestCatalogHasUniqueCanonicalCountriesAndBilingualLabels(t *testing.T) {
	if len(Catalog) < 240 {
		t.Fatal("catalog is missing worldwide country/territory choices")
	}
	codes, french, english := map[string]bool{}, map[string]string{}, map[string]string{}
	previous := ""
	for _, c := range Catalog {
		region, err := language.ParseRegion(c.Code)
		if err != nil || region.Canonicalize().String() != c.Code || !region.IsCountry() || c.Code == "ZZ" {
			t.Fatalf("noncanonical country choice: %+v", c)
		}
		if codes[c.Code] || c.Code <= previous {
			t.Fatalf("duplicate or unsorted country code: %s", c.Code)
		}
		codes[c.Code], previous = true, c.Code
		for _, label := range []struct {
			text string
			seen map[string]string
		}{{c.NameFR, french}, {c.NameEN, english}} {
			name := NormalizeSearch(label.text)
			if name == "" || strings.TrimSpace(label.text) != label.text {
				t.Fatalf("empty/untrimmed country label: %+v", c)
			}
			if earlier := label.seen[name]; earlier != "" {
				t.Fatalf("duplicate country label %q for %s and %s", label.text, earlier, c.Code)
			}
			label.seen[name] = c.Code
		}
	}
	for _, code := range []string{"FR", "GB", "EG", "NC", "PF", "US", "JP", "ZA", "BR"} {
		if !codes[code] {
			t.Errorf("missing country/territory %s", code)
		}
	}
}

func TestCountryResolutionRetainsLegacyAliasesAndRejectsBlankChoices(t *testing.T) {
	for input, code := range map[string]string{"FX": "FR", "UK": "GB", "DD": "DE", "TP": "TL", "BU": "MM", "France": "FR", "egypte": "EG", "French Polynesia": "PF"} {
		c, err := Resolve(input)
		if err != nil || c.Code != code {
			t.Errorf("Resolve(%q) = %+v, %v; want %s", input, c, err, code)
		}
	}
	for _, input := range []string{"", " ", "\u00a0", "AN", "SU", "unknown_country"} {
		if _, err := Resolve(input); err == nil {
			t.Errorf("accepted empty/unknown country %q", input)
		}
	}
	for input, code := range map[string]string{"FX": "FR", "UK": "GB", "NC": "NC", "PF": "PF"} {
		if c := ByCode(input); c == nil || c.Code != code {
			t.Errorf("legacy stored code %s did not resolve to %s", input, code)
		}
	}
}
