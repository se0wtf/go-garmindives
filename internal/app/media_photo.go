package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	identity "github.com/se0wtf/go-garmindives/internal/identity"
	mediaproc "github.com/se0wtf/go-garmindives/internal/media"
)

const photoProfileVersion = 1
const photoKeepBytes int64 = 1_000_000

type photoState struct {
	Version            int    `json:"version"`
	SourceFilename     string `json:"source_filename,omitempty"`
	SourceSHA256       string `json:"source_sha256,omitempty"`
	Projection         string `json:"projection,omitempty"`
	ProjectionFormat   string `json:"projection_format,omitempty"`
	HEIFDecoderVersion int    `json:"heif_decoder_version,omitempty"`
}

func (m mediaItem) needsHEIFRepair() bool {
	state := m.photoState()
	if state.Version != photoProfileVersion || state.HEIFDecoderVersion >= 1 {
		return false
	}
	_, storedType := mediaType(m.Filename)
	_, sourceType := mediaType(state.SourceFilename)
	return m.ContentType == "image/heic" || storedType == "image/heic" || sourceType == "image/heic"
}

func (m mediaItem) heifRepairMessage() string {
	return "HEIC source required: restore " + m.photoState().SourceFilename + " and rescan, or upload that original again; the previous HEIC decoder may have retained only one tile"
}

func (m mediaItem) photoState() photoState {
	var envelope struct {
		Photo photoState `json:"garmindives_photo"`
	}
	_ = json.Unmarshal(m.Probe, &envelope)
	return envelope.Photo
}

func (m *mediaItem) setPhotoState(state photoState) {
	var document map[string]json.RawMessage
	if json.Unmarshal(m.Probe, &document) != nil || document == nil {
		document = map[string]json.RawMessage{}
	}
	document["garmindives_photo"], _ = json.Marshal(state)
	m.Probe, _ = json.Marshal(document)
}

func (m mediaItem) thumbnailPath() string {
	ext := ".jpg"
	if m.photoState().Version == photoProfileVersion {
		ext = ".jxl"
	}
	return ".thumbnails/" + m.ThumbnailKey + ext
}

func convertedPhotoFilename(name string) string {
	if strings.EqualFold(filepath.Ext(name), ".jxl") {
		return name
	}
	if len(name)+4 <= 255 {
		return name + ".jxl"
	}
	sum := sha256.Sum256([]byte(name))
	prefix := name[:238]
	for !utf8.ValidString(prefix) {
		prefix = prefix[:len(prefix)-1]
	}
	return prefix + "-" + hex.EncodeToString(sum[:6]) + ".jxl"
}

func (l *mediaLibrary) lookupPhotoAlias(ctx context.Context, owner mediaOwner, name string) (mediaItem, error) {
	m, err := l.store.ownedMediaByFilename(ctx, owner, name)
	if !errors.Is(err, errMediaNotFound) {
		return m, err
	}
	target := convertedPhotoFilename(name)
	if target == name {
		return m, err
	}
	converted, lookupErr := l.store.ownedMediaByFilename(ctx, owner, target)
	if lookupErr == nil && converted.photoState().SourceFilename == name {
		return converted, nil
	}
	if lookupErr != nil && !errors.Is(lookupErr, errMediaNotFound) {
		return converted, lookupErr
	}
	return m, err
}

func hashPhoto(ctx context.Context, input *os.File) (string, error) {
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, mediaContextReader{ctx: ctx, r: input}); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (l *mediaLibrary) hashPhotoPath(ctx context.Context, path string) (string, error) {
	file, err := l.root.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	return hashPhoto(ctx, file)
}

func (l *mediaLibrary) photoStage() (*os.File, string, error) {
	name := ".uploads/photo-" + identity.NewID()
	file, err := l.root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	return file, name, err
}

