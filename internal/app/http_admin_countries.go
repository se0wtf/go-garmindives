package app

import (
	"net/http"
	"strings"

	httpx "github.com/se0wtf/go-garmindives/internal/httpx"
)

type missingCountryLookup struct {
	Status      string   `json:"status"`
	Latitude    *float64 `json:"latitude"`
	Longitude   *float64 `json:"longitude"`
	ErrorCode   string   `json:"error_code,omitempty"`
	Attempts    int      `json:"attempts"`
	CheckedAtMS *int64   `json:"checked_at_ms"`
}

type missingCountryDive struct {
	Dive   diveSummary          `json:"dive"`
	Lookup missingCountryLookup `json:"lookup"`
}

type missingCountryListResponse struct {
	Items      []missingCountryDive `json:"items"`
	NextCursor string               `json:"next_cursor,omitempty"`
}

func (s *apiServer) registerCountryAdminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/admin/dives/missing-country", s.listDivesMissingCountry)
}

func (s *apiServer) listDivesMissingCountry(w http.ResponseWriter, r *http.Request) {
	status := strings.TrimSpace(r.URL.Query().Get("lookup_status"))
	switch status {
	case "", "all":
		status = "all"
	case "not_found", "error", "pending", "missing_coordinates":
	default:
		httpx.WriteAPIError(w, 400, "invalid_request", "lookup_status must be all, not_found, error, pending, or missing_coordinates")
		return
	}
	limit, err := pageLimit(r, defaultDivePageSize, maximumDivePageSize)
	if err != nil {
		httpx.WriteAPIError(w, 400, "invalid_request", err.Error())
		return
	}
	options := missingCountryOptions{Limit: limit, LookupStatus: status}
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		if err := decodeCursor(raw, &options.Cursor); err != nil || options.Cursor.ID <= 0 {
			httpx.WriteAPIError(w, 400, "invalid_cursor", "cursor is invalid")
			return
		}
	}
	items, more, err := s.store.DivesMissingCountry(r.Context(), options)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	response := missingCountryListResponse{Items: items}
	if more && len(items) > 0 {
		last := items[len(items)-1].Dive
		response.NextCursor, err = encodeCursor(diveCursor{LocalDate: last.LocalDate, StartTime: last.StartTime, ID: last.ID})
		if err != nil {
			s.internalError(w, r, err)
			return
		}
	}
	s.writeTranslatedJSON(w, r, 200, &response)
}
