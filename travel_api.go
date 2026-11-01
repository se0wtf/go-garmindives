package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
)

func (s *apiServer) registerTravelRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/countries", s.listCountries)
	mux.HandleFunc("GET /api/v1/countries/status", s.countryStatus)
	mux.HandleFunc("POST /api/v1/countries/retry", s.retryCountries)
	mux.HandleFunc("GET /api/v1/home-countries", s.listHomeCountries)
	mux.HandleFunc("POST /api/v1/home-countries", s.createHomeCountry)
	mux.HandleFunc("GET /api/v1/home-countries/{homeID}", s.getHomeCountry)
	mux.HandleFunc("PATCH /api/v1/home-countries/{homeID}", s.updateHomeCountry)
	mux.HandleFunc("DELETE /api/v1/home-countries/{homeID}", s.deleteHomeCountry)
	mux.HandleFunc("GET /api/v1/trips", s.listTrips)
	mux.HandleFunc("POST /api/v1/trips", s.createTrip)
	mux.HandleFunc("POST /api/v1/trips/rebuild", s.rebuildTrips)
	mux.HandleFunc("GET /api/v1/trips/{tripID}", s.getTrip)
	mux.HandleFunc("PATCH /api/v1/trips/{tripID}", s.updateTrip)
	mux.HandleFunc("DELETE /api/v1/trips/{tripID}", s.removeTrip)
	mux.HandleFunc("GET /api/v1/trips/{tripID}/dives", s.listTripDives)
	mux.HandleFunc("PUT /api/v1/trips/{tripID}/dives/{id}", s.linkTripDive)
	mux.HandleFunc("DELETE /api/v1/trips/{tripID}/dives/{id}", s.unlinkTripDive)
}

