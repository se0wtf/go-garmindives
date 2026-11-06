package app

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	identity "github.com/se0wtf/go-garmindives/internal/identity"
)

// A durable intent bridges filesystem publication and the SQL metadata commit.
// Recovery either finishes source cleanup or restores the previous retained file.
type photoJob struct {
	Kind         string     `json:"kind,omitempty"`
	ID           string     `json:"id"`
	Owner        mediaOwner `json:"owner"`
	Source       string     `json:"source"`
	SourceSHA    string     `json:"source_sha"`
	Target       string     `json:"target"`
	TargetSHA    string     `json:"target_sha"`
	Backup       string     `json:"backup,omitempty"`
	BackupSHA    string     `json:"backup_sha,omitempty"`
	Thumbnail    string     `json:"thumbnail"`
	OldThumbnail string     `json:"old_thumbnail,omitempty"`
	Obsolete     string     `json:"obsolete,omitempty"`
	ObsoleteSHA  string     `json:"obsolete_sha,omitempty"`
}

var errPhotoSourceChanged = errors.New("photo source was replaced after conversion")

func (j photoJob) path() string { return ".photo-jobs/" + j.ID + ".json" }

func validPhotoHash(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32
}

func (j photoJob) valid() bool {
	kind := j.Kind
	if kind == "" {
		kind = "photo"
	} // Pre-video journals remain recoverable.
	if kind != "photo" && kind != "video" {
		return false
	}
	ownerValid := j.Owner.DiveID > 0 && j.Owner.TripID == "" || j.Owner.DiveID == 0 && identity.ValidID(j.Owner.TripID)
	ownedPhoto := func(path string) bool {
		name := strings.TrimPrefix(path, j.Owner.directory()+"/")
		fileKind, _ := mediaType(name)
		return path == j.Owner.path(name) && validMediaFilename(name) && fileKind == kind
	}
	backupValid := j.Backup == "" && j.BackupSHA == "" || strings.HasPrefix(j.Backup, ".uploads/photo-backup-") && identity.ValidID(strings.TrimPrefix(j.Backup, ".uploads/photo-backup-")) && validPhotoHash(j.BackupSHA)
	obsoleteValid := j.Obsolete == "" && j.ObsoleteSHA == "" || ownedPhoto(j.Obsolete) && j.Obsolete != j.Target && validPhotoHash(j.ObsoleteSHA)
	return ownerValid && identity.ValidID(j.ID) && ownedPhoto(j.Source) && ownedPhoto(j.Target) && validPhotoHash(j.SourceSHA) && validPhotoHash(j.TargetSHA) && validPhotoHash(j.Thumbnail) && (j.OldThumbnail == "" || validPhotoHash(j.OldThumbnail)) && backupValid && obsoleteValid
}

