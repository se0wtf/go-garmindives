package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	charmLog "github.com/charmbracelet/log"
)

const defaultMediaMaxUpload int64 = 16 << 30

var (
	errMediaConflict = errors.New("a file with that name already exists")
	errMediaTooLarge = errors.New("file exceeds the upload size limit")
	errInvalidMedia  = errors.New("unsupported media filename or extension")
	errMediaEmpty    = errors.New("uploaded file is empty")
)

type mediaLibrary struct {
	store     *sqlStore
	root      *os.Root
	directory string
	maxUpload int64
	processor mediaProcessor
	logger    *charmLog.Logger
	gate      chan struct{}
}

type mediaScanResult struct {
	Scanned   int `json:"scanned"`
	Added     int `json:"added"`
	Updated   int `json:"updated"`
	Unchanged int `json:"unchanged"`
	Removed   int `json:"removed"`
	Skipped   int `json:"skipped"`
	Failed    int `json:"failed"`
}

func openMediaLibrary(store *sqlStore, directory string, maxUpload int64, logger *charmLog.Logger) (*mediaLibrary, error) {
	if strings.TrimSpace(directory) == "" || maxUpload <= 0 || maxUpload > 1<<60 {
		return nil, errors.New("assets directory must not be empty and media upload limit must be positive")
	}
	directory, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, fmt.Errorf("create assets directory: %w", err)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	for _, path := range []string{".thumbnails", ".uploads"} {
		if err := root.MkdirAll(path, 0o700); err != nil {
			root.Close()
			return nil, err
		}
	}
	processor := newMediaProcessor()
	if logger != nil && (processor.ffmpeg == "" || processor.ffprobe == "") {
		logger.Warn("media thumbnails need ffmpeg and ffprobe; install FFmpeg and rescan", "assets_dir", directory)
	}
	return &mediaLibrary{store: store, root: root, directory: directory, maxUpload: maxUpload,
		processor: processor, logger: logger, gate: make(chan struct{}, 1)}, nil
}

func (l *mediaLibrary) Close() error { return l.root.Close() }

func (l *mediaLibrary) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case l.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *mediaLibrary) unlock() { <-l.gate }

