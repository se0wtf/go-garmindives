package app

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	identity "github.com/se0wtf/go-garmindives/internal/identity"
	mediaproc "github.com/se0wtf/go-garmindives/internal/media"
)

const videoProfileVersion = 1

type videoState struct {
	Version          int    `json:"version"`
	SourceFilename   string `json:"source_filename,omitempty"`
	SourceSHA256     string `json:"source_sha256,omitempty"`
	Projection       string `json:"projection,omitempty"`
	ProjectionFormat string `json:"projection_format,omitempty"`
	StereoLayout     string `json:"stereo_layout,omitempty"`
}

func (m mediaItem) videoState() videoState {
	var envelope struct {
		Video videoState `json:"garmindives_video"`
	}
	_ = json.Unmarshal(m.Probe, &envelope)
	return envelope.Video
}
func (m *mediaItem) setVideoState(state videoState) {
	var document map[string]json.RawMessage
	if json.Unmarshal(m.Probe, &document) != nil || document == nil {
		document = map[string]json.RawMessage{}
	}
	document["garmindives_video"], _ = json.Marshal(state)
	m.Probe, _ = json.Marshal(document)
}

func convertedVideoFilename(name string) string {
	if ext := strings.ToLower(filepath.Ext(name)); ext == ".mp4" || ext == ".m4v" {
		return name
	}
	// The photo helper already preserves long UTF-8 basenames with a hash suffix.
	return strings.TrimSuffix(convertedPhotoFilename(name), ".jxl") + ".mp4"
}

func (l *mediaLibrary) videoCapture(ctx context.Context, owner mediaOwner, input *os.File) *photoCapture {
	capture, err := mediaproc.NewCaptureReader().ReadCapture(ctx, input)
	if err != nil || capture == nil {
		return nil
	}
	dive := pendingDive{}
	if owner.DiveID > 0 {
		var start, zone string
		if err := l.store.db.QueryRowContext(ctx, l.store.bind(`SELECT start_time,timezone FROM dives WHERE id=?`), owner.DiveID).Scan(&start, &zone); err == nil {
			_, dive.location, dive.timezone, _ = pendingDiveStart(start, zone)
		}
	}
	resolved, _ := captureForDive(capture, dive)
	return resolved
}

