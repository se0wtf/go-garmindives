package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"
)

var errTripNotFound = errors.New("trip not found")
var errInvalidTrip = errors.New("invalid trip")
var errResidenceOverlap = errors.New("home-country date ranges must not overlap")

func (t diveTrip) translationSource() translationSource {
	name := ""
	if t.NameOverridden && t.NameEN == "" {
		name = t.Name
	}
	return translationSource{Kind: "trip", Name: name, Subtitle: t.Subtitle, Description: t.Description, OperatorComment: t.OperatorComment, Price: t.Price}
}

type homeCountry struct {
	ID      string   `json:"id"`
	Country *country `json:"country"`
	From    string   `json:"from"`
	To      *string  `json:"to"`
}

type diveTrip struct {
	ID                string       `json:"id"`
	Name              string       `json:"name"`
	NameEN            string       `json:"name_en"`
	NameOverridden    bool         `json:"name_overridden"`
	Year              int          `json:"year"`
	YearOverridden    bool         `json:"year_overridden"`
	Country           *country     `json:"country"`
	CountryOverridden bool         `json:"country_overridden"`
	Subtitle          string       `json:"subtitle"`
	Description       string       `json:"description"`
	Rating            *float64     `json:"rating"`
	Cruise            bool         `json:"cruise"`
	OperatorName      string       `json:"operator_name"`
	OperatorWebsite   string       `json:"operator_website"`
	OperatorComment   string       `json:"operator_comment"`
	Price             string       `json:"price"`
	StartDate         string       `json:"start_date"`
	EndDate           string       `json:"end_date"`
	Automatic         bool         `json:"automatic"`
	DiveCount         int64        `json:"dive_count"`
	CoverMediaID      *string      `json:"cover_media_id"`
	CoverPhoto        *mediaItem   `json:"cover_photo"`
	CreatedAt         string       `json:"created_at"`
	UpdatedAt         string       `json:"updated_at"`
	Translations      translations `json:"translations"`
	edited, deleted   bool
	anchor            sql.NullInt64
}

const tripColumns = `id,name,name_en,name_override,year,year_override,country_code,country_override,subtitle,description,
	rating,cruise,operator_name,operator_website,operator_comment,price,start_date,end_date,automatic,edited,deleted,anchor_dive_id,
	cover_media_id,created_at,updated_at`

func scanTrip(row rowScanner) (diveTrip, error) {
	var t diveTrip
	var code, cover sql.NullString
	var rating sql.NullFloat64
	err := row.Scan(&t.ID, &t.Name, &t.NameEN, &t.NameOverridden, &t.Year, &t.YearOverridden, &code, &t.CountryOverridden, &t.Subtitle, &t.Description,
		&rating, &t.Cruise, &t.OperatorName, &t.OperatorWebsite, &t.OperatorComment, &t.Price, &t.StartDate, &t.EndDate, &t.Automatic, &t.edited, &t.deleted, &t.anchor,
		&cover, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return t, errTripNotFound
	}
	if err != nil {
		return t, err
	}
	t.Country = countryByCode(code.String)
	t.CoverMediaID = nullStringPointer(cover)
	t.Rating = nullFloatPointer(rating)
	return t, nil
}

