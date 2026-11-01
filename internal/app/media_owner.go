package app

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	httpx "github.com/se0wtf/go-garmindives/internal/httpx"
	identity "github.com/se0wtf/go-garmindives/internal/identity"
	// Only these fixed table/column names are interpolated into SQL. Owner values
	// and filenames always use bound parameters and confined filesystem paths.
)

type mediaOwner struct {
	DiveID int64
	TripID string
}

func (o mediaOwner) table() string {
	if o.TripID != "" {
		return "trip_media"
	}
	return "dive_media"
}
func (o mediaOwner) column() string {
	if o.TripID != "" {
		return "trip_id"
	}
	return "dive_id"
}
func (o mediaOwner) columns() string { return strings.Replace(mediaColumns, "dive_id", o.column(), 1) }
func (o mediaOwner) value() any {
	if o.TripID != "" {
		return o.TripID
	}
	return o.DiveID
}
func (o mediaOwner) directory() string {
	if o.TripID != "" {
		return "trips/" + o.TripID
	}
	return strconv.FormatInt(o.DiveID, 10)
}
func (o mediaOwner) path(name string) string { return o.directory() + "/" + name }
func (o mediaOwner) apiPath() string {
	if o.TripID != "" {
		return "/api/v1/trips/" + o.TripID
	}
	return "/api/v1/dives/" + strconv.FormatInt(o.DiveID, 10)
}
func (m mediaItem) owner() mediaOwner { return mediaOwner{DiveID: m.DiveID, TripID: m.TripID} }
func (s *sqlStore) ownerExists(ctx context.Context, o mediaOwner) (bool, error) {
	if o.TripID == "" {
		return s.diveExists(ctx, o.DiveID)
	}
	var n int
	err := s.db.QueryRowContext(ctx, s.bind(`SELECT COUNT(*) FROM dive_trips WHERE id=? AND deleted=0`), o.TripID).Scan(&n)
	return n == 1, err
}
func (s *apiServer) mediaRequestOwner(w http.ResponseWriter, r *http.Request, check bool) (mediaOwner, bool) {
	o := mediaOwner{TripID: r.PathValue("tripID")}
	if o.TripID != "" {
		if !identity.ValidID(o.TripID) {
			httpx.WriteAPIError(w, 400, "invalid_request", "invalid trip ID")
			return o, false
		}
	} else {
		id, ok := diveID(w, r)
		if !ok {
			return o, false
		}
		o.DiveID = id
	}
	if check {
		exists, err := s.store.ownerExists(r.Context(), o)
		if err != nil {
			s.internalError(w, r, err)
			return o, false
		}
		if !exists {
			httpx.WriteAPIError(w, 404, "not_found", "dive or trip not found")
			return o, false
		}
	}
	return o, true
}