func validMediaFilename(name string) bool {
	if name == "" || strings.HasPrefix(name, ".") || len(name) > 255 || strings.ContainsAny(name, "/\\") {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func mediaType(name string) (kind, contentType string) {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg", ".jps":
		return "photo", "image/jpeg"
	case ".png":
		return "photo", "image/png"
	case ".gif":
		return "photo", "image/gif"
	case ".webp":
		return "photo", "image/webp"
	case ".heic", ".heif":
		return "photo", "image/heic"
	case ".avif":
		return "photo", "image/avif"
	case ".tif", ".tiff":
		return "photo", "image/tiff"
	case ".bmp":
		return "photo", "image/bmp"
	case ".mp4", ".m4v":
		return "video", "video/mp4"
	case ".mov":
		return "video", "video/quicktime"
	case ".mkv":
		return "video", "video/x-matroska"
	case ".webm":
		return "video", "video/webm"
	case ".avi":
		return "video", "video/x-msvideo"
	case ".mts", ".m2ts":
		return "video", "video/mp2t"
	case ".3gp":
		return "video", "video/3gpp"
	case ".insv", ".360":
		return "video", "application/octet-stream"
	}
	return "", ""
}

func mediaPath(diveID int64, name string) string {
	return strconv.FormatInt(diveID, 10) + "/" + name
}

func newMediaID() string {
	var id [16]byte
	_, _ = rand.Read(id[:]) // crypto/rand.Read cannot fail on supported Go versions.
	return hex.EncodeToString(id[:])
}

func (l *mediaLibrary) logFile(message string, m mediaItem, err error) {
	if l.logger == nil {
		return
	}
	args := []any{"activity_id", m.DiveID, "filename", m.Filename}
	if err != nil {
		l.logger.Warn(message, append(args, "err", err)...)
	} else {
		l.logger.Info(message, args...)
	}
}

// Scan skips unchanged files using size/mtime. A forced scan hashes everything,
// allowing reconciliation after an external tool preserves both size and mtime.
func (l *mediaLibrary) Scan(ctx context.Context, force bool) (result mediaScanResult, err error) {
	if err := l.lock(ctx); err != nil {
		return result, err
	}
	defer l.unlock()
	started := time.Now()
	if l.logger != nil {
		l.logger.Info("scanning media assets", "assets_dir", l.directory, "force", force)
		defer func() {
			l.logger.Info("media scan complete", "scanned", result.Scanned, "added", result.Added,
				"updated", result.Updated, "unchanged", result.Unchanged, "removed", result.Removed,
				"skipped", result.Skipped, "failed", result.Failed, "duration", time.Since(started).Round(time.Millisecond))
		}()
	}
	ids, err := l.store.DiveIDs(ctx)
	if err != nil {
		return result, err
	}
	known := make(map[int64]bool, len(ids))
	for _, id := range ids {
		known[id] = true
	}
	entries, err := fs.ReadDir(l.root.FS(), ".")
	if err != nil {
		return result, err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		id, parseErr := strconv.ParseInt(entry.Name(), 10, 64)
		if !entry.IsDir() || parseErr != nil || id <= 0 || strconv.FormatInt(id, 10) != entry.Name() || !known[id] {
			result.Skipped++
			if l.logger != nil {
				l.logger.Warn("skipping assets entry; expected a directory named with a stored activity ID", "entry", entry.Name())
			}
		}
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		old, err := l.store.mediaForDive(ctx, id)
		if err != nil {
			return result, err
		}
		dir := strconv.FormatInt(id, 10)
		info, err := l.root.Lstat(dir)
		if err == nil && !info.IsDir() {
			// In particular, never scan symlinked activity directories.
			continue
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return result, err
		}
		var files []fs.DirEntry
		if err == nil {
			files, err = fs.ReadDir(l.root.FS(), dir)
			if err != nil {
				return result, err // A failed listing must never prune the index.
			}
		}
		seen := make(map[string]bool, len(files))
		for _, entry := range files {
			name := entry.Name()
			kind, _ := mediaType(name)
			if !validMediaFilename(name) || kind == "" || !entry.Type().IsRegular() {
				result.Skipped++
				continue
			}
			seen[name] = true
			result.Scanned++
			m, action, err := l.indexFile(ctx, id, name, force)
			if err != nil {
				if ctx.Err() != nil {
					return result, ctx.Err()
				}
				result.Failed++
				l.logFile("could not index media", mediaItem{DiveID: id, Filename: name}, err)
				continue
			}
			switch action {
			case "added":
				result.Added++
			case "updated":
				result.Updated++
			default:
				result.Unchanged++
			}
			if m.ThumbnailStatus == "error" {
				result.Failed++
			}
		}
		for _, item := range old {
			if !seen[item.Filename] {
				if err := l.store.removeMedia(ctx, item); err != nil {
					return result, err
				}
				l.removeThumbnail(item)
				result.Removed++
				l.logFile("removed missing media from index", item, nil)
			}
		}
	}
	return result, nil
}

type mediaContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r mediaContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func (l *mediaLibrary) indexFile(ctx context.Context, id int64, name string, force bool) (mediaItem, string, error) {
	m, err := l.store.mediaByFilename(ctx, id, name)
	fresh := errors.Is(err, errMediaNotFound)
	if err != nil && !fresh {
		return m, "", err
	}
	old := m
	input, err := l.root.Open(mediaPath(id, name))
	if err != nil {
		return m, "", err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return m, "", err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return m, "", errors.New("media must be a nonempty regular file")
	}
	_, thumbErr := l.root.Stat(".thumbnails/" + m.ThumbnailKey + ".jpg")
	if !fresh && !force && m.SizeBytes == info.Size() && m.ModifiedNS == info.ModTime().UnixNano() && m.ThumbnailStatus == "ready" && m.ThumbnailKey == m.thumbnailKey() && thumbErr == nil {
		return m, "unchanged", nil
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, mediaContextReader{ctx: ctx, r: input}); err != nil {
		return m, "", err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if fresh {
		m = mediaItem{ID: newMediaID(), DiveID: id, Filename: name, StereoLayout: "auto", Projection: "auto", CreatedAt: now}
	}
	m.Kind, m.ContentType = mediaType(name)
	m.SizeBytes, m.ModifiedNS = info.Size(), info.ModTime().UnixNano()
	m.SHA256, m.UpdatedAt = hex.EncodeToString(hash.Sum(nil)), now
	m.Width, m.Height, m.DurationSeconds, m.DetectedStereoLayout = 0, 0, 0, ""
	m.Probe, m.ThumbnailKey, m.ThumbnailStatus, m.ThumbnailError = []byte("{}"), "", "error", ""
	probe, processErr := l.processor.Inspect(ctx, input)
	if processErr == nil {
		m.Width, m.Height, m.DurationSeconds = probe.Width, probe.Height, probe.Duration
		m.DetectedStereoLayout, m.Probe = probe.StereoLayout, probe.JSON
		processErr = l.makeThumbnail(ctx, input, &m)
	}
	if ctx.Err() != nil {
		if m.ThumbnailKey != old.ThumbnailKey {
			l.removeThumbnail(m)
		}
		return m, "", ctx.Err()
	}
	if processErr != nil {
		m.ThumbnailError = processErr.Error()
	}
	// Files copied in place may still be changing: keep their old index until a
	// later scan sees a stable file. API uploads are published only when complete.
	after, statErr := l.root.Stat(mediaPath(id, name))
	if statErr != nil || !os.SameFile(info, after) || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		if m.ThumbnailKey != old.ThumbnailKey {
			l.removeThumbnail(m)
		}
		return m, "", errors.New("media changed during processing; retry after the copy finishes")
	}
	if err := l.store.saveMedia(ctx, m); err != nil {
		if m.ThumbnailKey != old.ThumbnailKey {
			l.removeThumbnail(m)
		}
		return m, "", err
	}
	if old.ThumbnailKey != m.ThumbnailKey {
		l.removeThumbnail(old)
	}
	m.setURLs()
	action := "updated"
	if fresh {
		action = "added"
	}
	l.logFile("media "+action, m, processErr)
	return m, action, nil
}

func (l *mediaLibrary) makeThumbnail(ctx context.Context, input *os.File, m *mediaItem) error {
	m.resolveProjection()
	layout := m.StereoLayout
	if layout == "auto" {
		layout = m.DetectedStereoLayout
	}
	name := m.thumbnailKey()
	tmp := ".thumbnails/.tmp-" + newMediaID()
	output, err := l.root.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer l.root.Remove(tmp)
	err = l.processor.Thumbnail(ctx, input, output, mediaThumbnailOptions{
		StereoLayout: layout, Projection: m.EffectiveProjection, ProjectionFormat: m.ProjectionFormat,
	})
	closeErr := output.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := l.root.Rename(tmp, ".thumbnails/"+name+".jpg"); err != nil {
		return err
	}
	m.ThumbnailKey, m.ThumbnailStatus = name, "ready"
	return nil
}

func (m mediaItem) thumbnailKey() string {
	m.resolveProjection()
	layout := m.StereoLayout
	if layout == "auto" {
		layout = m.DetectedStereoLayout
	}
	key := sha256.Sum256([]byte("v2:" + m.ID + ":" + m.SHA256 + ":" + layout + ":" + m.EffectiveProjection + ":" + m.ProjectionFormat))
	return hex.EncodeToString(key[:])
}

func (l *mediaLibrary) removeThumbnail(m mediaItem) {
	if m.ThumbnailKey != "" {
		_ = l.root.Remove(".thumbnails/" + m.ThumbnailKey + ".jpg")
	}
}

// validateEnd verifies the multipart trailer before publishing a staged file.
func (l *mediaLibrary) Upload(ctx context.Context, id int64, name string, source io.Reader, validateEnd func() error) (mediaItem, error) {
	if !validMediaFilename(name) {
		return mediaItem{}, errInvalidMedia
	}
	if kind, _ := mediaType(name); kind == "" {
		return mediaItem{}, errInvalidMedia
	}
	stage := ".uploads/" + newMediaID()
	file, err := l.root.OpenFile(stage, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return mediaItem{}, err
	}
	defer l.root.Remove(stage)
	n, copyErr := io.Copy(file, io.LimitReader(mediaContextReader{ctx: ctx, r: source}, l.maxUpload+1))
	closeErr := file.Close()
	if copyErr != nil {
		return mediaItem{}, copyErr
	}
	if n > l.maxUpload {
		return mediaItem{}, errMediaTooLarge
	}
	if closeErr != nil {
		return mediaItem{}, closeErr
	}
	if n == 0 {
		return mediaItem{}, errMediaEmpty
	}
	if validateEnd != nil {
		if err := validateEnd(); err != nil {
			return mediaItem{}, err
		}
	}
	if err := l.lock(ctx); err != nil {
		return mediaItem{}, err
	}
	defer l.unlock()
	exists, err := l.store.diveExists(ctx, id)
	if err != nil {
		return mediaItem{}, err
	}
	if !exists {
		return mediaItem{}, errDiveNotFound
	}
	if err := l.root.MkdirAll(strconv.FormatInt(id, 10), 0o755); err != nil {
		return mediaItem{}, err
	}
	target := mediaPath(id, name)
	// A hard link publishes complete bytes atomically and never overwrites an
	// existing name, including a file not yet picked up by a scan.
	if err := l.root.Link(stage, target); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return mediaItem{}, errMediaConflict
		}
		return mediaItem{}, err
	}
	m, _, err := l.indexFile(ctx, id, name, true)
	if err != nil {
		_ = l.root.Remove(target)
		return mediaItem{}, err
	}
	return m, nil
}

