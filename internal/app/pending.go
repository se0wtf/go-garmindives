package app

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	identity "github.com/se0wtf/go-garmindives/internal/identity"
	mediaproc "github.com/se0wtf/go-garmindives/internal/media"
)

type pendingScanResult struct {
	Scanned  int `json:"scanned"`
	Assigned int `json:"assigned"`
	Pending  int `json:"pending"`
	Removed  int `json:"removed"`
	Skipped  int `json:"skipped"`
	Failed   int `json:"failed"`
}
type pendingScanStatus struct {
	Running     bool              `json:"running"`
	StartedAt   string            `json:"started_at,omitempty"`
	CompletedAt string            `json:"completed_at,omitempty"`
	Result      pendingScanResult `json:"result"`
	Error       string            `json:"error,omitempty"`
}
type pendingService struct {
	store                *sqlStore
	media                *mediaLibrary
	files                *mediaLibrary
	reader               mediaproc.CaptureReader
	directory            string
	interval             time.Duration
	now                  func() time.Time
	gate, scanGate, wake chan struct{}
	mu                   sync.Mutex
	status               pendingScanStatus
	force                bool
	cancel               context.CancelFunc
	done                 chan struct{}
	closed               bool
}

func newPendingService(media *mediaLibrary, directory string, interval time.Duration) (*pendingService, error) {
	if interval < time.Second || interval > 24*time.Hour {
		return nil, errors.New("invalid pending scan interval")
	}
	rootPath, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	assets, err := filepath.EvalSymlinks(media.directory)
	if err != nil {
		return nil, err
	}
	// Reject an inbox that contains the assets root or aliases an owner/cache.
	if rel, err := filepath.Rel(rootPath, assets); err == nil && (rel == "." || !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..") {
		return nil, errors.New("pending directory must not contain the assets directory")
	}
	if err := os.MkdirAll(rootPath, 0o700); err != nil {
		return nil, err
	}
	rootPath, err = filepath.EvalSymlinks(rootPath)
	if err != nil {
		return nil, err
	}
	if rel, err := filepath.Rel(rootPath, assets); err == nil && (rel == "." || !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..") {
		return nil, errors.New("pending directory must not contain the assets directory")
	}
	if rel, err := filepath.Rel(assets, rootPath); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		first := strings.Split(rel, string(filepath.Separator))[0]
		_, numeric := strconv.ParseInt(first, 10, 64)
		if numeric == nil || first == "trips" || first == ".uploads" || first == ".thumbnails" || first == ".fallbacks" || first == ".photo-jobs" {
			return nil, errors.New("pending directory overlaps an activity or internal media directory")
		}
		media.pendingSubdir = first
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{".uploads", ".thumbnails", ".fallbacks"} {
		if err := root.MkdirAll(name, 0o700); err != nil {
			root.Close()
			return nil, err
		}
	}
	files := &mediaLibrary{root: root, directory: rootPath, processor: media.processor, logger: media.logger, fallbackGate: make(chan struct{}, 1), gate: make(chan struct{}, 1)}
	reader := mediaproc.NewCaptureReader()
	if reader.Path == "" && media.logger != nil {
		media.logger.Warn("pending medias need exiftool to read capture dates; manual assignment remains available")
	}
	return &pendingService{store: media.store, media: media, files: files, reader: reader, directory: rootPath, interval: interval, now: time.Now, gate: make(chan struct{}, 1), scanGate: make(chan struct{}, 1), wake: make(chan struct{}, 1), done: make(chan struct{})}, nil
}

