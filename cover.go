package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

var errInvalidCover = errors.New("cover_media_id must reference a photo attached to this dive")

// Select and update in one statement so invalid selections never change the old
// cover. The foreign key clears this choice when scanning or deleting the photo.
func (s *sqlStore) setDiveCover(ctx context.Context, id int64, mediaID *string) (diveSummary, error) {
	query := `UPDATE dives SET cover_media_id = NULL WHERE id = ? RETURNING id`
	args := []any{id}
	if mediaID != nil {
		query = `UPDATE dives SET cover_media_id = ? WHERE id = ? AND EXISTS (
			SELECT 1 FROM dive_media WHERE id = ? AND dive_id = dives.id AND kind = 'photo'
		) RETURNING id`
		args = []any{*mediaID, id, *mediaID}
	}
	var updated int64
	err := s.db.QueryRowContext(ctx, s.bind(query), args...).Scan(&updated)
	if errors.Is(err, sql.ErrNoRows) {
		exists, lookupErr := s.diveExists(ctx, id)
		if lookupErr != nil {
			return diveSummary{}, lookupErr
		}
		if !exists {
			return diveSummary{}, errDiveNotFound
		}
		return diveSummary{}, errInvalidCover
	}
	if err != nil {
		return diveSummary{}, err
	}
	item, err := scanDiveSummary(s.db.QueryRowContext(ctx, s.bind(`SELECT `+diveSummaryColumns+` `+diveSummaryFrom+` WHERE dives.id = ?`), id))
	if err != nil {
		return item, err
	}
	return item, nil
}

func (s *apiServer) patchDive(w http.ResponseWriter, r *http.Request) {
	id, ok := diveID(w, r)
	if !ok {
		return
	}
	var patch struct {
		CoverMediaID json.RawMessage `json:"cover_media_id"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&patch); err != nil || len(patch.CoverMediaID) == 0 {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "provide cover_media_id as a photo ID or null to clear the cover")
		return
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "request must contain one JSON object")
		return
	}
	var mediaID *string
	if err := json.Unmarshal(patch.CoverMediaID, &mediaID); err != nil || (mediaID != nil && *mediaID == "") {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "cover_media_id must be a nonempty photo ID or null")
		return
	}
	// Serialize selection with media deletions/scans in the running server.
	if s.store.media != nil {
		mediaDeadlines(w)
		if err := s.store.media.lock(r.Context()); err != nil {
			s.internalError(w, r, err)
			return
		}
		defer s.store.media.unlock()
	}
	item, err := s.store.setDiveCover(r.Context(), id, mediaID)
	switch {
	case errors.Is(err, errInvalidCover):
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, errDiveNotFound):
		writeAPIError(w, http.StatusNotFound, "not_found", "dive not found")
	case err != nil:
		s.internalError(w, r, err)
	default:
		if s.logger != nil {
			coverID := ""
			if item.CoverMediaID != nil {
				coverID = *item.CoverMediaID
			}
			s.logger.Info("dive cover updated", "activity_id", id, "cover_media_id", coverID)
		}
		writeJSON(w, http.StatusOK, item)
	}
}
