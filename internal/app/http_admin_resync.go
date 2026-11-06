package app

import (
	"errors"
	"math"
	"net/http"

	httpx "github.com/se0wtf/go-garmindives/internal/httpx"
	identity "github.com/se0wtf/go-garmindives/internal/identity"
)

type resyncStatusResponse struct {
	Available           bool       `json:"available"`
	UnavailableCode     string     `json:"unavailable_code,omitempty"`
	Busy                bool       `json:"busy"`
	ActiveJob           *resyncJob `json:"active_job"`
	LatestJob           *resyncJob `json:"latest_job"`
	PollIntervalSeconds int        `json:"poll_interval_seconds"`
}
type resyncJobList struct {
	Items      []resyncJob `json:"items"`
	NextCursor string      `json:"next_cursor,omitempty"`
}
type resyncItemList struct {
	Items      []resyncItem `json:"items"`
	NextCursor string       `json:"next_cursor,omitempty"`
}
type resyncJobCursor struct {
	CreatedMS int64  `json:"created_at_ms"`
	ID        string `json:"id"`
}
type resyncItemCursor struct {
	ActivityID int64 `json:"activity_id"`
}

func (s *apiServer) registerResyncRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/admin/dives/{id}/resync", s.enqueueActivityResync)
	mux.HandleFunc("POST /api/v1/admin/dives/resync", s.enqueueAllResync)
	mux.HandleFunc("GET /api/v1/admin/resync/status", s.resyncStatus)
	mux.HandleFunc("GET /api/v1/admin/resync/jobs", s.listResyncJobs)
	mux.HandleFunc("GET /api/v1/admin/resync/jobs/{jobID}", s.getResyncJob)
	mux.HandleFunc("GET /api/v1/admin/resync/jobs/{jobID}/items", s.listResyncItems)
	mux.HandleFunc("POST /api/v1/admin/resync/jobs/{jobID}/cancel", s.cancelResyncJob)
}
func (s *apiServer) resyncService(w http.ResponseWriter) *resyncService {
	if s.store.resync == nil {
		httpx.WriteAPIError(w, 503, "resync_unavailable", "Activity resync is unavailable")
		return nil
	}
	return s.store.resync
}
func (s *apiServer) enqueueActivityResync(w http.ResponseWriter, r *http.Request) {
	id, ok := diveID(w, r)
	if ok {
		s.enqueueResync(w, r, id)
	}
}
func (s *apiServer) enqueueAllResync(w http.ResponseWriter, r *http.Request) {
	s.enqueueResync(w, r, 0)
}
func (s *apiServer) enqueueResync(w http.ResponseWriter, r *http.Request, id int64) {
	service := s.resyncService(w)
	if service == nil {
		return
	}
	if service.factory == nil {
		code := service.unavailableCode
		if code == "" {
			code = "garmin_not_configured"
		}
		httpx.WriteAPIError(w, 503, code, "Configure Garmin credentials on the serving process to enable activity resync")
		return
	}
	job, err := service.Enqueue(r.Context(), id)
	if errors.Is(err, errResyncBusy) {
		if active, e := s.store.activeResyncJob(r.Context()); e == nil {
			w.Header().Set("Location", active.URL)
		}
		httpx.WriteAPIError(w, 409, "resync_in_progress", "Another activity resync is already queued or running")
		return
	}
	if errors.Is(err, errDiveNotFound) {
		httpx.WriteAPIError(w, 404, "not_found", "Stored dive not found")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	w.Header().Set("Location", job.URL)
	httpx.WriteJSON(w, 202, job)
}
func (s *apiServer) resyncStatus(w http.ResponseWriter, r *http.Request) {
	service := s.resyncService(w)
	if service == nil {
		return
	}
	response := resyncStatusResponse{Available: service.factory != nil, UnavailableCode: service.unavailableCode, PollIntervalSeconds: 2}
	active, err := s.store.activeResyncJob(r.Context())
	if err == nil {
		response.ActiveJob = &active
		response.Busy = true
	} else if !errors.Is(err, errResyncNotFound) {
		s.internalError(w, r, err)
		return
	}
	latest, err := scanResyncJob(s.store.db.QueryRowContext(r.Context(), `SELECT `+resyncJobColumns+` FROM resync_jobs j ORDER BY created_at_ms DESC,id DESC LIMIT 1`))
	if err == nil {
		response.LatestJob = &latest
	} else if !errors.Is(err, errResyncNotFound) {
		s.internalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, 200, response)
}
func (s *apiServer) resyncRequest(w http.ResponseWriter, r *http.Request) (resyncJob, bool) {
	if s.resyncService(w) == nil {
		return resyncJob{}, false
	}
	id := r.PathValue("jobID")
	if !identity.ValidID(id) {
		httpx.WriteAPIError(w, 400, "invalid_request", "Invalid resync job ID")
		return resyncJob{}, false
	}
	job, err := s.store.resyncJob(r.Context(), id)
	if errors.Is(err, errResyncNotFound) {
		httpx.WriteAPIError(w, 404, "not_found", "Resync job not found")
		return job, false
	}
	if err != nil {
		s.internalError(w, r, err)
		return job, false
	}
	return job, true
}
func (s *apiServer) getResyncJob(w http.ResponseWriter, r *http.Request) {
	if job, ok := s.resyncRequest(w, r); ok {
		httpx.WriteJSON(w, 200, job)
	}
}
func (s *apiServer) cancelResyncJob(w http.ResponseWriter, r *http.Request) {
	job, ok := s.resyncRequest(w, r)
	if !ok {
		return
	}
	wasActive := job.Status == "queued" || job.Status == "running"
	job, err := s.store.resync.Cancel(r.Context(), job.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	status := 202
	if !wasActive {
		status = 200
	}
	httpx.WriteJSON(w, status, job)
}
func (s *apiServer) listResyncJobs(w http.ResponseWriter, r *http.Request) {
	if s.resyncService(w) == nil {
		return
	}
	limit, err := pageLimit(r, 20, 100)
	if err != nil {
		httpx.WriteAPIError(w, 400, "invalid_request", err.Error())
		return
	}
	var cursor resyncJobCursor
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		if err := decodeCursor(raw, &cursor); err != nil || cursor.CreatedMS <= 0 || !identity.ValidID(cursor.ID) {
			httpx.WriteAPIError(w, 400, "invalid_cursor", "Invalid resync cursor")
			return
		}
	}
	cutoff := cursor.CreatedMS
	if cutoff == 0 {
		cutoff = math.MaxInt64
	}
	rows, err := s.store.db.QueryContext(r.Context(), s.store.bind(`SELECT `+resyncJobColumns+` FROM resync_jobs j WHERE (created_at_ms<? OR (created_at_ms=? AND id<?)) ORDER BY created_at_ms DESC,id DESC LIMIT ?`), cutoff, cutoff, cursor.ID, limit+1)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	response := resyncJobList{Items: []resyncJob{}}
	for rows.Next() {
		job, err := scanResyncJob(rows)
		if err != nil {
			rows.Close()
			s.internalError(w, r, err)
			return
		}
		response.Items = append(response.Items, job)
	}
	if err := closeRows(rows); err != nil {
		s.internalError(w, r, err)
		return
	}
	if len(response.Items) > limit {
		response.Items = response.Items[:limit]
		last := response.Items[limit-1]
		response.NextCursor, err = encodeCursor(resyncJobCursor{last.createdMS, last.ID})
		if err != nil {
			s.internalError(w, r, err)
			return
		}
	}
	httpx.WriteJSON(w, 200, response)
}
func (s *apiServer) listResyncItems(w http.ResponseWriter, r *http.Request) {
	job, ok := s.resyncRequest(w, r)
	if !ok {
		return
	}
	limit, err := pageLimit(r, 50, 100)
	if err != nil {
		httpx.WriteAPIError(w, 400, "invalid_request", err.Error())
		return
	}
	status := r.URL.Query().Get("status")
	switch status {
	case "", "queued", "running", "succeeded", "failed", "skipped":
	default:
		httpx.WriteAPIError(w, 400, "invalid_request", "Unknown activity result status")
		return
	}
	var cursor resyncItemCursor
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		if err := decodeCursor(raw, &cursor); err != nil || cursor.ActivityID <= 0 {
			httpx.WriteAPIError(w, 400, "invalid_cursor", "Invalid activity result cursor")
			return
		}
	}
	rows, err := s.store.db.QueryContext(r.Context(), s.store.bind(`SELECT activity_id,status,error_code,error_message,completed_at_ms FROM resync_items WHERE job_id=? AND (?='' OR status=?) AND activity_id>? ORDER BY activity_id LIMIT ?`), job.ID, status, status, cursor.ActivityID, limit+1)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	response := resyncItemList{Items: []resyncItem{}}
	for rows.Next() {
		var item resyncItem
		var completed int64
		if err := rows.Scan(&item.ActivityID, &item.Status, &item.ErrorCode, &item.Error, &completed); err != nil {
			rows.Close()
			s.internalError(w, r, err)
			return
		}
		item.CompletedAt = resyncTime(completed)
		response.Items = append(response.Items, item)
	}
	if err := closeRows(rows); err != nil {
		s.internalError(w, r, err)
		return
	}
	if len(response.Items) > limit {
		response.Items = response.Items[:limit]
		last := response.Items[limit-1]
		response.NextCursor, err = encodeCursor(resyncItemCursor{last.ActivityID})
		if err != nil {
			s.internalError(w, r, err)
			return
		}
	}
	httpx.WriteJSON(w, 200, response)
}