func boolNumber(value bool) int {
	if value {
		return 1
	}
	return 0
}
func tripCountryCode(t diveTrip) any {
	if t.Country == nil {
		return nil
	}
	return t.Country.Code
}
func nowString() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func saveTripTx(ctx context.Context, s *sqlStore, tx *sql.Tx, t diveTrip) error {
	searchName := searchText(t.Name + " " + t.NameEN)
	if t.NameOverridden && t.NameEN == "" {
		source := t.translationSource()
		key, _ := s.translationService().key(source)
		var raw string
		err := tx.QueryRowContext(ctx, s.bind(`SELECT result_json FROM translation_cache WHERE source_hash=? AND status='ready'`), key).Scan(&raw)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			var values []*string
			if json.Unmarshal([]byte(raw), &values) == nil {
				if translated, err := source.translated(values); err == nil {
					searchName = searchText(t.Name + " " + translated.Name)
				}
			}
		}
	}
	_, err := tx.ExecContext(ctx, s.bind(`INSERT INTO dive_trips (`+tripColumns+`,search_name) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	 ON CONFLICT (id) DO UPDATE SET name=excluded.name,name_en=excluded.name_en,name_override=excluded.name_override,
	 year=excluded.year,year_override=excluded.year_override,country_code=excluded.country_code,country_override=excluded.country_override,
	 subtitle=excluded.subtitle,description=excluded.description,rating=excluded.rating,cruise=excluded.cruise,operator_name=excluded.operator_name,
	 operator_website=excluded.operator_website,operator_comment=excluded.operator_comment,price=excluded.price,start_date=excluded.start_date,end_date=excluded.end_date,
	 automatic=excluded.automatic,edited=excluded.edited,deleted=excluded.deleted,anchor_dive_id=excluded.anchor_dive_id,cover_media_id=excluded.cover_media_id,
	 updated_at=excluded.updated_at,search_name=CASE WHEN excluded.name=dive_trips.name AND excluded.name_en=dive_trips.name_en THEN dive_trips.search_name ELSE excluded.search_name END`),
		t.ID, t.Name, t.NameEN, boolNumber(t.NameOverridden), t.Year, boolNumber(t.YearOverridden), tripCountryCode(t), boolNumber(t.CountryOverridden), t.Subtitle, t.Description,
		t.Rating, boolNumber(t.Cruise), t.OperatorName, t.OperatorWebsite, t.OperatorComment, t.Price, t.StartDate, t.EndDate, boolNumber(t.Automatic), boolNumber(t.edited), boolNumber(t.deleted), t.anchor,
		t.CoverMediaID, t.CreatedAt, t.UpdatedAt, searchName)
	return err
}

type travelDive struct {
	id                            int64
	date, start, code, trip, mode string
}

// Add three calendar months while clamping month-end dates (31 January ->
// 30 April), rather than treating a month as thirty days or rolling into May.
func threeMonthsAfter(date time.Time) time.Time {
	y, m, _ := date.Date()
	target := time.Date(y, m+4, 0, 0, 0, 0, 0, time.UTC)
	day := min(date.Day(), target.Day())
	return time.Date(target.Year(), target.Month(), day, 0, 0, 0, 0, time.UTC)
}