func (l *mediaLibrary) indexVideo(ctx context.Context, owner mediaOwner, name string, input *os.File, info fs.FileInfo, old mediaItem, fresh bool, seed *pendingImport) (mediaItem, string, error) {
	sourceSHA, err := hashPhoto(ctx, input)
	if err != nil {
		return old, "", err
	}
	if seed != nil && sourceSHA != seed.SHA {
		return old, "", errPendingChanged
	}
	previous := old.videoState()
	if old.Filename != name && previous.SourceFilename == name && previous.SourceSHA256 == sourceSHA {
		if err := l.recoverPhotoJobs(ctx); err != nil {
			return old, "", err
		}
		thumb, thumbErr := l.root.Stat(old.thumbnailPath())
		if sha, err := l.hashPhotoPath(ctx, owner.path(old.Filename)); err == nil && sha == old.SHA256 && thumbErr == nil && thumb.Size() > 0 {
			if err := l.retirePhoto(ctx, owner.path(name), sourceSHA); err != nil {
				return old, "", err
			}
			return old, "unchanged", nil
		}
	}
	m := old
	if fresh {
		m = mediaItem{ID: identity.NewID(), DiveID: owner.DiveID, TripID: owner.TripID, StereoLayout: "auto", Projection: "auto", CreatedAt: nowString()}
		if seed != nil {
			m.ID, m.Capture = seed.ID, seed.Capture
		}
	}
	if m.Capture == nil && seed == nil {
		m.Capture = l.videoCapture(ctx, owner, input)
	}
	m.Filename, m.Kind, m.ContentType = name, "video", ""
	_, m.ContentType = mediaType(name)
	m.SizeBytes, m.ModifiedNS, m.SHA256, m.UpdatedAt = info.Size(), info.ModTime().UnixNano(), sourceSHA, nowString()
	m.ThumbnailKey, m.ThumbnailStatus, m.ThumbnailError = "", "error", ""
	m.Probe = json.RawMessage(`{}`)
	probe, processErr := l.processor.Inspect(ctx, input)
	if processErr == nil {
		m.Width, m.Height, m.DurationSeconds, m.DetectedStereoLayout, m.Probe = probe.Width, probe.Height, probe.Duration, probe.StereoLayout, probe.JSON
	}
	processed := previous.Version == videoProfileVersion && old.Filename == name && old.SHA256 == sourceSHA
	if processed {
		m.setVideoState(previous)
		m.DetectedStereoLayout = previous.StereoLayout
	}
	original := m
	fail := func(cause error) (mediaItem, string, error) {
		if m.ThumbnailKey != old.ThumbnailKey {
			l.removeThumbnail(m)
		}
		if ctx.Err() != nil {
			return old, "", ctx.Err()
		}
		if seed != nil {
			return old, "", cause
		}
		current, statErr := l.root.Stat(owner.path(name))
		if statErr != nil || !os.SameFile(info, current) || current.Size() != info.Size() || !current.ModTime().Equal(info.ModTime()) {
			return old, "", errors.New("video changed during processing; retry after copying finishes")
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
		l.logFile("video processing failed; source retained", original, cause)
		if fresh {
			return original, "added", nil
		}
		return original, "updated", nil
	}
	if processErr != nil {
		return fail(processErr)
	}
	if probe.Duration <= 0 {
		return fail(errors.New("video has no usable duration"))
	}
	state := videoState{Version: videoProfileVersion, Projection: probe.Projection, ProjectionFormat: probe.ProjectionFormat, StereoLayout: probe.StereoLayout}
	if processed {
		state = previous
	}
	master, stage := input, ""
	convert := !mediaproc.VideoConforms(probe, name)
	if convert {
		if l.logger != nil {
			l.logger.Info("converting video", "filename", name, "codec", probe.VideoCodec, "width", probe.Width, "height", probe.Height)
		}
		output, temporary, err := l.photoStage()
		if err != nil {
			return fail(err)
		}
		defer output.Close()
		defer l.root.Remove(temporary)
		encoded, err := l.processor.(mediaproc.VideoProcessor).Video(ctx, input, output, probe)
		if err != nil {
			return fail(err)
		}
		if !mediaproc.VideoConforms(encoded, "video.mp4") {
			return fail(errors.New("encoded video does not satisfy the H.264/1080p profile"))
		}
		m.Filename, m.ContentType = convertedVideoFilename(name), "video/mp4"
		m.SHA256, err = hashPhoto(ctx, output)
		if err != nil {
			return fail(err)
		}
		encodedInfo, err := output.Stat()
		if err != nil || encodedInfo.Size() <= 0 {
			return fail(errors.New("encoded video is empty"))
		}
		m.SizeBytes, m.ModifiedNS, m.Width, m.Height, m.DurationSeconds, m.Probe = encodedInfo.Size(), encodedInfo.ModTime().UnixNano(), encoded.Width, encoded.Height, encoded.Duration, encoded.JSON
		state.SourceFilename, state.SourceSHA256 = name, sourceSHA
		master, stage = output, temporary
	}
	m.setVideoState(state)
	m.DetectedStereoLayout = state.StereoLayout
	if err := l.makeThumbnail(ctx, master, &m); err != nil {
		return fail(err)
	}
	after, err := l.root.Stat(owner.path(name))
	if err != nil || !os.SameFile(info, after) || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return fail(errors.New("video changed during processing; retry after copying finishes"))
	}
	// Hash again before publication: source replacement must never authorize
	// deleting bytes that were not used for the verified master.
	if currentSHA, err := hashPhoto(ctx, input); err != nil || currentSHA != sourceSHA {
		return fail(errors.New("video bytes changed during processing"))
	}
	var job *photoJob
	if convert {
		job, err = l.publishPhoto(ctx, owner, name, sourceSHA, stage, m, old)
		if err != nil {
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
			l.logFile("video saved; source cleanup deferred", m, err)
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
	l.logFile("video "+action, m, nil)
	return m, action, nil
}