func (p *pendingService) lock(ctx context.Context) error {
	select {
	case p.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (p *pendingService) unlock() { <-p.gate }
func (p *pendingService) Signal(force bool) {
	p.mu.Lock()
	p.force = p.force || force
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}
func (p *pendingService) Start(ctx context.Context) {
	p.mu.Lock()
	if p.cancel != nil || p.closed {
		p.mu.Unlock()
		return
	}
	ctx, p.cancel = context.WithCancel(ctx)
	p.mu.Unlock()
	go func() {
		defer close(p.done)
		for {
			p.mu.Lock()
			force := p.force
			p.force = false
			p.mu.Unlock()
			if _, err := p.Scan(ctx, force); err != nil && ctx.Err() == nil && p.media.logger != nil {
				p.media.logger.Warn("pending media scan failed", "err", err)
			}
			timer := time.NewTimer(p.interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-p.wake:
				timer.Stop()
			case <-timer.C:
			}
		}
	}()
}
func (p *pendingService) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	cancel := p.cancel
	p.mu.Unlock()
	if cancel != nil {
		cancel()
		<-p.done
	}
	p.files.Close()
}
func (p *pendingService) scanStatus() pendingScanStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}

func (p *pendingService) Scan(ctx context.Context, force bool) (result pendingScanResult, err error) {
	select {
	case p.scanGate <- struct{}{}:
	case <-ctx.Done():
		return result, ctx.Err()
	}
	defer func() { <-p.scanGate }()
	p.mu.Lock()
	p.status.Running = true
	p.status.StartedAt = nowString()
	p.status.Error = ""
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.status.Running = false
		p.status.CompletedAt = nowString()
		p.status.Result = result
		if err != nil {
			p.status.Error = err.Error()
		}
		p.mu.Unlock()
		if p.media.logger != nil && (result.Scanned > 0 || result.Assigned > 0 || result.Removed > 0 || result.Failed > 0 || err != nil) {
			p.media.logger.Info("pending media scan complete", "scanned", result.Scanned, "assigned", result.Assigned, "pending", result.Pending, "removed", result.Removed, "failed", result.Failed, "skipped", result.Skipped)
		}
	}()
	recovered, recoverErr := p.Recover(ctx)
	if recoverErr != nil {
		return result, recoverErr
	}
	result.Assigned += recovered
	entries, err := fs.ReadDir(p.files.root.FS(), ".")
	if err != nil {
		return result, err
	}
	old, err := p.store.pendingItems(ctx)
	if err != nil {
		return result, err
	}
	dives, err := p.store.pendingDives(ctx)
	if err != nil {
		return result, err
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		kind, _ := mediaType(name)
		if !validMediaFilename(name) || kind == "" || !entry.Type().IsRegular() {
			if !strings.HasPrefix(name, ".") {
				result.Skipped++
			}
			continue
		}
		seen[name] = true
		result.Scanned++
		if err := p.lock(ctx); err != nil {
			return result, err
		}
		assigned, reason, fileErr := p.scanFile(ctx, name, dives, force)
		p.unlock()
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if assigned {
			result.Assigned++
		} else {
			result.Pending++
		}
		if fileErr != nil || reason == "metadata_error" || reason == "processing_error" {
			result.Failed++
		}
		if fileErr != nil && p.media.logger != nil {
			p.media.logger.Warn("pending media could not be processed", "filename", name, "err", fileErr)
		}
	}
	for _, item := range old {
		if seen[item.Filename] || item.State == "assigning" {
			continue
		}
		if err := p.lock(ctx); err != nil {
			return result, err
		}
		_, deleteErr := p.store.db.ExecContext(ctx, p.store.bind(`DELETE FROM pending_media WHERE id=? AND state='pending'`), item.ID)
		p.unlock()
		if deleteErr != nil {
			return result, deleteErr
		}
		p.removePreviews(item)
		result.Removed++
	}
	return result, nil
}

