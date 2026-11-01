package country

import (
	"errors"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/language"
	"golang.org/x/text/language/display"
	"golang.org/x/text/unicode/norm"
)

type Country struct {
	Code   string `json:"code"`
	NameFR string `json:"name_fr"`
	NameEN string `json:"name_en"`
}

var Catalog = func() []Country {
	var list []Country
	fr, en := display.Regions(language.French), display.Regions(language.English)
	for _, region := range language.Supported.Regions() {
		if code := region.String(); len(code) == 2 && region.IsCountry() && code != "ZZ" {
			list = append(list, Country{Code: code, NameFR: fr.Name(region), NameEN: en.Name(region)})
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Code < list[j].Code })
	return list
}()

func NormalizeSearch(s string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(strings.ToLower(strings.TrimSpace(s))) {
		if !unicode.Is(unicode.Mn, r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func Resolve(value string) (*Country, error) {
	value = NormalizeSearch(value)
	for _, c := range Catalog {
		if NormalizeSearch(c.Code) == value || NormalizeSearch(c.NameFR) == value || NormalizeSearch(c.NameEN) == value {
			copy := c
			return &copy, nil
		}
	}
	return nil, errors.New("unknown country; use a country code or French/English name from /api/v1/countries")
}

func ByCode(code string) *Country {
	for _, c := range Catalog {
		if c.Code == code {
			copy := c
			return &copy
		}
	}
	return nil
}