func (l *mediaLibrary) Delete(ctx context.Context, id int64, mediaID string) error {
	if err := l.lock(ctx); err != nil {
		return err
	}
	defer l.unlock()
	m, err := l.store.mediaByID(ctx, id, mediaID)
	if err != nil {
		return err
	}
	path := mediaPath(id, m.Filename)
	trash := ".uploads/deleted-" + newMediaID()
	renameErr := l.root.Rename(path, trash)
	if renameErr != nil && !errors.Is(renameErr, fs.ErrNotExist) {
		return renameErr
	}
	if err := l.store.removeMedia(ctx, m); err != nil {
		if renameErr == nil {
			_ = l.root.Rename(trash, path)
		}
		return err
	}
	_ = l.root.Remove(trash)
	l.removeThumbnail(m)
	l.logFile("media deleted", m, nil)
	return nil
}

type mediaPatch struct {
	Caption      *string `json:"caption"`
	StereoLayout *string `json:"stereo_layout"`
	Projection   *string `json:"projection"`
}

func (p mediaPatch) validate() error {
	if p.Caption == nil && p.StereoLayout == nil && p.Projection == nil {
		return errors.New("provide caption, stereo_layout, or projection")
	}
	if p.Caption != nil && len(*p.Caption) > 4096 {
		return errors.New("caption exceeds 4096 bytes")
	}
	if p.StereoLayout != nil {
		switch *p.StereoLayout {
		case "auto", "mono", "side_by_side", "top_bottom", "multiview":
		default:
			return errors.New("stereo_layout must be auto, mono, side_by_side, top_bottom, or multiview")
		}
	}
	if p.Projection != nil && *p.Projection != "auto" && *p.Projection != "flat" && *p.Projection != "180" && *p.Projection != "360" {
		return errors.New("projection must be auto, flat, 180, or 360")
	}
	return nil
}