func (l *mediaLibrary) makePhotoThumbnail(ctx context.Context, input *os.File, m *mediaItem) error {
	if m.needsHEIFRepair() && m.photoState().SourceFilename != "" && !mediaproc.IsHEIF(input) {
		return errors.New(m.heifRepairMessage())
	}
	processor, ok := l.processor.(mediaproc.PhotoProcessor)
	if !ok {
		return errors.New("photo processor is unavailable")
	}
	m.resolveProjection()
	layout := m.StereoLayout
	if layout == "auto" {
		layout = m.DetectedStereoLayout
	}
	output, stage, err := l.photoStage()
	if err != nil {
		return err
	}
	defer output.Close()
	defer l.root.Remove(stage)
	_, err = processor.Photo(ctx, input, output, mediaproc.PhotoOptions{SmallSide: 480, Distance: 2, Effort: 8, Preview: true,
		ThumbnailOptions: mediaproc.ThumbnailOptions{StereoLayout: layout, Projection: m.EffectiveProjection, ProjectionFormat: m.ProjectionFormat}})
	if err != nil {
		return err
	}
	key := m.thumbnailKey()
	if err := l.root.Rename(stage, ".thumbnails/"+key+".jxl"); err != nil {
		return err
	}
	if err := l.syncPhotoDirectory(".thumbnails"); err != nil {
		return err
	}
	m.ThumbnailKey, m.ThumbnailStatus, m.ThumbnailError = key, "ready", ""
	return nil
}

