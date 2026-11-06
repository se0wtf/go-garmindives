package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"

	mediaproc "github.com/se0wtf/go-garmindives/internal/media"
)

var errMediaNotFound = errors.New("media not found")

type mediaItem struct {
	ID                   string          `json:"id"`
	DiveID               int64           `json:"dive_id,omitempty"`
	TripID               string          `json:"trip_id,omitempty"`
	Favorite             bool            `json:"favorite"`
	Filename             string          `json:"filename"`
	Kind                 string          `json:"kind"`
	ContentType          string          `json:"content_type"`
	SizeBytes            int64           `json:"size_bytes"`
	ModifiedNS           int64           `json:"-"`
	SHA256               string          `json:"sha256"`
	Width                int             `json:"width"`
	Height               int             `json:"height"`
	DurationSeconds      float64         `json:"duration_seconds"`
	StereoLayout         string          `json:"stereo_layout"`
	DetectedStereoLayout string          `json:"detected_stereo_layout,omitempty"`
	Projection           string          `json:"projection"`
	EffectiveProjection  string          `json:"effective_projection"`
	ProjectionFormat     string          `json:"projection_format,omitempty"`
	Caption              string          `json:"caption"`
	Description          string          `json:"description"`
	ThumbnailStatus      string          `json:"thumbnail_status"`
	ThumbnailError       string          `json:"thumbnail_error,omitempty"`
	ThumbnailKey         string          `json:"-"`
	Probe                json.RawMessage `json:"probe"`
	CreatedAt            string          `json:"created_at"`
	UpdatedAt            string          `json:"updated_at"`
	URL                  string          `json:"url"`
	ThumbnailURL         string          `json:"thumbnail_url,omitempty"`
	Translations         translations    `json:"translations"`
	Capture              *photoCapture   `json:"capture,omitempty"`
}

const mediaColumns = `id, dive_id, filename, kind, content_type, size_bytes, modified_ns,
	sha256, width, height, duration_seconds, stereo_layout, detected_stereo_layout,
	projection, caption, description, thumbnail_status, thumbnail_error, thumbnail_key, probe_json, created_at, updated_at, favorite, capture_json`

func scanMedia(row rowScanner) (mediaItem, error) {
	return scanOwnedMedia(row, mediaOwner{})
}

func scanOwnedMedia(row rowScanner, owner mediaOwner) (mediaItem, error) {
	var m mediaItem
	var probe, capture string
	var ownerID any = &m.DiveID
	if owner.TripID != "" {
		ownerID = &m.TripID
	}
	err := row.Scan(&m.ID, ownerID, &m.Filename, &m.Kind, &m.ContentType, &m.SizeBytes,
		&m.ModifiedNS, &m.SHA256, &m.Width, &m.Height, &m.DurationSeconds, &m.StereoLayout,
		&m.DetectedStereoLayout, &m.Projection, &m.Caption, &m.Description, &m.ThumbnailStatus,
		&m.ThumbnailError, &m.ThumbnailKey, &probe, &m.CreatedAt, &m.UpdatedAt, &m.Favorite, &capture)
	if errors.Is(err, sql.ErrNoRows) {
		return m, errMediaNotFound
	}
	if err != nil {
		return m, err
	}
	m.Probe = json.RawMessage(probe)
	if capture != "{}" && capture != "null" {
		if err := json.Unmarshal([]byte(capture), &m.Capture); err != nil {
			return m, err
		}
	}
	m.setURLs()
	return m, nil
}

func (m *mediaItem) setURLs() {
	m.resolveProjection()
	owner := m.owner()
	m.URL = "/assets/" + owner.directory() + "/" + url.PathEscape(m.Filename)
	m.ThumbnailURL = ""
	if m.ThumbnailStatus == "ready" {
		m.ThumbnailURL = owner.apiPath() + "/media/" + m.ID + "/thumbnail?v=" + m.ThumbnailKey
	}
}

// Projection hints are derived from the retained probe JSON, so existing media
// can expose spherical metadata without another schema migration or download.
func (m *mediaItem) resolveProjection() {
	m.EffectiveProjection, m.ProjectionFormat = m.Projection, ""
	probe, err := mediaproc.DecodeProbe(m.Probe)
	if photo := m.photoState(); photo.Version == photoProfileVersion && photo.Projection != "" {
		probe.Projection, probe.ProjectionFormat = photo.Projection, photo.ProjectionFormat
	}
	if video := m.videoState(); video.Version == videoProfileVersion && video.Projection != "" {
		probe.Projection, probe.ProjectionFormat = video.Projection, video.ProjectionFormat
	}
	if err == nil {
		m.ProjectionFormat = probe.ProjectionFormat
	}
	if m.Projection == "auto" || m.Projection == "" {
		m.EffectiveProjection = "flat"
		if err == nil && probe.Projection != "" {
			m.EffectiveProjection = probe.Projection
		}
	}
	if m.EffectiveProjection == "360" && m.ProjectionFormat == "" {
		// An explicit 360 override denotes an untagged stitched export.
		m.ProjectionFormat = "equirectangular"
	}
}

