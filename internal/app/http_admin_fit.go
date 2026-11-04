package app

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"
	"unicode"

	httpx "github.com/se0wtf/go-garmindives/internal/httpx"
)

func (s *apiServer) registerFITAdminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/admin/dives/{id}/fit", s.downloadFIT)
}

func (s *apiServer) downloadFIT(w http.ResponseWriter, r *http.Request) {
	id, ok := diveID(w, r)
	if !ok {
		return
	}
	files, err := s.store.FITFiles(r.Context(), id)
	if errors.Is(err, errDiveNotFound) {
		httpx.WriteAPIError(w, http.StatusNotFound, "not_found", "dive not found")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if len(files) == 0 {
		httpx.WriteAPIError(w, http.StatusNotFound, "fit_unavailable", "no original FIT file is stored for this dive")
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	if len(files) == 1 {
		file := files[0]
		name := fitDownloadFilename(file.Name, fmt.Sprintf("%d.fit", id))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(file.Data))
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": fmt.Sprintf("%d-fit.zip", id)}))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	if err := writeFITArchive(w, files); err != nil && s.logger != nil && r.Context().Err() == nil {
		// Headers/body may already be sent. Never append JSON to a binary download.
		s.logger.Error("FIT archive download failed", "activity_id", id, "err", err)
	}
}

func fitDownloadFilename(name, fallback string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '_'
		}
		return r
	}, name)
	if !strings.EqualFold(path.Ext(name), ".fit") || strings.HasPrefix(name, ".") {
		return fallback
	}
	return name
}

func writeFITArchive(output io.Writer, files []storedFITFile) error {
	archive := zip.NewWriter(output)
	for _, file := range files {
		name := fmt.Sprintf("%02d-%s", file.Index, fitDownloadFilename(file.Name, "activity.fit"))
		entry, err := archive.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if err == nil {
			_, err = entry.Write(file.Data)
		}
		if err != nil {
			_ = archive.Close()
			return err
		}
	}
	return archive.Close()
}
