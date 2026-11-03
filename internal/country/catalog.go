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
	seen := make(map[string]bool)
	fr, en := display.Regions(language.French), display.Regions(language.English)
	for _, region := range language.Supported.Regions() {
		// Supported regions include deprecated aliases such as FX/FR and UK/GB.
		// Some historical regions have no replacement or localized display name.
		region = region.Canonicalize()
		code := region.String()
		if len(code) != 2 || !region.IsCountry() || code == "ZZ" || seen[code] {
			continue
		}
		nameFR, nameEN := strings.TrimSpace(fr.Name(region)), strings.TrimSpace(en.Name(region))
		if nameFR == "" || nameEN == "" {
			continue
		}
		seen[code] = true
		list = append(list, Country{Code: code, NameFR: nameFR, NameEN: nameEN})
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
	if value != "" {
		// Continue accepting unambiguous legacy codes without offering duplicate
		// choices in the catalog. Persist new selections under their current code.
		if c := ByCode(value); c != nil {
			return c, nil
		}
	}
	for _, c := range Catalog {
		if NormalizeSearch(c.Code) == value || NormalizeSearch(c.NameFR) == value || NormalizeSearch(c.NameEN) == value {
			copy := c
			return &copy, nil
		}
	}
	return nil, errors.New("unknown country; use a country code or French/English name from /api/v1/countries")
}

func ByCode(code string) *Country {
	if region, err := language.ParseRegion(code); err == nil {
		code = region.Canonicalize().String()
	}
	for _, c := range Catalog {
		if c.Code == code {
			copy := c
			return &copy
		}
	}
	return nil
}

// MatchingCodes includes historical spellings of a canonical country code so
// filtering remains compatible with older stored rows. Resolve inputs first.
func MatchingCodes(code string) []string {
	codes := []string{code}
	for _, region := range language.Supported.Regions() {
		alias := region.String()
		if alias != code && region.Canonicalize().String() == code {
			codes = append(codes, alias)
		}
	}
	return codes
}