func (l *mediaLibrary) Patch(ctx context.Context, id int64, mediaID string, patch mediaPatch) (mediaItem, error) {
	if err := patch.validate(); err != nil {
		return mediaItem{}, err
	}
	if err := l.lock(ctx); err != nil {
		return mediaItem{}, err
	}
	defer l.unlock()
	m, err := l.store.mediaByID(ctx, id, mediaID)
	if err != nil {
		return m, err
	}
	old := m
	if patch.Caption != nil {
		m.Caption = *patch.Caption
	}
	if patch.Projection != nil {
		m.Projection = *patch.Projection
	}
	if patch.StereoLayout != nil {
		m.StereoLayout = *patch.StereoLayout
	}
	if m.Projection != old.Projection || m.StereoLayout != old.StereoLayout {
		file, err := l.root.Open(mediaPath(id, m.Filename))
		if err != nil {
			return m, err
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return m, err
		}
		if info.Size() != m.SizeBytes || info.ModTime().UnixNano() != m.ModifiedNS {
			return m, errMediaConflict
		}
		m.ThumbnailKey, m.ThumbnailStatus, m.ThumbnailError = "", "error", ""
		if err := l.makeThumbnail(ctx, file, &m); err != nil {
			m.ThumbnailError = err.Error()
		}
		if ctx.Err() != nil {
			l.removeThumbnail(m)
			return m, ctx.Err()
		}
	}
	m.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := l.store.saveMedia(ctx, m); err != nil {
		if m.ThumbnailKey != old.ThumbnailKey {
			l.removeThumbnail(m)
		}
		return m, err
	}
	if m.ThumbnailKey != old.ThumbnailKey {
		l.removeThumbnail(old)
	}
	m.setURLs()
	l.logFile("media metadata updated", m, nil)
	return m, nil
}