func (p *pendingService) scanFile(ctx context.Context, name string, dives []pendingDive, force bool) (bool, string, error) {
	info, err := p.files.root.Lstat(name)
	if err != nil {
		return false, "", err
	}
	if !info.Mode().IsRegular() {
		return false, "", errPendingChanged
	}
	item, err := p.store.pendingByFilename(ctx, name)
	if errors.Is(err, errPendingNotFound) {
		item = pendingItem{ID: identity.NewID(), Filename: name, SizeBytes: info.Size(), ModifiedNS: info.ModTime().UnixNano(), Reason: "waiting_for_file", State: "pending", StableSince: p.now().UnixMilli(), CreatedAt: nowString(), Candidates: []pendingCandidate{}}
	} else if err != nil {
		return false, "", err
	}
	if item.State == "assigning" {
		return false, item.Reason, nil
	}
	if item.SizeBytes != info.Size() || item.ModifiedNS != info.ModTime().UnixNano() {
		p.removePreviews(item)
		item.SizeBytes, item.ModifiedNS = info.Size(), info.ModTime().UnixNano()
		item.StableSince = p.now().UnixMilli()
		item.MetadataReady = false
		item.Capture = nil
		item.SHA256 = ""
		item.Candidates = []pendingCandidate{}
	}
	if info.Size() == 0 || p.now().UnixMilli()-item.StableSince < pendingSettleTime.Milliseconds() {
		return false, "waiting_for_file", p.saveState(ctx, &item, "waiting_for_file", nil)
	}
	if force || !item.MetadataReady {
		input, openErr := p.openSource(ctx, item, false)
		if openErr != nil {
			return false, "source_changed", openErr
		}
		beforeSHA, hashErr := hashPhoto(ctx, input)
		if hashErr != nil {
			input.Close()
			return false, "", hashErr
		}
		capture, readErr := p.reader.ReadCapture(ctx, input)
		afterSHA, hashErr := hashPhoto(ctx, input)
		after, statErr := input.Stat()
		input.Close()
		if ctx.Err() != nil {
			return false, "", ctx.Err()
		}
		if hashErr != nil || statErr != nil || afterSHA != beforeSHA || after.Size() != item.SizeBytes || after.ModTime().UnixNano() != item.ModifiedNS {
			return false, "source_changed", errPendingChanged
		}
		if item.SHA256 != "" && item.SHA256 != afterSHA {
			p.removePreviews(item)
		}
		item.SHA256, item.Capture, item.MetadataReady = afterSHA, capture, readErr == nil
		if readErr != nil {
			return false, "metadata_error", p.saveState(ctx, &item, "metadata_error", readErr)
		}
	}
	candidates, reason := matchPendingCapture(item.Capture, dives)
	item.Candidates = candidates
	if err := p.saveState(ctx, &item, reason, nil); err != nil {
		return false, item.Reason, err
	}
	if item.Reason != "matched" {
		return false, item.Reason, nil
	}
	for _, dive := range dives {
		if dive.id == item.Candidates[0].DiveID {
			capture, _ := captureForDive(item.Capture, dive)
			_, err := p.assignLocked(ctx, &item, dive.id, capture)
			return err == nil, item.Reason, err
		}
	}
	return false, item.Reason, nil
}

func (p *pendingService) saveState(ctx context.Context, item *pendingItem, reason string, cause error) error {
	changed := item.Reason != reason || cause != nil && item.Error != cause.Error()
	item.Reason, item.Error, item.UpdatedAt = reason, "", nowString()
	if cause != nil {
		item.Error = cause.Error()
	}
	if err := p.store.savePending(ctx, *item); err != nil {
		return err
	}
	if changed && p.media.logger != nil {
		p.media.logger.Info("pending media status", "filename", item.Filename, "reason", reason, "candidates", len(item.Candidates))
	}
	return nil
}

func (p *pendingService) openSource(ctx context.Context, item pendingItem, verify bool) (*os.File, error) {
	info, err := p.files.root.Lstat(item.Filename)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() != item.SizeBytes || info.ModTime().UnixNano() != item.ModifiedNS {
		return nil, errPendingChanged
	}
	input, err := p.files.root.Open(item.Filename)
	if err != nil {
		return nil, err
	}
	after, err := input.Stat()
	if err != nil || !os.SameFile(info, after) {
		input.Close()
		return nil, errPendingChanged
	}
	if verify {
		actual, hashErr := hashPhoto(ctx, input)
		if hashErr != nil || item.SHA256 == "" || actual != item.SHA256 {
			input.Close()
			return nil, errPendingChanged
		}
	}
	return input, nil
}