func (s *sqlStore) mediaForDive(ctx context.Context, diveID int64) ([]mediaItem, error) {
	return s.mediaForOwner(ctx, mediaOwner{DiveID: diveID})
}
func (s *sqlStore) mediaForOwner(ctx context.Context, owner mediaOwner) ([]mediaItem, error) {
	rows, err := s.db.QueryContext(ctx, s.bind(`SELECT `+owner.columns()+` FROM `+owner.table()+` WHERE `+owner.column()+` = ? ORDER BY filename, id`), owner.value())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []mediaItem{}
	for rows.Next() {
		item, err := scanOwnedMedia(rows, owner)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *sqlStore) mediaByID(ctx context.Context, diveID int64, id string) (mediaItem, error) {
	return s.ownedMediaByID(ctx, mediaOwner{DiveID: diveID}, id)
}
func (s *sqlStore) ownedMediaByID(ctx context.Context, owner mediaOwner, id string) (mediaItem, error) {
	return scanOwnedMedia(s.db.QueryRowContext(ctx, s.bind(`SELECT `+owner.columns()+` FROM `+owner.table()+` WHERE `+owner.column()+` = ? AND id = ?`), owner.value(), id), owner)
}

func (s *sqlStore) listMedia(ctx context.Context, diveID int64, limit int, afterName, afterID string) ([]mediaItem, bool, error) {
	return s.listOwnedMedia(ctx, mediaOwner{DiveID: diveID}, limit, afterName, afterID)
}
func (s *sqlStore) listOwnedMedia(ctx context.Context, owner mediaOwner, limit int, afterName, afterID string) ([]mediaItem, bool, error) {
	rows, err := s.db.QueryContext(ctx, s.bind(`SELECT `+owner.columns()+` FROM `+owner.table()+`
		WHERE `+owner.column()+` = ? AND (filename > ? OR (filename = ? AND id > ?))
		ORDER BY filename, id LIMIT ?`), owner.value(), afterName, afterName, afterID, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	items := []mediaItem{}
	for rows.Next() {
		item, err := scanOwnedMedia(rows, owner)
		if err != nil {
			return nil, false, err
		}
		items = append(items, item)
	}
	more := len(items) > limit
	if more {
		items = items[:limit]
	}
	return items, more, rows.Err()
}

func (s *sqlStore) mediaByFilename(ctx context.Context, diveID int64, name string) (mediaItem, error) {
	return s.ownedMediaByFilename(ctx, mediaOwner{DiveID: diveID}, name)
}
func (s *sqlStore) ownedMediaByFilename(ctx context.Context, owner mediaOwner, name string) (mediaItem, error) {
	return scanOwnedMedia(s.db.QueryRowContext(ctx, s.bind(`SELECT `+owner.columns()+` FROM `+owner.table()+` WHERE `+owner.column()+` = ? AND filename = ?`), owner.value(), name), owner)
}

func (s *sqlStore) saveMedia(ctx context.Context, m mediaItem) error {
	owner := m.owner()
	_, err := s.db.ExecContext(ctx, s.bind(`INSERT INTO `+owner.table()+` (`+owner.columns()+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET filename=excluded.filename, kind=excluded.kind,
		content_type=excluded.content_type, size_bytes=excluded.size_bytes, modified_ns=excluded.modified_ns,
		sha256=excluded.sha256, width=excluded.width, height=excluded.height,
		duration_seconds=excluded.duration_seconds, stereo_layout=excluded.stereo_layout,
		detected_stereo_layout=excluded.detected_stereo_layout, projection=excluded.projection,
		caption=excluded.caption, description=excluded.description, thumbnail_status=excluded.thumbnail_status,
		thumbnail_error=excluded.thumbnail_error, thumbnail_key=excluded.thumbnail_key,
		probe_json=excluded.probe_json, updated_at=excluded.updated_at,favorite=excluded.favorite,capture_json=excluded.capture_json`),
		m.ID, owner.value(), m.Filename, m.Kind, m.ContentType, m.SizeBytes, m.ModifiedNS,
		m.SHA256, m.Width, m.Height, m.DurationSeconds, m.StereoLayout, m.DetectedStereoLayout,
		m.Projection, m.Caption, m.Description, m.ThumbnailStatus, m.ThumbnailError, m.ThumbnailKey,
		string(m.Probe), m.CreatedAt, m.UpdatedAt, boolNumber(m.Favorite), captureJSON(m.Capture))
	if err == nil {
		s.translationsChanged()
	}
	return err
}

func (s *sqlStore) removeMedia(ctx context.Context, m mediaItem) error {
	owner := m.owner()
	_, err := s.db.ExecContext(ctx, s.bind(`DELETE FROM `+owner.table()+` WHERE `+owner.column()+` = ? AND id = ?`), owner.value(), m.ID)
	if err == nil {
		s.translationsChanged()
	}
	return err
}