func reconcileTripsTx(ctx context.Context, s *sqlStore, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT id,country_code,from_date,to_date FROM home_countries ORDER BY from_date,id`)
	if err != nil {
		return err
	}
	type residence struct {
		code, from string
		to         sql.NullString
	}
	var homes []residence
	for rows.Next() {
		var id string
		var h residence
		if err := rows.Scan(&id, &h.code, &h.from, &h.to); err != nil {
			rows.Close()
			return err
		}
		homes = append(homes, h)
	}
	if err := closeRows(rows); err != nil {
		return err
	}
	rows, err = tx.QueryContext(ctx, `SELECT `+tripColumns+` FROM dive_trips ORDER BY created_at,id`)
	if err != nil {
		return err
	}
	trips := map[string]diveTrip{}
	var tripOrder []string
	for rows.Next() {
		t, err := scanTrip(rows)
		if err != nil {
			rows.Close()
			return err
		}
		trips[t.ID] = t
		tripOrder = append(tripOrder, t.ID)
	}
	if err := closeRows(rows); err != nil {
		return err
	}
	rows, err = tx.QueryContext(ctx, `SELECT DISTINCT trip_id FROM trip_media`)
	if err != nil {
		return err
	}
	withMedia := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		withMedia[id] = true
	}
	if err := closeRows(rows); err != nil {
		return err
	}
	rows, err = tx.QueryContext(ctx, `SELECT id,local_date,start_time,COALESCE(country_override,country_code,''),COALESCE(trip_id,''),trip_assignment FROM dives ORDER BY local_date,start_time,id`)
	if err != nil {
		return err
	}
	var dives []travelDive
	for rows.Next() {
		var d travelDive
		if err := rows.Scan(&d.id, &d.date, &d.start, &d.code, &d.trip, &d.mode); err != nil {
			rows.Close()
			return err
		}
		dives = append(dives, d)
	}
	if err := closeRows(rows); err != nil {
		return err
	}
	var groups [][]int
	var group []int
	var lastDate time.Time
	lastCode := ""
	flush := func() {
		if len(group) > 0 {
			groups = append(groups, group)
			group = nil
		}
		lastCode = ""
	}
	for i, d := range dives {
		date, dateErr := time.Parse(time.DateOnly, d.date)
		home := ""
		for _, h := range homes {
			if d.date >= h.from && (!h.to.Valid || d.date <= h.to.String) {
				home = h.code
				break
			}
		}
		if dateErr != nil || d.code == "" || home == "" || home == d.code {
			flush()
			continue
		}
		if d.mode == "excluded" {
			continue
		}
		if len(group) > 0 && (lastCode != d.code || date.After(threeMonthsAfter(lastDate))) {
			flush()
		}
		group = append(group, i)
		lastCode, lastDate = d.code, date
	}
	flush()
	desired := map[int64]string{}
	used := map[string]bool{}
	created := map[string]bool{}
	manualMembers := map[string]bool{}
	for _, d := range dives {
		if d.mode == "manual" {
			manualMembers[d.trip] = true
		}
	}
	for _, g := range groups {
		var automatic []int
		members := map[int64]bool{}
		for _, i := range g {
			members[dives[i].id] = true
			if dives[i].mode == "auto" {
				automatic = append(automatic, i)
			}
		}
		if len(automatic) == 0 {
			continue
		}
		selected := ""
		protected := false
		for _, i := range g {
			t, ok := trips[dives[i].trip]
			if ok && t.Automatic && !t.deleted && !used[t.ID] {
				custom := t.edited || withMedia[t.ID] || manualMembers[t.ID]
				if selected == "" || (!protected && custom) {
					selected = t.ID
					protected = custom
				}
			}
		}
		if selected == "" {
			for _, id := range tripOrder {
				t := trips[id]
				if t.Automatic && !t.deleted && !used[id] && t.anchor.Valid && members[t.anchor.Int64] {
					selected = id
					break
				}
			}
		}
		if selected == "" {
			d := dives[automatic[0]]
			date, _ := time.Parse(time.DateOnly, d.date)
			stamp := nowString()
			selected = newMediaID()
			t := diveTrip{ID: selected, Year: date.Year(), Country: countryByCode(d.code), Automatic: true, anchor: sql.NullInt64{Int64: d.id, Valid: true}, CreatedAt: stamp, UpdatedAt: stamp}
			trips[selected] = t
			created[selected] = true
			tripOrder = append(tripOrder, selected)
		}
		used[selected] = true
		for _, i := range automatic {
			desired[dives[i].id] = selected
		}
	}
	// New trips must exist before membership foreign keys are updated.
	for _, id := range tripOrder {
		t := trips[id]
		if created[id] {
			if err := saveTripTx(ctx, s, tx, t); err != nil {
				return err
			}
		}
	}
	for i := range dives {
		d := &dives[i]
		if d.mode != "auto" {
			continue
		}
		next := desired[d.id]
		if d.trip != next {
			var value any
			if next != "" {
				value = next
			}
			if _, err := tx.ExecContext(ctx, s.bind(`UPDATE dives SET trip_id=? WHERE id=? AND trip_assignment='auto'`), value, d.id); err != nil {
				return err
			}
			d.trip = next
		}
	}
	type extent struct{ from, to, code string }
	extents := map[string]extent{}
	for _, d := range dives {
		if d.trip == "" {
			continue
		}
		e, exists := extents[d.trip]
		if !exists {
			e = extent{from: d.date, code: d.code}
		}
		e.to = d.date
		extents[d.trip] = e
	}
	for _, id := range tripOrder {
		t := trips[id]
		if t.deleted {
			continue
		}
		old := t
		e := extents[id]
		t.StartDate, t.EndDate = e.from, e.to
		if !t.YearOverridden && len(e.from) >= 4 {
			date, err := time.Parse(time.DateOnly, e.from)
			if err == nil {
				t.Year = date.Year()
			}
		}
		if !t.CountryOverridden && e.code != "" {
			t.Country = countryByCode(e.code)
		}
		if !t.NameOverridden {
			fr, en := "Voyage", "Trip"
			if t.Country != nil {
				fr, en = t.Country.NameFR, t.Country.NameEN
			}
			t.Name = fmt.Sprintf("%s %d", fr, t.Year)
			t.NameEN = fmt.Sprintf("%s %d", en, t.Year)
		}
		if t.Name != old.Name || t.NameEN != old.NameEN || t.StartDate != old.StartDate || t.EndDate != old.EndDate || t.Year != old.Year || fmt.Sprint(tripCountryCode(t)) != fmt.Sprint(tripCountryCode(old)) {
			t.UpdatedAt = nowString()
			if err := saveTripTx(ctx, s, tx, t); err != nil {
				return err
			}
		}
	}
	return nil
}

func closeRows(rows *sql.Rows) error {
	err := rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func (s *sqlStore) homeCountries(ctx context.Context) ([]homeCountry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,country_code,from_date,to_date FROM home_countries ORDER BY from_date,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []homeCountry{}
	for rows.Next() {
		var h homeCountry
		var code string
		var to sql.NullString
		if err := rows.Scan(&h.ID, &code, &h.From, &to); err != nil {
			return nil, err
		}
		h.Country = countryByCode(code)
		h.To = nullStringPointer(to)
		items = append(items, h)
	}
	return items, rows.Err()
}

func (s *sqlStore) saveHomeCountry(ctx context.Context, id, code, from string, to *string, create bool) (homeCountry, error) {
	tx, err := s.beginTravelTx(ctx)
	if err != nil {
		return homeCountry{}, err
	}
	defer tx.Rollback()
	var overlap int
	err = tx.QueryRowContext(ctx, s.bind(`SELECT COUNT(*) FROM home_countries WHERE id<>? AND (CAST(? AS TEXT) IS NULL OR from_date<=?) AND (to_date IS NULL OR to_date>=?)`), id, to, to, from).Scan(&overlap)
	if err != nil {
		return homeCountry{}, err
	}
	if overlap > 0 {
		return homeCountry{}, errResidenceOverlap
	}
	if create {
		_, err = tx.ExecContext(ctx, s.bind(`INSERT INTO home_countries(id,country_code,from_date,to_date) VALUES(?,?,?,?)`), id, code, from, to)
	} else {
		var r sql.Result
		r, err = tx.ExecContext(ctx, s.bind(`UPDATE home_countries SET country_code=?,from_date=?,to_date=? WHERE id=?`), code, from, to, id)
		if err == nil {
			n, _ := r.RowsAffected()
			if n == 0 {
				return homeCountry{}, sql.ErrNoRows
			}
		}
	}
	if err != nil {
		return homeCountry{}, err
	}
	if err := reconcileTripsTx(ctx, s, tx); err != nil {
		return homeCountry{}, err
	}
	if err := tx.Commit(); err != nil {
		return homeCountry{}, err
	}
	return homeCountry{ID: id, Country: countryByCode(code), From: from, To: to}, nil
}

type tripPatch struct {
	Name            json.RawMessage `json:"name"`
	NameEN          *string         `json:"name_en"`
	Year            json.RawMessage `json:"year"`
	Country         json.RawMessage `json:"country_code"`
	Subtitle        *string         `json:"subtitle"`
	Description     *string         `json:"description"`
	Rating          json.RawMessage `json:"rating"`
	Cruise          *bool           `json:"cruise"`
	OperatorName    *string         `json:"operator_name"`
	OperatorWebsite *string         `json:"operator_website"`
	OperatorComment *string         `json:"operator_comment"`
	Price           *string         `json:"price"`
	Cover           json.RawMessage `json:"cover_media_id"`
	DiveIDs         *[]int64        `json:"dive_ids"`
}

func applyTripPatch(t *diveTrip, p tripPatch, create bool) error {
	invalid := func(message string) error { return fmt.Errorf("%w: %s", errInvalidTrip, message) }
	if !create && len(p.Name) == 0 && p.NameEN == nil && len(p.Year) == 0 && len(p.Country) == 0 && p.Subtitle == nil && p.Description == nil && len(p.Rating) == 0 && p.Cruise == nil && p.OperatorName == nil && p.OperatorWebsite == nil && p.OperatorComment == nil && p.Price == nil && len(p.Cover) == 0 && p.DiveIDs == nil {
		return invalid("provide at least one trip field")
	}
	if len(p.Name) > 0 {
		var name *string
		if err := json.Unmarshal(p.Name, &name); err != nil {
			return invalid("name must be a string or null")
		}
		t.NameOverridden = name != nil
		t.NameEN = ""
		if name != nil {
			if strings.TrimSpace(*name) == "" || len(*name) > 1024 {
				return invalid("name must contain 1 to 1024 bytes")
			}
			t.Name = *name
		}
	}
	if p.NameEN != nil {
		if len(*p.NameEN) > 1024 {
			return invalid("name_en exceeds 1024 bytes")
		}
		if !t.NameOverridden {
			return invalid("name_en requires a custom name")
		}
		t.NameEN = *p.NameEN
	}
	if len(p.Year) > 0 {
		var year *int
		if err := json.Unmarshal(p.Year, &year); err != nil || (year != nil && (*year < 1 || *year > 9999)) {
			return invalid("year must be 1 to 9999 or null")
		}
		t.YearOverridden = year != nil
		if year != nil {
			t.Year = *year
		}
	}
	if len(p.Country) > 0 {
		var value *string
		if err := json.Unmarshal(p.Country, &value); err != nil {
			return invalid("country_code must be a country or null")
		}
		t.CountryOverridden = value != nil
		if value != nil {
			c, err := resolveCountry(*value)
			if err != nil {
				return invalid(err.Error())
			}
			t.Country = c
		}
	}
	if create && (t.Year == 0 || t.Country == nil) {
		return invalid("provide year and country_code when creating a trip")
	}
	for _, field := range []struct {
		value       *string
		destination *string
		limit       int
		name        string
	}{
		{p.Subtitle, &t.Subtitle, 4096, "subtitle"}, {p.Description, &t.Description, 32768, "description"}, {p.OperatorName, &t.OperatorName, 1024, "operator_name"},
		{p.OperatorWebsite, &t.OperatorWebsite, 2048, "operator_website"}, {p.OperatorComment, &t.OperatorComment, 8192, "operator_comment"}, {p.Price, &t.Price, 4096, "price"},
	} {
		if field.value != nil {
			if len(*field.value) > field.limit {
				return invalid(field.name + " is too long")
			}
			*field.destination = *field.value
		}
	}
	if p.Cruise != nil {
		t.Cruise = *p.Cruise
	}
	if t.OperatorWebsite != "" {
		u, err := url.Parse(t.OperatorWebsite)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil {
			return invalid("operator_website must be an HTTP or HTTPS URL without credentials")
		}
	}
	if len(p.Rating) > 0 {
		var value *float64
		if err := json.Unmarshal(p.Rating, &value); err != nil || (value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 || *value > 5)) {
			return invalid("rating must be 0 to 5 or null")
		}
		t.Rating = value
	}
	if len(p.Cover) > 0 {
		if err := json.Unmarshal(p.Cover, &t.CoverMediaID); err != nil || (t.CoverMediaID != nil && *t.CoverMediaID == "") {
			return invalid("cover_media_id must be a photo ID or null")
		}
	}
	return nil
}

func (s *sqlStore) patchTrip(ctx context.Context, id string, p tripPatch, create bool) (diveTrip, error) {
	tx, err := s.beginTravelTx(ctx)
	if err != nil {
		return diveTrip{}, err
	}
	defer tx.Rollback()
	stamp := nowString()
	t := diveTrip{ID: id, CreatedAt: stamp, UpdatedAt: stamp, edited: true}
	if !create {
		t, err = scanTrip(tx.QueryRowContext(ctx, s.bind(`SELECT `+tripColumns+` FROM dive_trips WHERE id=? AND deleted=0`), id))
		if err != nil {
			return t, err
		}
	}
	if err := applyTripPatch(&t, p, create); err != nil {
		return t, err
	}
	t.edited = true
	t.UpdatedAt = stamp
	if t.CoverMediaID != nil {
		var count int
		if err := tx.QueryRowContext(ctx, s.bind(`SELECT COUNT(*) FROM trip_media WHERE id=? AND trip_id=? AND kind='photo'`), *t.CoverMediaID, id).Scan(&count); err != nil {
			return t, err
		}
		if count != 1 {
			return t, fmt.Errorf("%w: cover must be a photo owned by this trip", errInvalidTrip)
		}
	}
	if err := saveTripTx(ctx, s, tx, t); err != nil {
		return t, err
	}
	if p.DiveIDs != nil {
		seen := map[int64]bool{}
		for _, diveID := range *p.DiveIDs {
			if diveID <= 0 || seen[diveID] {
				return t, fmt.Errorf("%w: dive_ids must contain unique positive IDs", errInvalidTrip)
			}
			seen[diveID] = true
			var count int
			if err := tx.QueryRowContext(ctx, s.bind(`SELECT COUNT(*) FROM dives WHERE id=?`), diveID).Scan(&count); err != nil {
				return t, err
			}
			if count != 1 {
				return t, errDiveNotFound
			}
		}
		if _, err := tx.ExecContext(ctx, s.bind(`UPDATE dives SET trip_id=NULL,trip_assignment='excluded' WHERE trip_id=?`), id); err != nil {
			return t, err
		}
		for diveID := range seen {
			if _, err := tx.ExecContext(ctx, s.bind(`UPDATE dives SET trip_id=?,trip_assignment='manual' WHERE id=?`), id, diveID); err != nil {
				return t, err
			}
		}
	}
	if err := reconcileTripsTx(ctx, s, tx); err != nil {
		return t, err
	}
	if err := tx.Commit(); err != nil {
		return t, err
	}
	s.translationsChanged()
	return s.trip(ctx, id)
}

func (s *sqlStore) trip(ctx context.Context, id string) (diveTrip, error) {
	t, err := scanTrip(s.db.QueryRowContext(ctx, s.bind(`SELECT `+tripColumns+` FROM dive_trips WHERE id=? AND deleted=0`), id))
	if err != nil {
		return t, err
	}
	err = s.db.QueryRowContext(ctx, s.bind(`SELECT COUNT(*) FROM dives WHERE trip_id=?`), id).Scan(&t.DiveCount)
	if err != nil {
		return t, err
	}
	if t.CoverMediaID != nil {
		m, err := s.ownedMediaByID(ctx, mediaOwner{TripID: id}, *t.CoverMediaID)
		if err != nil {
			return t, err
		}
		t.CoverPhoto = &m
	}
	return t, nil
}

func (s *sqlStore) deleteTrip(ctx context.Context, id string) error {
	tx, err := s.beginTravelTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := tx.ExecContext(ctx, s.bind(`UPDATE dive_trips SET deleted=1,updated_at=? WHERE id=? AND deleted=0`), nowString(), id)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return errTripNotFound
	}
	if _, err := tx.ExecContext(ctx, s.bind(`UPDATE dives SET trip_id=NULL,trip_assignment='excluded' WHERE trip_id=?`), id); err != nil {
		return err
	}
	if err := reconcileTripsTx(ctx, s, tx); err != nil {
		return err
	}
	return tx.Commit()
}
