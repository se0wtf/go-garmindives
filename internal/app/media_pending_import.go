package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"time"

	identity "github.com/se0wtf/go-garmindives/internal/identity"
)

func (l *mediaLibrary) reservePending(ctx context.Context, item *pendingItem, diveID int64, capture *photoCapture) error {
	if err := l.lock(ctx); err != nil {
		return err
	}
	defer l.unlock()
	owner := mediaOwner{DiveID: diveID}
	exists, err := l.store.ownerExists(ctx, owner)
	if err != nil {
		return err
	}
	if !exists {
		return errDiveNotFound
	}
	if _, err := l.lookupPhotoAlias(ctx, owner, item.Filename); err == nil {
		return errMediaConflict
	} else if !errors.Is(err, errMediaNotFound) {
		return err
	}
	if _, err := l.root.Lstat(owner.path(item.Filename)); err == nil {
		return errMediaConflict
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	item.State, item.TargetDiveID, item.MediaID = "assigning", diveID, identity.NewID()
	item.TransferSHA, item.TransferCapture = item.SHA256, capture
	item.Reason, item.Error, item.UpdatedAt = "assigning", "", nowString()
	return l.store.savePending(ctx, *item)
}

func (l *mediaLibrary) importPending(ctx context.Context, item pendingItem, input *os.File) (result mediaItem, err error) {
	if err = l.lock(ctx); err != nil {
		return result, err
	}
	defer l.unlock()
	owner := mediaOwner{DiveID: item.TargetDiveID}
	if err = l.recoverPhotoJobs(ctx); err != nil {
		return result, err
	}
	if m, lookupErr := l.store.ownedMediaByID(ctx, owner, item.MediaID); lookupErr == nil {
		actual, hashErr := l.hashPhotoPath(ctx, owner.path(m.Filename))
		if hashErr != nil || actual != m.SHA256 {
			return result, errPendingChanged
		}
		if thumbnail, thumbErr := l.root.Stat(m.thumbnailPath()); m.ThumbnailStatus != "ready" || thumbErr != nil || thumbnail.Size() == 0 {
			m, _, err = l.indexOwnedFile(ctx, owner, m.Filename, true)
			if err != nil {
				return result, err
			}
		}
		if m.ThumbnailStatus != "ready" {
			return result, errors.New("media processing is incomplete")
		}
		return m, nil
	} else if !errors.Is(lookupErr, errMediaNotFound) {
		return result, lookupErr
	}
	exists, err := l.store.ownerExists(ctx, owner)
	if err != nil {
		return result, err
	}
	if !exists {
		return result, errDiveNotFound
	}
	if existing, err := l.lookupPhotoAlias(ctx, owner, item.Filename); err == nil && existing.ID != item.MediaID {
		return result, errMediaConflict
	} else if err != nil && !errors.Is(err, errMediaNotFound) {
		return result, err
	}
	if err = l.root.MkdirAll(owner.directory(), 0o755); err != nil {
		return result, err
	}
	// The durable receipt predates publication, so recovery and ordinary scans
	// select the same media ID even after an interrupted SQL commit.
	defer func() {
		if err == nil {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if recoverErr := l.recoverPhotoJobs(cleanup); recoverErr != nil {
			return
		}
		if _, lookupErr := l.store.ownedMediaByID(cleanup, owner, item.MediaID); !errors.Is(lookupErr, errMediaNotFound) {
			return
		}
		if actual, hashErr := l.hashPhotoPath(cleanup, owner.path(item.Filename)); hashErr == nil && actual == item.TransferSHA {
			_ = l.retirePhoto(cleanup, owner.path(item.Filename), item.TransferSHA)
		}
	}()
	path := owner.path(item.Filename)
	if actual, hashErr := l.hashPhotoPath(ctx, path); hashErr == nil {
		if actual != item.TransferSHA {
			return result, errMediaConflict
		}
	} else if !errors.Is(hashErr, fs.ErrNotExist) {
		return result, hashErr
	} else {
		if input == nil {
			return result, fs.ErrNotExist
		}
		stage, temporary, stageErr := l.photoStage()
		if stageErr != nil {
			return result, stageErr
		}
		defer stage.Close()
		defer l.root.Remove(temporary)
		if _, err = input.Seek(0, io.SeekStart); err != nil {
			return result, err
		}
		hash := sha256.New()
		if _, err = io.Copy(io.MultiWriter(stage, hash), mediaContextReader{ctx: ctx, r: input}); err != nil {
			return result, err
		}
		if hex.EncodeToString(hash.Sum(nil)) != item.TransferSHA {
			return result, errPendingChanged
		}
		if err = stage.Sync(); err != nil {
			return result, err
		}
		if err = l.root.Link(temporary, path); err != nil {
			if errors.Is(err, fs.ErrExist) {
				return result, errMediaConflict
			}
			return result, err
		}
		if err = l.syncPhotoDirectory(owner.directory()); err != nil {
			return result, err
		}
	}
	result, _, err = l.indexOwnedFile(ctx, owner, item.Filename, true)
	if err == nil && (result.ID != item.MediaID || result.ThumbnailStatus != "ready") {
		err = errors.New("pending import did not produce complete media")
	}
	return result, err
}

func (l *mediaLibrary) abortPending(ctx context.Context, item pendingItem) error {
	if item.State != "assigning" {
		return nil
	}
	if err := l.lock(ctx); err != nil {
		return err
	}
	defer l.unlock()
	if err := l.recoverPhotoJobs(ctx); err != nil {
		return err
	}
	owner := mediaOwner{DiveID: item.TargetDiveID}
	if _, err := l.store.ownedMediaByID(ctx, owner, item.MediaID); err == nil {
		return errPendingAssigned
	} else if !errors.Is(err, errMediaNotFound) {
		return err
	}
	path := owner.path(item.Filename)
	if actual, err := l.hashPhotoPath(ctx, path); err == nil && actual == item.TransferSHA {
		return l.retirePhoto(ctx, path, item.TransferSHA)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