func (p *pendingService) assignLocked(ctx context.Context, item *pendingItem, diveID int64, capture *photoCapture) (mediaItem, error) {
	if item.State != "assigning" {
		if err := p.media.reservePending(ctx, item, diveID, capture); err != nil {
			latest, readErr := p.store.pendingByID(ctx, item.ID)
			if readErr == nil {
				*item = latest
				reason := "processing_error"
				if errors.Is(err, errMediaConflict) {
					reason = "filename_conflict"
				}
				_ = p.saveState(ctx, item, reason, err)
			}
			return mediaItem{}, err
		}
	}
	m, err := p.transfer(ctx, *item)
	if err != nil {
		reason := "processing_error"
		if errors.Is(err, errMediaConflict) {
			reason = "filename_conflict"
		}
		if errors.Is(err, errPendingChanged) {
			reason = "source_changed"
		}
		_ = p.saveState(ctx, item, reason, err)
	}
	return m, err
}

func (p *pendingService) transfer(ctx context.Context, item pendingItem) (mediaItem, error) {
	input, openErr := p.openSource(ctx, item, true)
	if input != nil {
		defer input.Close()
	}
	m, err := p.media.importPending(ctx, item, input)
	if err != nil {
		if openErr != nil && errors.Is(err, fs.ErrNotExist) {
			return m, openErr
		}
		return m, err
	}
	if err := p.files.retirePhoto(ctx, item.Filename, item.TransferSHA); err != nil && !errors.Is(err, errPhotoSourceChanged) {
		return m, err
	}
	if err := p.files.syncPhotoDirectory("."); err != nil {
		return m, err
	}
	if _, err := p.store.db.ExecContext(ctx, p.store.bind(`DELETE FROM pending_media WHERE id=? AND media_id=?`), item.ID, item.MediaID); err != nil {
		return m, err
	}
	p.removePreviews(item)
	if p.media.logger != nil {
		p.media.logger.Info("pending media assigned", "filename", item.Filename, "activity_id", m.DiveID, "media_id", m.ID)
	}
	return m, nil
}

func (p *pendingService) Recover(ctx context.Context) (int, error) {
	items, err := p.store.pendingItems(ctx)
	if err != nil {
		return 0, err
	}
	completed := 0
	for _, item := range items {
		if item.State != "assigning" {
			continue
		}
		if err := p.lock(ctx); err != nil {
			return completed, err
		}
		_, err := p.transfer(ctx, item)
		if err == nil {
			completed++
		} else if errors.Is(err, errPendingChanged) || errors.Is(err, fs.ErrNotExist) {
			if abortErr := p.media.abortPending(ctx, item); abortErr == nil {
				if errors.Is(err, fs.ErrNotExist) {
					_, err = p.store.db.ExecContext(ctx, p.store.bind(`DELETE FROM pending_media WHERE id=?`), item.ID)
				} else {
					item.State, item.MediaID, item.TargetDiveID, item.TransferSHA, item.TransferCapture = "pending", "", 0, "", nil
					item.MetadataReady = false
					item.SHA256 = ""
					item.StableSince = p.now().UnixMilli()
					err = p.saveState(ctx, &item, "waiting_for_file", nil)
				}
			} else {
				_ = p.saveState(ctx, &item, "processing_error", err)
			}
		} else {
			_ = p.saveState(ctx, &item, "processing_error", err)
		}
		p.unlock()
		if ctx.Err() != nil {
			return completed, ctx.Err()
		}
	}
	return completed, nil
}

