package main

import (
	"context"
	"database/sql"
	"errors"
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
	var patch divePatch
	if !decodeRequest(w, r, &patch) {
		return
	}
	if s.store.media != nil {
		mediaDeadlines(w)
		if err := s.store.media.lock(r.Context()); err != nil {
			s.internalError(w, r, err)
			return
		}
		defer s.store.media.unlock()
	}
	item, err := s.store.patchDiveTravel(r.Context(), id, patch)
	switch {
	case errors.Is(err, errInvalidCover), errors.Is(err, errInvalidDivePatch):
		writeAPIError(w, 400, "invalid_request", err.Error())
	case errors.Is(err, errDiveNotFound), errors.Is(err, errTripNotFound):
		writeAPIError(w, 404, "not_found", err.Error())
	case err != nil:
		s.internalError(w, r, err)
	default:
		if s.logger != nil {
			s.logger.Info("dive metadata updated", "activity_id", id)
		}
		s.writeTranslatedJSON(w, r, 200, &item)
	}
}
