package main

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"strconv"
	"time"
)

type mediaCursor struct {
	Filename string `json:"filename"`
	ID       string `json:"id"`
}

type mediaListResponse struct {
	Items      []mediaItem `json:"items"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

func (s *apiServer) registerMediaRoutes(mux *http.ServeMux) {
	if s.store.media == nil {
		return
	}
	mux.HandleFunc("GET /api/v1/dives/{id}/media", s.listMedia)
	mux.HandleFunc("POST /api/v1/dives/{id}/media", s.uploadMedia)
	mux.HandleFunc("GET /api/v1/dives/{id}/media/{mediaID}", s.getMedia)
	mux.HandleFunc("PATCH /api/v1/dives/{id}/media/{mediaID}", s.patchMedia)
	mux.HandleFunc("DELETE /api/v1/dives/{id}/media/{mediaID}", s.deleteMedia)
	mux.HandleFunc("GET /api/v1/dives/{id}/media/{mediaID}/thumbnail", s.getThumbnail)
	mux.HandleFunc("GET /assets/{id}/{filename}", s.getAsset)
	mux.HandleFunc("POST /api/v1/media/scan", s.scanMedia)
	mux.HandleFunc("GET /api/v1/media/status", s.mediaStatus)
}

func (s *apiServer) mediaDiveID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, ok := diveID(w, r)
	if !ok {
		return 0, false
	}
	exists, err := s.store.diveExists(r.Context(), id)
	if err != nil {
		s.internalError(w, r, err)
		return 0, false
	}
	if !exists {
		writeAPIError(w, http.StatusNotFound, "not_found", "dive not found")
		return 0, false
	}
	return id, true
}

func (s *apiServer) mediaError(w http.ResponseWriter, r *http.Request, err error) {
	var sizeErr *http.MaxBytesError
	switch {
	case errors.Is(err, errMediaNotFound), errors.Is(err, errDiveNotFound), errors.Is(err, fs.ErrNotExist):
		writeAPIError(w, http.StatusNotFound, "not_found", "dive or media not found")
	case errors.Is(err, errMediaConflict):
		writeAPIError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, errMediaTooLarge), errors.As(err, &sizeErr):
		writeAPIError(w, http.StatusRequestEntityTooLarge, "upload_too_large", "file exceeds the configured upload limit")
	case errors.Is(err, errInvalidMedia):
		writeAPIError(w, http.StatusUnsupportedMediaType, "unsupported_media", err.Error())
	case errors.Is(err, errMediaEmpty), errors.Is(err, io.ErrUnexpectedEOF):
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
	default:
		s.internalError(w, r, err)
	}
}

func (s *apiServer) listMedia(w http.ResponseWriter, r *http.Request) {
	id, ok := s.mediaDiveID(w, r)
	if !ok {
		return
	}
	limit, err := pageLimit(r, 50, 100)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var cursor mediaCursor
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		if err := decodeCursor(raw, &cursor); err != nil || !validMediaFilename(cursor.Filename) || cursor.ID == "" {
			writeAPIError(w, http.StatusBadRequest, "invalid_cursor", "cursor is invalid")
			return
		}
	}
	items, more, err := s.store.listMedia(r.Context(), id, limit, cursor.Filename, cursor.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	response := mediaListResponse{Items: items}
	if more {
		last := items[len(items)-1]
		response.NextCursor, err = encodeCursor(mediaCursor{Filename: last.Filename, ID: last.ID})
		if err != nil {
			s.internalError(w, r, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *apiServer) getMedia(w http.ResponseWriter, r *http.Request) {
	id, ok := diveID(w, r)
	if !ok {
		return
	}
	m, err := s.store.mediaByID(r.Context(), id, r.PathValue("mediaID"))
	if err != nil {
		s.mediaError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *apiServer) uploadMedia(w http.ResponseWriter, r *http.Request) {
	id, ok := s.mediaDiveID(w, r)
	if !ok {
		return
	}
	l := s.store.media
	r.Body = http.MaxBytesReader(w, r.Body, l.maxUpload+(1<<20))
	reader, err := r.MultipartReader()
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "use multipart/form-data with one file field named file")
		return
	}
	part, err := reader.NextPart()
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "missing file part")
		return
	}
	defer part.Close()
	// Part.FileName sanitizes path components. Inspect the original header so an
	// invalid path is rejected rather than silently changing the original name.
	_, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
	name := params["filename"]
	if err != nil || part.FormName() != "file" || !validMediaFilename(name) {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "expected one file field with an original filename and no path components")
		return
	}
	var endErr error
	m, err := l.Upload(r.Context(), id, name, part, func() error {
		_, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			return nil
		}
		endErr = errors.New("send exactly one file per request with a complete multipart body")
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return err
		}
		return endErr
	})
	if err != nil {
		if endErr != nil && errors.Is(err, endErr) {
			writeAPIError(w, http.StatusBadRequest, "invalid_request", endErr.Error())
		} else {
			s.mediaError(w, r, err)
		}
		return
	}
	w.Header().Set("Location", "/api/v1/dives/"+strconv.FormatInt(id, 10)+"/media/"+m.ID)
	writeJSON(w, http.StatusCreated, m)
}

func (s *apiServer) patchMedia(w http.ResponseWriter, r *http.Request) {
	id, ok := diveID(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var patch mediaPatch
	if err := decoder.Decode(&patch); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "invalid metadata JSON")
		return
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "request must contain one JSON object")
		return
	}
	if err := patch.validate(); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	m, err := s.store.media.Patch(r.Context(), id, r.PathValue("mediaID"), patch)
	if err != nil {
		s.mediaError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *apiServer) deleteMedia(w http.ResponseWriter, r *http.Request) {
	id, ok := diveID(w, r)
	if !ok {
		return
	}
	if err := s.store.media.Delete(r.Context(), id, r.PathValue("mediaID")); err != nil {
		s.mediaError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *apiServer) getAsset(w http.ResponseWriter, r *http.Request) {
	id, ok := diveID(w, r)
	if !ok {
		return
	}
	name := r.PathValue("filename")
	if !validMediaFilename(name) {
		writeAPIError(w, http.StatusNotFound, "not_found", "media not found")
		return
	}
	m, err := s.store.mediaByFilename(r.Context(), id, name)
	if err != nil {
		s.mediaError(w, r, err)
		return
	}
	s.serveMediaFile(w, r, m, false)
}

func (s *apiServer) getThumbnail(w http.ResponseWriter, r *http.Request) {
	id, ok := diveID(w, r)
	if !ok {
		return
	}
	m, err := s.store.mediaByID(r.Context(), id, r.PathValue("mediaID"))
	if err != nil {
		s.mediaError(w, r, err)
		return
	}
	if m.ThumbnailStatus != "ready" {
		writeAPIError(w, http.StatusNotFound, "thumbnail_unavailable", "thumbnail is unavailable; check media metadata and rescan to retry")
		return
	}
	s.serveMediaFile(w, r, m, true)
}

func (s *apiServer) serveMediaFile(w http.ResponseWriter, r *http.Request, m mediaItem, thumbnail bool) {
	path, contentType, etag := mediaPath(m.DiveID, m.Filename), m.ContentType, m.SHA256
	name := m.Filename
	if thumbnail {
		path, contentType, etag = ".thumbnails/"+m.ThumbnailKey+".jpg", "image/jpeg", m.ThumbnailKey
		name += ".thumbnail.jpg"
	}
	file, err := s.store.media.root.Open(path)
	if err != nil {
		s.mediaError(w, r, err)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !info.Mode().IsRegular() {
		writeAPIError(w, http.StatusNotFound, "not_found", "media not found")
		return
	}
	if !thumbnail && (info.Size() != m.SizeBytes || info.ModTime().UnixNano() != m.ModifiedNS) {
		writeAPIError(w, http.StatusConflict, "media_changed", "file changed on disk; rescan the assets directory")
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": name}))
	w.Header().Set("Cache-Control", "public, max-age=0, must-revalidate")
	w.Header().Set("ETag", `"`+etag+`"`)
	http.ServeContent(w, r, name, info.ModTime(), file)
}

func (s *apiServer) scanMedia(w http.ResponseWriter, r *http.Request) {
	force := false
	if raw := r.URL.Query().Get("force"); raw != "" {
		var err error
		force, err = strconv.ParseBool(raw)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid_request", "force must be true or false")
			return
		}
	}
	result, err := s.store.media.Scan(r.Context(), force)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *apiServer) mediaStatus(w http.ResponseWriter, r *http.Request) {
	var total, photos, videos, ready, failed int64
	err := s.store.db.QueryRowContext(r.Context(), `SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN kind='photo' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN kind='video' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN thumbnail_status='ready' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN thumbnail_status='error' THEN 1 ELSE 0 END),0) FROM dive_media`).Scan(&total, &photos, &videos, &ready, &failed)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "photos": photos, "videos": videos,
		"thumbnails_ready": ready, "thumbnails_failed": failed, "max_upload_bytes": s.store.media.maxUpload,
		"thumbnail_max_dimension": 640})
}

// Media bodies and full-file video responses need longer than the JSON API's
// short deadlines, especially for large 360-degree recordings.
func mediaDeadlines(w http.ResponseWriter) {
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(time.Hour))
	_ = controller.SetWriteDeadline(time.Time{})
}