func (l *mediaLibrary) indexPhoto(ctx context.Context, owner mediaOwner, name string, input *os.File, info fs.FileInfo, old mediaItem, fresh bool) (mediaItem, string, error) {
	sourceSHA, err := hashPhoto(ctx, input)
	if err != nil {
		return old, "", err
	}
	heifInput := mediaproc.IsHEIF(input)
	if old.Filename == name && old.SHA256 == sourceSHA && old.needsHEIFRepair() && !heifInput {
		// Existing JXL bytes cannot recover tiles discarded by the old decoder.
		m := old
		message := m.heifRepairMessage()
		action := "unchanged"
		if m.ThumbnailStatus != "error" || m.ThumbnailError != message {
			m.ThumbnailStatus, m.ThumbnailError, m.UpdatedAt = "error", message, nowString()
			if err := l.store.saveMedia(ctx, m); err != nil {
				return old, "", err
			}
			action = "updated"
		}
		m.setURLs()
		l.logFile("HEIC original needed to repair previous conversion", m, errors.New(message))
		return m, action, nil
	}
	// A committed conversion can leave a source behind after interrupted cleanup.
	if !old.needsHEIFRepair() && old.Filename != name && old.photoState().SourceFilename == name && old.photoState().SourceSHA256 == sourceSHA {
		if err := l.recoverPhotoJobs(ctx); err != nil {
			return old, "", err
		}
		thumb, thumbErr := l.root.Stat(old.thumbnailPath())
		if current, err := l.hashPhotoPath(ctx, owner.path(old.Filename)); err == nil && current == old.SHA256 && thumbErr == nil && thumb.Size() > 0 {
			if err := l.retirePhoto(ctx, owner.path(name), sourceSHA); err != nil {
				return old, "", err
			}
			return old, "unchanged", nil
		}
	}
	m := old
	if fresh {
		m = mediaItem{ID: identity.NewID(), DiveID: owner.DiveID, TripID: owner.TripID, StereoLayout: "auto", Projection: "auto", CreatedAt: nowString()}
	}
	m.Filename, m.Kind, m.ContentType = name, "photo", ""
	_, m.ContentType = mediaType(name)
	if mediaproc.IsJPEGXL(input) {
		m.ContentType = "image/jxl"
	} else if heifInput {
		m.ContentType = "image/heic"
	}
	m.SizeBytes, m.ModifiedNS, m.SHA256, m.UpdatedAt = info.Size(), info.ModTime().UnixNano(), sourceSHA, nowString()
	m.ThumbnailKey, m.ThumbnailStatus, m.ThumbnailError = "", "error", ""
	m.Probe = json.RawMessage(`{}`)
	processed := old.photoState().Version == photoProfileVersion && old.Filename == name && old.SHA256 == sourceSHA
	if heifInput && old.photoState().HEIFDecoderVersion < 1 {
		processed = false
	}
	probe, processErr := l.processor.Inspect(ctx, input)
	if processErr == nil {
		m.Width, m.Height, m.DurationSeconds, m.DetectedStereoLayout, m.Probe = probe.Width, probe.Height, probe.Duration, probe.StereoLayout, probe.JSON
	}
	if processed {
		m.DetectedStereoLayout = old.DetectedStereoLayout
		m.setPhotoState(old.photoState())
	}
	original := m
	fail := func(cause error) (mediaItem, string, error) {
		if m.ThumbnailKey != old.ThumbnailKey {
			l.removeThumbnail(m)
		}
		if ctx.Err() != nil {
			return old, "", ctx.Err()
		}
		current, statErr := l.root.Stat(owner.path(name))
		if statErr != nil || !os.SameFile(info, current) || current.Size() != info.Size() || !current.ModTime().Equal(info.ModTime()) {
			return old, "", errors.New("photo changed during processing; retry after copying finishes")
		}
		if !fresh && old.Filename != name {
			return old, "", cause
		}
		original.ThumbnailError = cause.Error()
		if err := l.store.saveMedia(ctx, original); err != nil {
			return old, "", err
		}
		if old.ThumbnailKey != "" {
			l.removeThumbnail(old)
		}
		original.setURLs()
		l.logFile("photo processing failed; source retained", original, cause)
		if fresh {
			return original, "added", nil
		}
		return original, "updated", nil
	}
	if processErr != nil {
		return fail(processErr)
	}
	if min(probe.Width, probe.Height) <= 0 {
		return fail(errors.New("photo has invalid dimensions"))
	}
	state := photoState{Version: photoProfileVersion, Projection: probe.Projection, ProjectionFormat: probe.ProjectionFormat}
	if heifInput {
		state.HEIFDecoderVersion = 1
	}
	processor := l.processor.(mediaproc.PhotoProcessor)
	convert := !processed && !(info.Size() < photoKeepBytes && min(probe.Width, probe.Height) < 2048)
	master := input
	stage := ""
	if convert {
		if probe.Duration > 0 {
			return fail(errors.New("animated photos require an animation-preserving encoder; source retained"))
		}
		output, temporary, err := l.photoStage()
		if err != nil {
			return fail(err)
		}
		defer output.Close()
		defer l.root.Remove(temporary)
		encodedProbe, err := processor.Photo(ctx, input, output, mediaproc.PhotoOptions{SmallSide: 1080, Distance: 1.5, Effort: 7})
		if err != nil {
			return fail(err)
		}
		m.Filename, m.ContentType = convertedPhotoFilename(name), "image/jxl"
		m.SHA256, err = hashPhoto(ctx, output)
		if err != nil {
			return fail(err)
		}
		encodedInfo, err := output.Stat()
		if err != nil || encodedInfo.Size() <= 0 {
			return fail(errors.New("encoded photo is empty"))
		}
		m.SizeBytes, m.ModifiedNS, m.Width, m.Height, m.Probe = encodedInfo.Size(), encodedInfo.ModTime().UnixNano(), encodedProbe.Width, encodedProbe.Height, encodedProbe.JSON
		state.SourceFilename, state.SourceSHA256 = name, sourceSHA
		master, stage = output, temporary
	} else if processed {
		state = old.photoState()
	}
	m.setPhotoState(state)
	if err := l.makeThumbnail(ctx, master, &m); err != nil {
		return fail(err)
	}
	after, err := l.root.Stat(owner.path(name))
	if err != nil || !os.SameFile(info, after) || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return fail(errors.New("photo changed during processing; retry after copying finishes"))
	}
	var job *photoJob
	if convert {
		job, err = l.publishPhoto(ctx, owner, name, sourceSHA, stage, m, old)
		if err != nil {
			return fail(err)
		}
	} else if !fresh && old.Filename != name {
		job = &photoJob{ID: m.ID, Owner: owner, Source: owner.path(name), SourceSHA: sourceSHA, Target: owner.path(name), TargetSHA: m.SHA256, Thumbnail: m.ThumbnailKey, OldThumbnail: old.ThumbnailKey, Obsolete: owner.path(old.Filename), ObsoleteSHA: old.SHA256}
		if err := l.savePhotoJob(*job); err != nil {
			return fail(err)
		}
	}
	if err := l.store.saveMedia(ctx, m); err != nil {
		if job != nil {
			cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = l.recoverPhotoJobs(cleanup)
			cancel()
		}
		return old, "", err
	}
	if job != nil {
		if err := l.finishPhotoJob(ctx, *job); err != nil {
			l.logFile("photo saved; source cleanup deferred", m, err)
		}
	}
	if old.ThumbnailKey != m.ThumbnailKey {
		l.removeThumbnail(old)
	}
	m.setURLs()
	action := "updated"
	if fresh {
		action = "added"
	}
	l.logFile("photo "+action, m, nil)
	return m, action, nil
}
