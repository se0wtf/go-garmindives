package app

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	mediaproc "github.com/se0wtf/go-garmindives/internal/media"
)

type countedVideoProcessor struct {
	mediaproc.Processor
	conversions int
	fail        bool
	sphere      bool
}

func (p *countedVideoProcessor) Video(ctx context.Context, input, output *os.File, probe mediaproc.Probe) (mediaproc.Probe, error) {
	p.conversions++
	if p.fail {
		return mediaproc.Probe{}, errors.New("test encoder failure")
	}
	return p.Processor.(mediaproc.VideoProcessor).Video(ctx, input, output, probe)
}
func (p *countedVideoProcessor) Inspect(ctx context.Context, input *os.File) (mediaproc.Probe, error) {
	probe, err := p.Processor.Inspect(ctx, input)
	if p.sphere && filepath.Ext(input.Name()) == ".mov" {
		probe.Projection, probe.ProjectionFormat = "360", "equirectangular"
	}
	return probe, err
}

func appVideoFixture(t *testing.T, size, codec, ext string) []byte {
	t.Helper()
	p := mediaproc.NewProcessor()
	if p.FFmpeg == "" || p.FFprobe == "" {
		t.Skip("install FFmpeg")
	}
	path := filepath.Join(t.TempDir(), "source."+ext)
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-f", "lavfi", "-i", "color=c=blue:s=" + size + ":r=5", "-t", "0.4", "-c:v", codec, "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-threads", "1"}
	if codec == "libx265" {
		args = append(args, "-x265-params", "pools=none:frame-threads=1:log-level=error")
	}
	args = append(args, path)
	if output, err := exec.Command(p.FFmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("video fixture: %s %v", output, err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestVideoUploadAndRescanNormalization(t *testing.T) {
	for _, test := range []struct {
		name, size, codec, ext string
		converted              bool
		w, h                   int
	}{
		{"compatible", "320x180", "libx264", "mp4", false, 320, 180},
		{"large", "3840x2160", "libx264", "mp4", true, 1920, 1080},
		{"HEVC", "320x180", "libx265", "mov", true, 320, 180},
		{"portrait", "1080x2160", "libx264", "mp4", true, 960, 1920},
	} {
		t.Run(test.name, func(t *testing.T) {
			l, _ := mediaTestLibrary(t)
			p := &countedVideoProcessor{Processor: mediaproc.NewProcessor()}
			l.processor = p
			body := appVideoFixture(t, test.size, test.codec, test.ext)
			name := "Reef." + test.ext
			m, err := l.Upload(context.Background(), 42, name, bytes.NewReader(body), nil)
			if err != nil || m.ThumbnailStatus != "ready" || m.Width != test.w || m.Height != test.h || m.videoState().Version != videoProfileVersion {
				t.Fatalf("upload: %+v %v", m, err)
			}
			if (p.conversions == 1) != test.converted {
				t.Fatalf("conversions=%d", p.conversions)
			}
			retained, err := l.root.ReadFile(m.owner().path(m.Filename))
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Equal(retained, body) == test.converted {
				t.Fatal("unexpected change to retained bytes")
			}
			if test.ext == "mov" {
				if m.Filename != "Reef.mov.mp4" {
					t.Fatal(m.Filename)
				}
				if _, err := l.root.Stat("42/" + name); !errors.Is(err, fs.ErrNotExist) {
					t.Fatal("source not retired")
				}
			}
			description := "Requin"
			favorite := true
			m, err = l.Patch(context.Background(), 42, m.ID, mediaPatch{Description: &description, Favorite: &favorite})
			if err != nil {
				t.Fatal(err)
			}
			count := p.conversions
			for _, force := range []bool{false, true} {
				if _, err := l.Scan(context.Background(), force); err != nil {
					t.Fatal(err)
				}
			}
			saved, err := l.store.mediaByID(context.Background(), 42, m.ID)
			after, _ := l.root.ReadFile(m.owner().path(m.Filename))
			if err != nil || saved.Description != description || !saved.Favorite || !bytes.Equal(retained, after) || p.conversions != count {
				t.Fatal("rescan lost metadata, identity or recompressed the video")
			}
			if _, err := l.Upload(context.Background(), 42, name, bytes.NewReader(body), nil); !errors.Is(err, errMediaConflict) {
				t.Fatalf("alias collision: %v", err)
			}
		})
	}
}

func TestVideoFailureRollbackAndRetry(t *testing.T) {
	l, _ := mediaTestLibrary(t)
	body := appVideoFixture(t, "320x180", "libx264", "mov")
	old, err := l.Upload(context.Background(), 42, "Old.mov", bytes.NewReader(body), nil)
	if err != nil {
		t.Fatal(err)
	}
	processor := &countedVideoProcessor{Processor: mediaproc.NewProcessor(), fail: true}
	l.processor = processor
	if result, err := l.Scan(context.Background(), false); err != nil || result.Failed != 1 {
		t.Fatalf("encoder failure: %+v %v", result, err)
	}
	if source, _ := l.root.ReadFile("42/Old.mov"); !bytes.Equal(source, body) {
		t.Fatal("failed conversion deleted source")
	}
	processor.fail = false
	if _, err := l.store.db.Exec(`CREATE TRIGGER reject_video_rename BEFORE UPDATE OF filename ON dive_media WHEN NEW.filename != OLD.filename BEGIN SELECT RAISE(ABORT,'test SQL failure'); END`); err != nil {
		t.Fatal(err)
	}
	if result, err := l.Scan(context.Background(), false); err != nil || result.Failed != 1 {
		t.Fatalf("SQL failure: %+v %v", result, err)
	}
	if source, _ := l.root.ReadFile("42/Old.mov"); !bytes.Equal(source, body) {
		t.Fatal("rollback lost source")
	}
	if _, err := l.root.Stat("42/Old.mov.mp4"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("uncommitted master remained")
	}
	if _, err := l.store.db.Exec(`DROP TRIGGER reject_video_rename`); err != nil {
		t.Fatal(err)
	}
	if result, err := l.Scan(context.Background(), false); err != nil || result.Failed != 0 {
		t.Fatalf("retry: %+v %v", result, err)
	}
	m, err := l.store.mediaByID(context.Background(), 42, old.ID)
	if err != nil || m.Filename != "Old.mov.mp4" || m.ThumbnailStatus != "ready" {
		t.Fatalf("retry lost identity: %+v %v", m, err)
	}
}

func TestVideoTripScanAndSphereProvenance(t *testing.T) {
	l, _ := mediaTestLibrary(t)
	trip := createMediaTrip(t, newAPIHandler(l.store, testLogger(t)), "EG")
	l.processor = &countedVideoProcessor{Processor: mediaproc.NewProcessor(), sphere: true}
	body := appVideoFixture(t, "640x320", "libx264", "mov")
	writeTestMedia(t, l, "trips/"+trip.ID+"/Panorama.mov", body)
	if result, err := l.Scan(context.Background(), false); err != nil || result.Added != 1 || result.Failed != 0 {
		t.Fatalf("trip scan: %+v %v", result, err)
	}
	items, err := l.store.mediaForOwner(context.Background(), mediaOwner{TripID: trip.ID})
	if err != nil || len(items) != 1 {
		t.Fatal(err)
	}
	m := items[0]
	if m.EffectiveProjection != "360" || m.ProjectionFormat != "equirectangular" || m.Width != 640 || m.Height != 320 {
		t.Fatalf("sphere lost: %+v", m)
	}
	if _, err := l.Scan(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	m, err = l.store.ownedMediaByID(context.Background(), m.owner(), m.ID)
	if err != nil || m.EffectiveProjection != "360" {
		t.Fatal("forced scan lost spherical provenance")
	}
}

func TestVideoRecoveryJournalBeforeAndAfterCommit(t *testing.T) {
	for _, inPlace := range []bool{false, true} {
		for _, committed := range []bool{false, true} {
			name := "separate target"
			if inPlace {
				name = "same filename"
			}
			if committed {
				name += " committed"
			}
			t.Run(name, func(t *testing.T) {
				l, _ := mediaTestLibrary(t)
				ctx := context.Background()
				source := "Original.mov"
				if inPlace {
					source = "Original.mp4"
				}
				original := []byte("original video bytes")
				old, err := l.Upload(ctx, 42, source, bytes.NewReader(original), nil)
				if err != nil {
					t.Fatal(err)
				}
				m := old
				m.Filename, m.ContentType = convertedVideoFilename(source), "video/mp4"
				m.ThumbnailKey, m.ThumbnailStatus = strings.Repeat("a", 64), "ready"
				m.setVideoState(videoState{Version: videoProfileVersion, SourceFilename: source, SourceSHA256: old.SHA256})
				encoded := []byte("verified H264 output")
				writeTestMedia(t, l, ".uploads/video-stage", encoded)
				m.SHA256, err = l.hashPhotoPath(ctx, ".uploads/video-stage")
				if err != nil {
					t.Fatal(err)
				}
				writeTestMedia(t, l, m.thumbnailPath(), []byte("JPEG thumbnail"))
				job, err := l.publishPhoto(ctx, old.owner(), source, old.SHA256, ".uploads/video-stage", m, old)
				if err != nil || job.Kind != "video" {
					t.Fatalf("intent: %+v %v", job, err)
				}
				if committed {
					if err := l.store.saveMedia(ctx, m); err != nil {
						t.Fatal(err)
					}
				}
				reopened, err := openMediaLibrary(l.store, l.directory, l.maxUpload, testLogger(t))
				if err != nil {
					t.Fatal(err)
				}
				defer reopened.Close()
				if err := reopened.recoverPhotoJobs(ctx); err != nil {
					t.Fatal(err)
				}
				path, want := old.owner().path(source), original
				if committed {
					path, want = m.owner().path(m.Filename), encoded
				}
				if actual, err := reopened.root.ReadFile(path); err != nil || !bytes.Equal(actual, want) {
					t.Fatalf("recovered bytes: %q %v", actual, err)
				}
				if !inPlace {
					absent := m.owner().path(m.Filename)
					if committed {
						absent = old.owner().path(source)
					}
					if _, err := reopened.root.Stat(absent); !errors.Is(err, fs.ErrNotExist) {
						t.Fatalf("unexpected leftover %s", absent)
					}
				}
				if _, err := reopened.root.Stat(job.path()); !errors.Is(err, fs.ErrNotExist) {
					t.Fatal("video journal not retired")
				}
			})
		}
	}
}
