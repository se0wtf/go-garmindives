package app

import (
	"errors"
	"io/fs"
	"mime"
	"net/http"
	"strconv"

	httpx "github.com/se0wtf/go-garmindives/internal/httpx"
	identity "github.com/se0wtf/go-garmindives/internal/identity"
)

func (s *apiServer) registerPendingRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/admin/media/pending", s.listPending)
	mux.HandleFunc("GET /api/v1/admin/media/pending/status", s.pendingStatus)
	mux.HandleFunc("POST /api/v1/admin/media/pending/scan", s.scanPending)
	mux.HandleFunc("GET /api/v1/admin/media/pending/{pendingID}", s.getPending)
	mux.HandleFunc("GET /api/v1/admin/media/pending/{pendingID}/thumbnail", s.pendingThumbnail)
	mux.HandleFunc("GET /api/v1/admin/media/pending/{pendingID}/file", s.pendingFile)
	mux.HandleFunc("POST /api/v1/admin/media/pending/{pendingID}/assign", s.assignPending)
	mux.HandleFunc("DELETE /api/v1/admin/media/pending/{pendingID}", s.deletePending)
}

type pendingListResponse struct {
	Items      []pendingItem `json:"items"`
	NextCursor string        `json:"next_cursor,omitempty"`
}
type pendingStatusResponse struct {
	Total               int64             `json:"total"`
	Reasons             map[string]int64  `json:"reasons"`
	Directory           string            `json:"directory"`
	ScanIntervalSeconds float64           `json:"scan_interval_seconds"`
	MatchWindowSeconds  int               `json:"match_window_seconds"`
	SettleSeconds       int               `json:"settle_seconds"`
	Scan                pendingScanStatus `json:"scan"`
}

func (s *apiServer) pendingService(w http.ResponseWriter, r *http.Request) *pendingService {
	if s.store.pending == nil {
		httpx.WriteAPIError(w, 503, "pending_unavailable", "pending media service is unavailable")
		return nil
	}
	return s.store.pending
}
func (s *apiServer) pendingRequest(w http.ResponseWriter, r *http.Request) (*pendingService, pendingItem, bool) {
	p := s.pendingService(w, r)
	if p == nil {
		return nil, pendingItem{}, false
	}
	id := r.PathValue("pendingID")
	if !identity.ValidID(id) {
		httpx.WriteAPIError(w, 400, "invalid_request", "invalid pending media ID")
		return p, pendingItem{}, false
	}
	item, err := s.store.pendingByID(r.Context(), id)
	if err != nil {
		s.pendingError(w, r, err)
		return p, item, false
	}
	return p, item, true
}
func (s *apiServer) pendingError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, errPendingNotFound), errors.Is(err, fs.ErrNotExist):
		httpx.WriteAPIError(w, 404, "not_found", "pending media not found")
	case errors.Is(err, errDiveNotFound):
		httpx.WriteAPIError(w, 404, "not_found", "dive not found")
	case errors.Is(err, errPendingChanged):
		httpx.WriteAPIError(w, 409, "source_changed", err.Error())
	case errors.Is(err, errPendingAssigned):
		httpx.WriteAPIError(w, 409, "already_assigned", err.Error())
	case errors.Is(err, errMediaConflict):
		httpx.WriteAPIError(w, 409, "filename_conflict", "the destination filename is already in use; choose another dive or resolve the filename conflict")
	default:
		s.internalError(w, r, err)
	}
}

var pendingReasons = map[string]bool{"waiting_for_file": true, "missing_capture_time": true, "no_matching_dive": true, "multiple_matches": true, "ambiguous_time": true, "metadata_error": true, "filename_conflict": true, "processing_error": true, "source_changed": true, "assigning": true, "matched": true}