func (p *pendingService) Assign(ctx context.Context, id string, diveID int64) (mediaItem, error) {
	if err := p.lock(ctx); err != nil {
		return mediaItem{}, err
	}
	defer p.unlock()
	item, err := p.store.pendingByID(ctx, id)
	if err != nil {
		return mediaItem{}, err
	}
	if item.State == "assigning" && item.TargetDiveID != diveID {
		if err := p.media.abortPending(ctx, item); err != nil {
			return mediaItem{}, err
		}
		item.State, item.TargetDiveID, item.MediaID, item.TransferSHA, item.TransferCapture = "pending", 0, "", "", nil
		if err := p.store.savePending(ctx, item); err != nil {
			return mediaItem{}, err
		}
	}
	if item.State == "assigning" {
		return p.assignLocked(ctx, &item, diveID, item.TransferCapture)
	}
	if p.now().UnixMilli()-item.StableSince < pendingSettleTime.Milliseconds() {
		return mediaItem{}, errPendingChanged
	}
	input, err := p.openSource(ctx, item, false)
	if err != nil {
		return mediaItem{}, err
	}
	actual, err := hashPhoto(ctx, input)
	if err != nil {
		input.Close()
		return mediaItem{}, err
	}
	if item.SHA256 != "" && item.SHA256 != actual {
		input.Close()
		return mediaItem{}, errPendingChanged
	}
	item.SHA256 = actual
	if !item.MetadataReady {
		item.Capture, _ = p.reader.ReadCapture(ctx, input)
	}
	input.Close()
	var name, start, zone string
	if err := p.store.db.QueryRowContext(ctx, p.store.bind(`SELECT name,start_time,timezone FROM dives WHERE id=?`), diveID).Scan(&name, &start, &zone); err != nil {
		exists, checkErr := p.store.diveExists(ctx, diveID)
		if checkErr != nil {
			return mediaItem{}, checkErr
		}
		if !exists {
			return mediaItem{}, errDiveNotFound
		}
		return mediaItem{}, err
	}
	_, location, timezone, _ := pendingDiveStart(start, zone)
	if location == nil {
		location, _ = time.LoadLocation(zone)
		timezone = zone
	}
	capture, _ := captureForDive(item.Capture, pendingDive{location: location, timezone: timezone})
	if err := p.store.savePending(ctx, item); err != nil {
		return mediaItem{}, err
	}
	return p.assignLocked(ctx, &item, diveID, capture)
}

func (p *pendingService) Delete(ctx context.Context, id string) error {
	if err := p.lock(ctx); err != nil {
		return err
	}
	defer p.unlock()
	item, err := p.store.pendingByID(ctx, id)
	if err != nil {
		return err
	}
	if err := p.media.abortPending(ctx, item); err != nil {
		return err
	}
	if item.SHA256 == "" {
		input, err := p.openSource(ctx, item, false)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if input != nil {
			item.SHA256, err = hashPhoto(ctx, input)
			input.Close()
			if err != nil {
				return err
			}
		}
	}
	if err := p.files.retirePhoto(ctx, item.Filename, item.SHA256); err != nil {
		if errors.Is(err, errPhotoSourceChanged) {
			return errPendingChanged
		}
		return err
	}
	if _, err := p.store.db.ExecContext(ctx, p.store.bind(`DELETE FROM pending_media WHERE id=?`), item.ID); err != nil {
		return err
	}
	p.removePreviews(item)
	return nil
}

func (p *pendingService) previewMedia(item pendingItem) mediaItem {
	m := mediaItem{ID: item.ID, Filename: item.Filename, SHA256: item.SHA256, StereoLayout: "mono", Projection: "flat"}
	m.Kind, m.ContentType = mediaType(item.Filename)
	state := photoState{Version: photoProfileVersion}
	if m.ContentType == "image/heic" {
		state.HEIFDecoderVersion = 1
	}
	if m.Kind == "photo" {
		m.setPhotoState(state)
	}
	m.ThumbnailKey = m.thumbnailKey()
	return m
}
func (p *pendingService) removePreviews(item pendingItem) {
	m := p.previewMedia(item)
	p.files.removeThumbnail(m)
	key := photoFallbackKey(photoFallbackSource(m.ContentType, "pending-original-v1:"+item.SHA256))
	_ = p.files.root.Remove(".fallbacks/" + key + ".jpg")
}