func (l *mediaLibrary) syncPhotoDirectory(path string) error {
	directory, err := l.root.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (l *mediaLibrary) savePhotoJob(j photoJob) error {
	if !j.valid() {
		return errors.New("invalid photo recovery intent")
	}
	encoded, err := json.Marshal(j)
	if err != nil {
		return err
	}
	journalStage := ".uploads/photo-journal-" + identity.NewID()
	file, err := l.root.OpenFile(journalStage, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer l.root.Remove(journalStage)
	_, writeErr := file.Write(encoded)
	syncErr, closeErr := file.Sync(), file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	if err := l.root.Link(journalStage, j.path()); err != nil {
		return err
	}
	if err := l.syncPhotoDirectory(".photo-jobs"); err != nil {
		_ = l.root.Remove(j.path())
		return err
	}
	return nil
}

func (l *mediaLibrary) publishPhoto(ctx context.Context, owner mediaOwner, source, sourceSHA, stage string, m, old mediaItem) (*photoJob, error) {
	j := photoJob{ID: m.ID, Owner: owner, Source: owner.path(source), SourceSHA: sourceSHA, Target: owner.path(m.Filename), TargetSHA: m.SHA256, Thumbnail: m.ThumbnailKey, OldThumbnail: old.ThumbnailKey}
	if m.Kind == "video" {
		j.Kind = "video"
	}
	if _, err := l.root.Lstat(j.path()); err == nil {
		return nil, errors.New("photo recovery is pending; rescan before replacing this photo")
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if info, err := l.root.Lstat(j.Target); err == nil {
		if !info.Mode().IsRegular() {
			return nil, errMediaConflict
		}
		allowedSHA := old.SHA256
		if j.Source == j.Target {
			allowedSHA = sourceSHA
		} else if old.Filename != m.Filename {
			return nil, errMediaConflict
		}
		actual, err := l.hashPhotoPath(ctx, j.Target)
		if err != nil || actual != allowedSHA {
			return nil, errMediaConflict
		}
		j.Backup, j.BackupSHA = ".uploads/photo-backup-"+identity.NewID(), actual
		if err := l.root.Link(j.Target, j.Backup); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err := l.syncPhotoDirectory(".uploads"); err != nil {
		return nil, err
	}
	if err := l.savePhotoJob(j); err != nil {
		if j.Backup != "" {
			_ = l.root.Remove(j.Backup)
		}
		return nil, err
	}
	var err error
	if j.Backup != "" {
		err = l.root.Rename(stage, j.Target)
	} else {
		err = l.root.Link(stage, j.Target)
	}
	if err != nil {
		if j.Backup != "" {
			_ = l.root.Remove(j.Backup)
		}
		_ = l.root.Remove(j.path())
		return nil, err
	}
	if err := l.syncPhotoDirectory(owner.directory()); err != nil {
		_ = l.rollbackPhotoJob(ctx, j)
		return nil, err
	}
	return &j, nil
}

// Capture the path first, then verify captured bytes. A concurrent replacement
// can never cause an unverified source file to be unlinked.
func (l *mediaLibrary) retirePhoto(ctx context.Context, path, expected string) error {
	if _, err := l.root.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	trash := ".uploads/photo-retired-" + identity.NewID()
	if err := l.root.Rename(path, trash); err != nil {
		return err
	}
	actual, err := l.hashPhotoPath(ctx, trash)
	if err != nil || actual != expected {
		restoreErr := l.root.Link(trash, path)
		if restoreErr == nil {
			_ = l.root.Remove(trash)
			if err == nil && actual != expected {
				return errPhotoSourceChanged
			}
		}
		return fmt.Errorf("source changed or cannot be verified; retained at %s: %w", trash, errors.Join(err, restoreErr, errMediaConflict))
	}
	return l.root.Remove(trash)
}

func (l *mediaLibrary) finishPhotoJob(ctx context.Context, j photoJob) error {
	actual, err := l.hashPhotoPath(ctx, j.Target)
	if err != nil || actual != j.TargetSHA {
		return errors.New("retained photo changed; original cleanup postponed")
	}
	if thumb, err := l.root.Stat(j.thumbnailPath()); err != nil || thumb.Size() <= 0 {
		return errors.New("photo thumbnail missing; original cleanup postponed")
	}
	if j.Source != j.Target {
		if err := l.retirePhoto(ctx, j.Source, j.SourceSHA); err != nil {
			if !errors.Is(err, errPhotoSourceChanged) {
				return err
			}
			if l.logger != nil {
				l.logger.Warn("photo source changed after conversion; new bytes retained for next scan", "path", j.Source)
			}
		}
	}
	if j.Backup != "" {
		if err := l.retirePhoto(ctx, j.Backup, j.BackupSHA); err != nil {
			return err
		}
	}
	if j.Obsolete != "" {
		if err := l.retirePhoto(ctx, j.Obsolete, j.ObsoleteSHA); err != nil && !errors.Is(err, errPhotoSourceChanged) {
			return err
		}
	}
	if err := l.syncPhotoDirectory(j.Owner.directory()); err != nil {
		return err
	}
	if err := l.syncPhotoDirectory(".uploads"); err != nil {
		return err
	}
	return l.removePhotoJob(j)
}

func (l *mediaLibrary) rollbackPhotoJob(ctx context.Context, j photoJob) error {
	actual, err := l.hashPhotoPath(ctx, j.Target)
	// A keep-original update never published a new target; only its obsolete
	// previous master was scheduled for cleanup after the SQL commit.
	if j.Obsolete != "" {
		return l.removePhotoJob(j)
	}
	restored := err == nil && j.Backup != "" && actual == j.BackupSHA
	if !restored && err == nil && actual == j.TargetSHA {
		if err := l.retirePhoto(ctx, j.Target, j.TargetSHA); err != nil {
			return err
		}
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	} else if err == nil && !restored {
		return errors.New("photo target changed; rollback deferred")
	}
	if j.Backup != "" && !restored {
		if err := l.root.Link(j.Backup, j.Target); err != nil {
			return err
		}
		if err := l.syncPhotoDirectory(j.Owner.directory()); err != nil {
			return err
		}
	}
	if j.Backup != "" {
		if err := l.retirePhoto(ctx, j.Backup, j.BackupSHA); err != nil {
			return err
		}
	}
	if j.Thumbnail != j.OldThumbnail {
		_ = l.root.Remove(j.thumbnailPath())
	}
	if err := l.syncPhotoDirectory(j.Owner.directory()); err != nil {
		return err
	}
	return l.removePhotoJob(j)
}

func (j photoJob) thumbnailPath() string {
	ext := ".jxl"
	if j.Kind == "video" {
		ext = ".jpg"
	}
	return ".thumbnails/" + j.Thumbnail + ext
}

func (l *mediaLibrary) removePhotoJob(j photoJob) error {
	if err := l.root.Remove(j.path()); err != nil {
		return err
	}
	return l.syncPhotoDirectory(".photo-jobs")
}

func (l *mediaLibrary) recoverPhotoJobs(ctx context.Context) error {
	entries, err := fs.ReadDir(l.root.FS(), ".photo-jobs")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		file, err := l.root.Open(".photo-jobs/" + entry.Name())
		if err != nil {
			return err
		}
		var job photoJob
		err = json.NewDecoder(io.LimitReader(file, 64<<10)).Decode(&job)
		file.Close()
		if err != nil || !job.valid() || entry.Name() != job.ID+".json" {
			return errors.New("invalid photo recovery journal")
		}
		m, lookupErr := l.store.ownedMediaByID(ctx, job.Owner, job.ID)
		if lookupErr != nil && !errors.Is(lookupErr, errMediaNotFound) {
			return lookupErr
		}
		if lookupErr == nil && m.needsHEIFRepair() && job.Source != job.Target && m.SHA256 == job.TargetSHA {
			// Never finish legacy cleanup by deleting a HEIC that the corrected
			// decoder needs. Release the intent only when a source is available.
			if source, sourceErr := l.root.Stat(job.Source); sourceErr == nil && source.Mode().IsRegular() && source.Size() > 0 {
				if job.Backup != "" {
					if err := l.retirePhoto(ctx, job.Backup, job.BackupSHA); err != nil {
						return err
					}
				}
				if err := l.removePhotoJob(job); err != nil {
					return err
				}
			}
			l.logFile("legacy HEIC cleanup paused; original retained for repair", m, nil)
			continue
		}
		if lookupErr == nil && m.owner().path(m.Filename) == job.Target && m.SHA256 == job.TargetSHA && m.ThumbnailKey == job.Thumbnail && m.ThumbnailStatus == "ready" {
			err = l.finishPhotoJob(ctx, job)
		} else {
			err = l.rollbackPhotoJob(ctx, job)
		}
		if err != nil {
			l.logFile("photo recovery deferred", m, err)
		}
	}
	return nil
}