func (s *apiServer) listPending(w http.ResponseWriter, r *http.Request) {
	if s.pendingService(w, r) == nil {
		return
	}
	limit, err := pageLimit(r, 50, 100)
	if err != nil {
		httpx.WriteAPIError(w, 400, "invalid_request", err.Error())
		return
	}
	reason := r.URL.Query().Get("reason")
	if reason != "" && !pendingReasons[reason] {
		httpx.WriteAPIError(w, 400, "invalid_request", "unknown pending reason")
		return
	}
	var cursor mediaCursor
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		if err := decodeCursor(raw, &cursor); err != nil || !validMediaFilename(cursor.Filename) || !identity.ValidID(cursor.ID) {
			httpx.WriteAPIError(w, 400, "invalid_cursor", "cursor is invalid")
			return
		}
	}
	rows, err := s.store.db.QueryContext(r.Context(), s.store.bind(`SELECT `+pendingColumns+` FROM pending_media
		WHERE (?='' OR reason=?) AND (filename>? OR (filename=? AND id>?)) ORDER BY filename,id LIMIT ?`), reason, reason, cursor.Filename, cursor.Filename, cursor.ID, limit+1)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	items := []pendingItem{}
	for rows.Next() {
		item, err := scanPending(rows)
		if err != nil {
			rows.Close()
			s.internalError(w, r, err)
			return
		}
		items = append(items, item)
	}
	if err := closeRows(rows); err != nil {
		s.internalError(w, r, err)
		return
	}
	response := pendingListResponse{Items: items}
	if len(items) > limit {
		response.Items = items[:limit]
		last := response.Items[len(response.Items)-1]
		response.NextCursor, err = encodeCursor(mediaCursor{Filename: last.Filename, ID: last.ID})
		if err != nil {
			s.internalError(w, r, err)
			return
		}
	}
	httpx.WriteJSON(w, 200, response)
}
func (s *apiServer) getPending(w http.ResponseWriter, r *http.Request) {
	_, item, ok := s.pendingRequest(w, r)
	if ok {
		httpx.WriteJSON(w, 200, item)
	}
}
func (s *apiServer) pendingStatus(w http.ResponseWriter, r *http.Request) {
	p := s.pendingService(w, r)
	if p == nil {
		return
	}
	response := pendingStatusResponse{Reasons: map[string]int64{}, Directory: p.directory, ScanIntervalSeconds: p.interval.Seconds(), MatchWindowSeconds: int(pendingMatchWindow.Seconds()), SettleSeconds: int(pendingSettleTime.Seconds()), Scan: p.scanStatus()}
	rows, err := s.store.db.QueryContext(r.Context(), `SELECT reason,COUNT(*) FROM pending_media GROUP BY reason`)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	for rows.Next() {
		var reason string
		var count int64
		if err := rows.Scan(&reason, &count); err != nil {
			rows.Close()
			s.internalError(w, r, err)
			return
		}
		response.Reasons[reason] = count
		response.Total += count
	}
	if err := closeRows(rows); err != nil {
		s.internalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, 200, response)
}
func (s *apiServer) scanPending(w http.ResponseWriter, r *http.Request) {
	p := s.pendingService(w, r)
	if p == nil {
		return
	}
	force := false
	if raw := r.URL.Query().Get("force"); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			httpx.WriteAPIError(w, 400, "invalid_request", "force must be true or false")
			return
		}
		force = value
	}
	p.Signal(force)
	httpx.WriteJSON(w, 202, map[string]string{"status": "queued"})
}
func (s *apiServer) assignPending(w http.ResponseWriter, r *http.Request) {
	p, item, ok := s.pendingRequest(w, r)
	if !ok {
		return
	}
	var request struct {
		DiveID int64 `json:"dive_id"`
	}
	if !decodeRequest(w, r, &request) {
		return
	}
	if request.DiveID <= 0 {
		httpx.WriteAPIError(w, 400, "invalid_request", "dive_id must be positive")
		return
	}
	m, err := p.Assign(r.Context(), item.ID, request.DiveID)
	if err != nil {
		s.pendingError(w, r, err)
		return
	}
	w.Header().Set("Location", m.owner().apiPath()+"/media/"+m.ID)
	s.writeTranslatedJSON(w, r, 200, &m)
}
func (s *apiServer) deletePending(w http.ResponseWriter, r *http.Request) {
	p, item, ok := s.pendingRequest(w, r)
	if !ok {
		return
	}
	if err := p.Delete(r.Context(), item.ID); err != nil {
		s.pendingError(w, r, err)
		return
	}
	w.WriteHeader(204)
}
func (s *apiServer) pendingThumbnail(w http.ResponseWriter, r *http.Request) {
	s.servePending(w, r, true)
}
func (s *apiServer) pendingFile(w http.ResponseWriter, r *http.Request) { s.servePending(w, r, false) }
func (s *apiServer) servePending(w http.ResponseWriter, r *http.Request, thumbnail bool) {
	p, item, ok := s.pendingRequest(w, r)
	if !ok {
		return
	}
	input, err := p.openSource(r.Context(), item, true)
	if err != nil {
		s.pendingError(w, r, err)
		return
	}
	defer input.Close()
	m := p.previewMedia(item)
	contentType, name, etag := m.ContentType, item.Filename, item.SHA256
	file := input
	if thumbnail {
		if err := p.files.lock(r.Context()); err != nil {
			s.pendingError(w, r, err)
			return
		}
		defer p.files.unlock()
		if stat, err := p.files.root.Stat(m.thumbnailPath()); err != nil || stat.Size() == 0 {
			if err := p.files.makeThumbnail(r.Context(), input, &m); err != nil {
				s.internalError(w, r, err)
				return
			}
		}
		thumb, err := p.files.root.Open(m.thumbnailPath())
		if err != nil {
			s.pendingError(w, r, err)
			return
		}
		defer thumb.Close()
		file = thumb
		if m.Kind == "photo" {
			file, err = p.files.jpegFallback(r.Context(), thumb, m.ThumbnailKey)
			if err != nil {
				s.internalError(w, r, err)
				return
			}
			defer file.Close()
		}
		contentType, name = "image/jpeg", name+".thumbnail.jpg"
	} else {
		format := r.URL.Query().Get("format")
		if format != "" && format != "auto" && format != "original" && format != "jpeg" {
			httpx.WriteAPIError(w, 400, "invalid_request", "format must be auto, original, or jpeg")
			return
		}
		if format == "" || format == "auto" {
			w.Header().Add("Vary", "Accept")
		}
		if m.Kind == "photo" && contentType != "image/jpeg" && (format == "jpeg" || format != "original" && needsPhotoFallback(contentType, r.Header.Get("Accept"))) {
			file, err = p.files.jpegFallback(r.Context(), input, photoFallbackSource(contentType, "pending-original-v1:"+item.SHA256))
			if err != nil {
				s.internalError(w, r, err)
				return
			}
			defer file.Close()
			contentType, name = "image/jpeg", name+".jpg"
		}
	}
	if file != input {
		etag, err = hashPhoto(r.Context(), file)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
	}
	if _, err := file.Seek(0, 0); err != nil {
		s.internalError(w, r, err)
		return
	}
	info, err := file.Stat()
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": name}))
	w.Header().Set("ETag", `"`+etag+`"`)
	http.ServeContent(w, r, name, info.ModTime(), file)
}
