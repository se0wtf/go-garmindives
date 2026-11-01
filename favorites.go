package main

import (
	"context"
	"net/http"
)

func (s *sqlStore) favoriteMedia(ctx context.Context, tripID string, limit int, afterName, afterID string) ([]mediaItem, bool, error) {
	query := `SELECT ` + mediaColumns + ` FROM dive_media WHERE favorite=1 AND (filename>? OR (filename=? AND id>?))`
	args := []any{afterName, afterName, afterID}
	if tripID != "" {
		query += ` AND EXISTS (SELECT 1 FROM dives WHERE dives.id=dive_media.dive_id AND dives.trip_id=?)`
		args = append(args, tripID)
	}
	query += ` ORDER BY filename,id LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, s.bind(query), args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	items := []mediaItem{}
	for rows.Next() {
		m, err := scanMedia(rows)
		if err != nil {
			return nil, false, err
		}
		items = append(items, m)
	}
	more := len(items) > limit
	if more {
		items = items[:limit]
	}
	return items, more, rows.Err()
}
func (s *apiServer) listFavoriteMedia(w http.ResponseWriter, r *http.Request) {
	limit, err := pageLimit(r, 50, 100)
	if err != nil {
		writeAPIError(w, 400, "invalid_request", err.Error())
		return
	}
	var cursor mediaCursor
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		if decodeCursor(raw, &cursor) != nil || !validMediaFilename(cursor.Filename) || cursor.ID == "" {
			writeAPIError(w, 400, "invalid_cursor", "invalid media cursor")
			return
		}
	}
	tripID := r.URL.Query().Get("trip_id")
	if tripID != "" {
		if !validTripID(tripID) {
			writeAPIError(w, 400, "invalid_request", "invalid trip_id")
			return
		}
		exists, err := s.store.ownerExists(r.Context(), mediaOwner{TripID: tripID})
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		if !exists {
			s.travelError(w, r, errTripNotFound)
			return
		}
	}
	items, more, err := s.store.favoriteMedia(r.Context(), tripID, limit, cursor.Filename, cursor.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	response := mediaListResponse{Items: items}
	if more {
		last := items[len(items)-1]
		response.NextCursor, _ = encodeCursor(mediaCursor{Filename: last.Filename, ID: last.ID})
	}
	s.writeTranslatedJSON(w, r, 200, &response)
}
