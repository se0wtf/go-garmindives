package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image/jpeg"
	"io/fs"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	mediaproc "github.com/se0wtf/go-garmindives/internal/media"
)

func heifFixture(t *testing.T, name string) []byte {
	t.Helper()
	photoTestTools(t)
	if mediaproc.NewProcessor().HEIF == "" {
		t.Skip("install libheif with its HEVC decoder")
	}
	body, err := os.ReadFile("../media/testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func legacyHEIFMedia(t *testing.T, library *mediaLibrary, source []byte) mediaItem {
	t.Helper()
	m, err := library.Upload(context.Background(), 42, "Grid.HEIC.jxl", strings.NewReader("legacy HEIC tile bytes"), nil)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(source)
	m.Width, m.Height = 512, 512
	m.Description, m.Favorite = "Le récif", true
	m.setPhotoState(photoState{Version: photoProfileVersion, SourceFilename: "Grid.HEIC", SourceSHA256: hex.EncodeToString(sum[:])})
	writeTestMedia(t, library, m.thumbnailPath(), []byte("old tile thumbnail"))
	if err := library.store.saveMedia(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if _, err := library.store.setDiveCover(context.Background(), 42, &m.ID); err != nil {
		t.Fatal(err)
	}
	return m
}

func assertHEIFRepair(t *testing.T, library *mediaLibrary, previous mediaItem) {
	t.Helper()
	m, err := library.store.mediaByID(context.Background(), 42, previous.ID)
	if err != nil || m.Width != 1080 || m.Height != 1620 || m.ThumbnailStatus != "ready" || m.photoState().HEIFDecoderVersion != 1 || m.Description != previous.Description || !m.Favorite {
		t.Fatalf("HEIC repair lost complete image or metadata: %+v %v", m, err)
	}
	if m.Filename != previous.Filename {
		t.Fatal("HEIC repair changed retained filename")
	}
	dive, err := library.store.Dive(context.Background(), 42)
	if err != nil || dive.Dive.CoverMediaID == nil || *dive.Dive.CoverMediaID != previous.ID {
		t.Fatal("HEIC repair lost cover")
	}
	if _, err := library.root.Stat("42/Grid.HEIC"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("source remained after verified repair")
	}
}

func TestHEIFLegacyRepairByScanOrUpload(t *testing.T) {
	body := heifFixture(t, "grid-rotated.heic")
	for _, upload := range []bool{false, true} {
		name := "scan"
		if upload {
			name = "upload"
		}
		t.Run(name, func(t *testing.T) {
			library, _ := mediaTestLibrary(t)
			old := legacyHEIFMedia(t, library, body)
			library.processor = mediaproc.NewProcessor()
			result, err := library.Scan(context.Background(), false)
			if err != nil || result.Failed != 1 {
				t.Fatalf("legacy HEIC detection: %+v %v", result, err)
			}
			missing, err := library.store.mediaByID(context.Background(), 42, old.ID)
			if err != nil || missing.ThumbnailStatus != "error" || !strings.Contains(missing.ThumbnailError, "restore Grid.HEIC") {
				t.Fatal("missing original not exposed to administrator")
			}
			if kept, err := library.root.ReadFile("42/Grid.HEIC.jxl"); err != nil || string(kept) != "legacy HEIC tile bytes" {
				t.Fatal("legacy retained file was overwritten without original")
			}
			if upload {
				m, err := library.Upload(context.Background(), 42, "Grid.HEIC", bytes.NewReader(body), nil)
				if err != nil || m.ID != old.ID {
					t.Fatalf("repair upload: %+v %v", m, err)
				}
			} else {
				writeTestMedia(t, library, "42/Grid.HEIC", body)
				result, err = library.Scan(context.Background(), false)
				if err != nil || result.Failed != 0 {
					t.Fatalf("repair scan: %+v %v", result, err)
				}
			}
			assertHEIFRepair(t, library, old)
			if _, err := library.Upload(context.Background(), 42, "Grid.HEIC", bytes.NewReader(body), nil); !errors.Is(err, errMediaConflict) {
				t.Fatal("completed HEIC repair must restore normal duplicate protection")
			}
		})
	}
}

func TestHEIFLegacyJournalKeepsOriginalForCorrectedDecoder(t *testing.T) {
	body := heifFixture(t, "grid-rotated.heic")
	library, _ := mediaTestLibrary(t)
	m := legacyHEIFMedia(t, library, body)
	writeTestMedia(t, library, "42/Grid.HEIC", body)
	job := photoJob{ID: m.ID, Owner: m.owner(), Source: "42/Grid.HEIC", SourceSHA: m.photoState().SourceSHA256, Target: "42/Grid.HEIC.jxl", TargetSHA: m.SHA256, Thumbnail: m.ThumbnailKey}
	if err := library.savePhotoJob(job); err != nil {
		t.Fatal(err)
	}
	if err := library.recoverPhotoJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if kept, err := library.root.ReadFile("42/Grid.HEIC"); err != nil || !bytes.Equal(kept, body) {
		t.Fatal("legacy cleanup deleted the HEIC needed for repair")
	}
	if _, err := library.root.Stat(job.path()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("legacy journal still blocks corrected publication")
	}
	library.processor = mediaproc.NewProcessor()
	if result, err := library.Scan(context.Background(), false); err != nil || result.Failed != 0 {
		t.Fatalf("repair after interrupted legacy cleanup: %+v %v", result, err)
	}
	assertHEIFRepair(t, library, m)
}

func TestHEIFSmallPhotoRebuildsLegacyThumbnailAndFallback(t *testing.T) {
	body := heifFixture(t, "grid-small.heic")
	library, _ := mediaTestLibrary(t)
	m, err := library.Upload(context.Background(), 42, "Small.HEIC", bytes.NewReader(body), nil)
	if err != nil {
		t.Fatal(err)
	}
	m.setPhotoState(photoState{Version: photoProfileVersion})
	m.ThumbnailKey = m.thumbnailKey()
	writeTestMedia(t, library, m.thumbnailPath(), []byte("old tile thumbnail"))
	writeTestMedia(t, library, ".fallbacks/"+photoFallbackKey(m.SHA256)+".jpg", []byte("old cached tile JPEG"))
	if err := library.store.saveMedia(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	library.processor = mediaproc.NewProcessor()
	if result, err := library.Scan(context.Background(), false); err != nil || result.Failed != 0 {
		t.Fatalf("small HEIC repair: %+v %v", result, err)
	}
	updated, err := library.store.mediaByID(context.Background(), 42, m.ID)
	if err != nil || updated.Width != 480 || updated.Height != 640 || updated.ThumbnailKey == m.ThumbnailKey || updated.photoState().HEIFDecoderVersion != 1 {
		t.Fatalf("small HEIC: %+v %v", updated, err)
	}
	if actual, err := library.root.ReadFile("42/Small.HEIC"); err != nil || !bytes.Equal(actual, body) {
		t.Fatal("small HEIC source bytes changed")
	}
	handler := newAPIHandler(library.store, testLogger(t))
	for _, url := range []string{updated.URL, updated.ThumbnailURL} {
		r := httptest.NewRequest("GET", url, nil)
		r.Header.Set("Accept", "image/jpeg")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("HEIC JPEG response: %d %s", w.Code, w.Body.String())
		}
		photo, err := jpeg.Decode(bytes.NewReader(w.Body.Bytes()))
		if err != nil || photo.Bounds().Dx() != 480 || photo.Bounds().Dy() != 640 {
			t.Fatalf("stale/cropped HEIC JPEG: %v %v", photo, err)
		}
	}
}

func TestHEIFMultipleTopLevelImagesRetainOriginal(t *testing.T) {
	body := heifFixture(t, "multiple.heic")
	library, _ := mediaTestLibrary(t)
	library.processor = mediaproc.NewProcessor()
	m, err := library.Upload(context.Background(), 42, "Multiple.HEIC", bytes.NewReader(body), nil)
	if err != nil || m.ThumbnailStatus != "error" || !strings.Contains(m.ThumbnailError, "multiple top-level images") {
		t.Fatalf("multiple-image HEIF handling: %+v %v", m, err)
	}
	if actual, err := library.root.ReadFile("42/Multiple.HEIC"); err != nil || !bytes.Equal(actual, body) {
		t.Fatal("ambiguous HEIF was deleted")
	}
	if _, err := library.root.Stat("42/Multiple.HEIC.jxl"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("arbitrary HEIF image published")
	}
}