func decodeRequest(w http.ResponseWriter, r *http.Request, value any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 512<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		writeAPIError(w, 400, "invalid_request", "invalid JSON request")
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeAPIError(w, 400, "invalid_request", "request must contain one JSON object")
		return false
	}
	return true
}
func (s *apiServer) travelError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, errTripNotFound), errors.Is(err, errDiveNotFound), errors.Is(err, sql.ErrNoRows):
		writeAPIError(w, 404, "not_found", "trip, dive, or home-country period not found")
	case errors.Is(err, errResidenceOverlap):
		writeAPIError(w, 409, "conflict", err.Error())
	case errors.Is(err, errInvalidTrip):
		writeAPIError(w, 400, "invalid_request", err.Error())
	default:
		s.internalError(w, r, err)
	}
}
func (s *apiServer) listCountries(w http.ResponseWriter, r *http.Request) {
	items := []country{}
	q := searchText(r.URL.Query().Get("q"))
	for _, c := range countryCatalog {
		if q == "" || strings.Contains(searchText(c.Code+" "+c.NameFR+" "+c.NameEN), q) {
			items = append(items, c)
		}
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

type countryLookupStatus struct {
	Enabled                bool    `json:"enabled"`
	Pending                int64   `json:"pending"`
	Ready                  int64   `json:"ready"`
	NotFound               int64   `json:"not_found"`
	Failed                 int64   `json:"failed"`
	RequestIntervalSeconds float64 `json:"request_interval_seconds"`
	Attribution            string  `json:"attribution"`
	BlockedReason          string  `json:"blocked_reason,omitempty"`
}

func (s *apiServer) countryStatus(w http.ResponseWriter, r *http.Request) {
	status := countryLookupStatus{Attribution: geocodingAttribution}
	if s.store.geocoder != nil {
		status.Enabled = s.store.geocoder.client != nil
		status.RequestIntervalSeconds = s.store.geocoder.config.interval.Seconds()
		if reason := s.store.geocoder.blocked.Load(); reason != nil {
			status.BlockedReason = *reason
		}
	}
	err := s.store.db.QueryRowContext(r.Context(), `SELECT COALESCE(SUM(CASE WHEN status='pending' THEN 1 ELSE 0 END),0),COALESCE(SUM(CASE WHEN status='ready' THEN 1 ELSE 0 END),0),COALESCE(SUM(CASE WHEN status='not_found' THEN 1 ELSE 0 END),0),COALESCE(SUM(CASE WHEN status='error' THEN 1 ELSE 0 END),0) FROM country_lookup_cache WHERE EXISTS (SELECT 1 FROM dives d WHERE d.country_lookup_key=country_lookup_cache.lookup_key AND d.country_override IS NULL)`).Scan(&status.Pending, &status.Ready, &status.NotFound, &status.Failed)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, 200, status)
}
func (s *apiServer) retryCountries(w http.ResponseWriter, r *http.Request) {
	if s.store.geocoder == nil || s.store.geocoder.client == nil {
		writeAPIError(w, 503, "geocoding_disabled", "enable GARMIN_GEOCODING_ENABLED and restart serve")
		return
	}
	_, err := s.store.db.ExecContext(r.Context(), `UPDATE country_lookup_cache SET status='pending',attempts=0,retry_at=0,last_error='' WHERE status IN ('error','not_found')`)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.store.geocoder.blocked.Store(nil)
	writeJSON(w, 202, map[string]string{"status": "queued"})
}
func (s *apiServer) listHomeCountries(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.homeCountries(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}
func (s *apiServer) findHomeCountry(r *http.Request) (homeCountry, error) {
	items, err := s.store.homeCountries(r.Context())
	if err != nil {
		return homeCountry{}, err
	}
	for _, h := range items {
		if h.ID == r.PathValue("homeID") {
			return h, nil
		}
	}
	return homeCountry{}, sql.ErrNoRows
}
func (s *apiServer) getHomeCountry(w http.ResponseWriter, r *http.Request) {
	h, err := s.findHomeCountry(r)
	if err != nil {
		s.travelError(w, r, err)
		return
	}
	writeJSON(w, 200, h)
}
func (s *apiServer) createHomeCountry(w http.ResponseWriter, r *http.Request) {
	s.writeHomeCountry(w, r, true)
}
func (s *apiServer) updateHomeCountry(w http.ResponseWriter, r *http.Request) {
	s.writeHomeCountry(w, r, false)
}
func (s *apiServer) writeHomeCountry(w http.ResponseWriter, r *http.Request, create bool) {
	var p struct {
		Country *string         `json:"country_code"`
		From    *string         `json:"from"`
		To      json.RawMessage `json:"to"`
	}
	if !decodeRequest(w, r, &p) {
		return
	}
	if !create && p.Country == nil && p.From == nil && len(p.To) == 0 {
		writeAPIError(w, 400, "invalid_request", "provide country_code, from, or to")
		return
	}
	h := homeCountry{ID: newMediaID()}
	if !create {
		var err error
		h, err = s.findHomeCountry(r)
		if err != nil {
			s.travelError(w, r, err)
			return
		}
	}
	if p.Country != nil {
		c, err := resolveCountry(*p.Country)
		if err != nil {
			writeAPIError(w, 400, "invalid_request", err.Error())
			return
		}
		h.Country = c
	}
	if p.From != nil {
		h.From = *p.From
	}
	if len(p.To) > 0 {
		if err := json.Unmarshal(p.To, &h.To); err != nil {
			writeAPIError(w, 400, "invalid_request", "to must be a date or null")
			return
		}
	}
	to := ""
	if h.To != nil {
		to = *h.To
	}
	if h.Country == nil || h.From == "" || (h.To != nil && to == "") {
		writeAPIError(w, 400, "invalid_request", "provide country_code and from; to is a date or null for an open-ended period")
		return
	}
	if err := validateDateRange(h.From, to); err != nil {
		writeAPIError(w, 400, "invalid_request", err.Error())
		return
	}
	h, err := s.store.saveHomeCountry(r.Context(), h.ID, h.Country.Code, h.From, h.To, create)
	if err != nil {
		s.travelError(w, r, err)
		return
	}
	status := 200
	if create {
		status = 201
		w.Header().Set("Location", "/api/v1/home-countries/"+h.ID)
	}
	writeJSON(w, status, h)
}
func (s *apiServer) deleteHomeCountry(w http.ResponseWriter, r *http.Request) {
	tx, err := s.store.beginTravelTx(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(r.Context(), s.store.bind(`DELETE FROM home_countries WHERE id=?`), r.PathValue("homeID"))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		s.travelError(w, r, sql.ErrNoRows)
		return
	}
	if err := reconcileTripsTx(r.Context(), s.store, tx); err != nil {
		s.internalError(w, r, err)
		return
	}
	if err := tx.Commit(); err != nil {
		s.internalError(w, r, err)
		return
	}
	w.WriteHeader(204)
}

type tripCursor struct {
	StartDate string `json:"start_date"`
	ID        string `json:"id"`
}
type tripListResponse struct {
	Items      []diveTrip `json:"items"`
	NextCursor string     `json:"next_cursor,omitempty"`
}
type tripDetail struct {
	Trip                diveTrip    `json:"trip"`
	DiveIDs             []int64     `json:"dive_ids"`
	Media               []mediaItem `json:"media"`
	FavoriteDiveMedia   []mediaItem `json:"favorite_dive_media"`
	FavoritesNextCursor string      `json:"favorites_next_cursor,omitempty"`
}

func (s *apiServer) listTrips(w http.ResponseWriter, r *http.Request) {
	limit, err := pageLimit(r, 50, 100)
	if err != nil {
		writeAPIError(w, 400, "invalid_request", err.Error())
		return
	}
	where := []string{`deleted=0 AND (automatic=0 OR edited=1 OR EXISTS(SELECT 1 FROM dives WHERE dives.trip_id=dive_trips.id) OR EXISTS(SELECT 1 FROM trip_media WHERE trip_media.trip_id=dive_trips.id))`}
	args := []any{}
	if q := r.URL.Query().Get("name"); q != "" {
		where = append(where, `search_name LIKE ? ESCAPE '\'`)
		args = append(args, "%"+escapeLikePattern(searchText(q))+"%")
	}
	if q := r.URL.Query().Get("year"); q != "" {
		year, err := strconv.Atoi(q)
		if err != nil || year < 1 || year > 9999 {
			writeAPIError(w, 400, "invalid_request", "year must be 1 to 9999")
			return
		}
		where = append(where, "year=?")
		args = append(args, year)
	}
	if q := r.URL.Query().Get("country"); q != "" {
		c, err := resolveCountry(q)
		if err != nil {
			writeAPIError(w, 400, "invalid_request", err.Error())
			return
		}
		where = append(where, "country_code=?")
		args = append(args, c.Code)
	}
	from, to := r.URL.Query().Get("from"), r.URL.Query().Get("to")
	if err := validateDateRange(from, to); err != nil {
		writeAPIError(w, 400, "invalid_request", err.Error())
		return
	}
	if from != "" {
		where = append(where, "end_date>=? AND start_date<>''")
		args = append(args, from)
	}
	if to != "" {
		where = append(where, "start_date<=? AND start_date<>''")
		args = append(args, to)
	}
	var minRating, maxRating *float64
	for _, field := range []struct {
		name, operator string
		destination    **float64
	}{{"min_rating", ">=", &minRating}, {"max_rating", "<=", &maxRating}} {
		if raw := r.URL.Query().Get(field.name); raw != "" {
			rating, err := strconv.ParseFloat(raw, 64)
			if err != nil || math.IsNaN(rating) || math.IsInf(rating, 0) || rating < 0 || rating > 5 {
				writeAPIError(w, 400, "invalid_request", field.name+" must be 0 to 5")
				return
			}
			*field.destination = &rating
			where = append(where, "rating"+field.operator+"?")
			args = append(args, rating)
		}
	}
	if minRating != nil && maxRating != nil && *minRating > *maxRating {
		writeAPIError(w, 400, "invalid_request", "min_rating must not exceed max_rating")
		return
	}
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		var cursor tripCursor
		if decodeCursor(raw, &cursor) != nil || !validTripID(cursor.ID) {
			writeAPIError(w, 400, "invalid_cursor", "invalid trip cursor")
			return
		}
		where = append(where, "(start_date<? OR (start_date=? AND id<?))")
		args = append(args, cursor.StartDate, cursor.StartDate, cursor.ID)
	}
	args = append(args, limit+1)
	rows, err := s.store.db.QueryContext(r.Context(), s.store.bind(`SELECT `+tripColumns+` FROM dive_trips WHERE `+strings.Join(where, " AND ")+` ORDER BY start_date DESC,id DESC LIMIT ?`), args...)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	items := []diveTrip{}
	for rows.Next() {
		t, err := scanTrip(rows)
		if err != nil {
			rows.Close()
			s.internalError(w, r, err)
			return
		}
		items = append(items, t)
	}
	if err := closeRows(rows); err != nil {
		s.internalError(w, r, err)
		return
	}
	response := tripListResponse{Items: items}
	if len(items) > limit {
		response.Items = items[:limit]
		last := response.Items[limit-1]
		response.NextCursor, _ = encodeCursor(tripCursor{StartDate: last.StartDate, ID: last.ID})
	}
	if err := s.store.hydrateTrips(r.Context(), response.Items); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.writeTranslatedJSON(w, r, 200, &response)
}
func (s *sqlStore) hydrateTrips(ctx context.Context, items []diveTrip) error {
	if len(items) == 0 {
		return nil
	}
	args := []any{}
	index := map[string]int{}
	covers := map[string]int{}
	for i, t := range items {
		args = append(args, t.ID)
		index[t.ID] = i
		if t.CoverMediaID != nil {
			covers[*t.CoverMediaID] = i
		}
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
	rows, err := s.db.QueryContext(ctx, s.bind(`SELECT trip_id,COUNT(*) FROM dives WHERE trip_id IN (`+marks+`) GROUP BY trip_id`), args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		var n int64
		if err := rows.Scan(&id, &n); err != nil {
			rows.Close()
			return err
		}
		items[index[id]].DiveCount = n
	}
	if err := closeRows(rows); err != nil {
		return err
	}
	if len(covers) == 0 {
		return nil
	}
	args = nil
	for id := range covers {
		args = append(args, id)
	}
	marks = strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
	owner := mediaOwner{TripID: "trip"}
	rows, err = s.db.QueryContext(ctx, s.bind(`SELECT `+owner.columns()+` FROM trip_media WHERE id IN (`+marks+`)`), args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		m, err := scanOwnedMedia(rows, owner)
		if err != nil {
			rows.Close()
			return err
		}
		if i, ok := covers[m.ID]; ok {
			items[i].CoverPhoto = &m
		}
	}
	return closeRows(rows)
}
func tripRequestID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("tripID")
	if !validTripID(id) {
		writeAPIError(w, 400, "invalid_request", "invalid trip ID")
		return "", false
	}
	return id, true
}
func (s *apiServer) getTrip(w http.ResponseWriter, r *http.Request) {
	id, ok := tripRequestID(w, r)
	if !ok {
		return
	}
	t, err := s.store.trip(r.Context(), id)
	if err != nil {
		s.travelError(w, r, err)
		return
	}
	response := tripDetail{Trip: t, DiveIDs: []int64{}}
	rows, err := s.store.db.QueryContext(r.Context(), s.store.bind(`SELECT id FROM dives WHERE trip_id=? ORDER BY local_date,start_time,id`), id)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	for rows.Next() {
		var diveID int64
		if err := rows.Scan(&diveID); err != nil {
			rows.Close()
			s.internalError(w, r, err)
			return
		}
		response.DiveIDs = append(response.DiveIDs, diveID)
	}
	if err := closeRows(rows); err != nil {
		s.internalError(w, r, err)
		return
	}
	response.Media, err = s.store.mediaForOwner(r.Context(), mediaOwner{TripID: id})
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	var more bool
	response.FavoriteDiveMedia, more, err = s.store.favoriteMedia(r.Context(), id, 100, "", "")
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if more {
		last := response.FavoriteDiveMedia[len(response.FavoriteDiveMedia)-1]
		response.FavoritesNextCursor, _ = encodeCursor(mediaCursor{Filename: last.Filename, ID: last.ID})
	}
	s.writeTranslatedJSON(w, r, 200, &response)
}
func (s *apiServer) createTrip(w http.ResponseWriter, r *http.Request) { s.writeTrip(w, r, true) }
func (s *apiServer) updateTrip(w http.ResponseWriter, r *http.Request) { s.writeTrip(w, r, false) }
func (s *apiServer) writeTrip(w http.ResponseWriter, r *http.Request, create bool) {
	id := newMediaID()
	if !create {
		var ok bool
		id, ok = tripRequestID(w, r)
		if !ok {
			return
		}
	}
	var p tripPatch
	if !decodeRequest(w, r, &p) {
		return
	}
	if len(p.Cover) > 0 && s.store.media != nil {
		if err := s.store.media.lock(r.Context()); err != nil {
			s.internalError(w, r, err)
			return
		}
		defer s.store.media.unlock()
	}
	t, err := s.store.patchTrip(r.Context(), id, p, create)
	if err != nil {
		s.travelError(w, r, err)
		return
	}
	status := 200
	if create {
		status = 201
		w.Header().Set("Location", "/api/v1/trips/"+id)
	}
	s.writeTranslatedJSON(w, r, status, &t)
}
func (s *apiServer) removeTrip(w http.ResponseWriter, r *http.Request) {
	id, ok := tripRequestID(w, r)
	if !ok {
		return
	}
	if err := s.store.deleteTrip(r.Context(), id); err != nil {
		s.travelError(w, r, err)
		return
	}
	w.WriteHeader(204)
}
func (s *apiServer) rebuildTrips(w http.ResponseWriter, r *http.Request) {
	if err := s.store.reconcileTravel(r.Context()); err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "rebuilt"})
}
func (s *apiServer) listTripDives(w http.ResponseWriter, r *http.Request) {
	id, ok := tripRequestID(w, r)
	if !ok {
		return
	}
	exists, err := s.store.ownerExists(r.Context(), mediaOwner{TripID: id})
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !exists {
		s.travelError(w, r, errTripNotFound)
		return
	}
	copy := r.Clone(r.Context())
	q := copy.URL.Query()
	q.Set("trip_id", id)
	copy.URL.RawQuery = q.Encode()
	s.listDives(w, copy)
}
func (s *apiServer) linkTripDive(w http.ResponseWriter, r *http.Request) {
	s.changeTripDive(w, r, true)
}
func (s *apiServer) unlinkTripDive(w http.ResponseWriter, r *http.Request) {
	s.changeTripDive(w, r, false)
}
func (s *apiServer) changeTripDive(w http.ResponseWriter, r *http.Request, link bool) {
	id, ok := tripRequestID(w, r)
	if !ok {
		return
	}
	dive, ok := diveID(w, r)
	if !ok {
		return
	}
	tx, err := s.store.beginTravelTx(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	defer tx.Rollback()
	var n int
	err = tx.QueryRowContext(r.Context(), s.store.bind(`SELECT COUNT(*) FROM dive_trips WHERE id=? AND deleted=0`), id).Scan(&n)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if n == 0 {
		s.travelError(w, r, errTripNotFound)
		return
	}
	query := `UPDATE dives SET trip_id=?,trip_assignment='manual' WHERE id=?`
	args := []any{id, dive}
	if !link {
		query = `UPDATE dives SET trip_id=NULL,trip_assignment='excluded' WHERE id=? AND trip_id=?`
		args = []any{dive, id}
	}
	result, err := tx.ExecContext(r.Context(), s.store.bind(query), args...)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		s.travelError(w, r, errDiveNotFound)
		return
	}
	if err := reconcileTripsTx(r.Context(), s.store, tx); err != nil {
		s.internalError(w, r, err)
		return
	}
	if err := tx.Commit(); err != nil {
		s.internalError(w, r, err)
		return
	}
	w.WriteHeader(204)
}
