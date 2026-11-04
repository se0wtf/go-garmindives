package app

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io/fs"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	mediaproc "github.com/se0wtf/go-garmindives/internal/media"
)

func photoTestTools(t *testing.T) mediaproc.Processor {
	t.Helper()
	for _, name := range []string{"ffmpeg", "ffprobe", "cjxl", "djxl"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skip("install FFmpeg and JPEG XL tools for photo integration tests")
		}
	}
	return mediaproc.NewProcessor()
}

func photoPNG(t *testing.T, width, height int, noise bool) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	state := uint32(123456789)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			c := color.RGBA{R: uint8(x % 255), G: uint8(y % 255), B: 110, A: 255}
			if noise {
				state = 1664525*state + 1013904223
				c.R = uint8(state >> 24)
				state = 1664525*state + 1013904223
				c.G = uint8(state >> 24)
				state = 1664525*state + 1013904223
				c.B = uint8(state >> 24)
			}
			img.SetRGBA(x, y, c)
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestPhotoUploadThresholdsAndRescans(t *testing.T) {
	processor := photoTestTools(t)
	for _, test := range []struct {
		name             string
		width, height    int
		noise, converted bool
		side             int
	}{
		{"small", 800, 600, false, false, 600},
		{"resolution-boundary", 2048, 2048, false, true, 1080},
		{"large-bytes-small-resolution", 800, 600, true, true, 600},
	} {
		t.Run(test.name, func(t *testing.T) {
			library, _ := mediaTestLibrary(t)
			library.maxUpload = 10 << 20
			library.processor = processor
			body := photoPNG(t, test.width, test.height, test.noise)
			if test.noise && int64(len(body)) < photoKeepBytes {
				t.Fatal("fixture must exceed byte threshold")
			}
			m, err := library.Upload(context.Background(), 42, "Reef.png", bytes.NewReader(body), nil)
			if err != nil || m.ThumbnailStatus != "ready" || min(m.Width, m.Height) != test.side {
				t.Fatalf("photo: %+v %v", m, err)
			}
			if test.converted {
				if m.Filename != "Reef.png.jxl" || m.ContentType != "image/jxl" {
					t.Fatalf("wrong conversion: %+v", m)
				}
				if _, err := library.root.Stat("42/Reef.png"); !errors.Is(err, fs.ErrNotExist) {
					t.Fatal("source remained after verified conversion")
				}
			} else {
				kept, err := library.root.ReadFile("42/Reef.png")
				if err != nil || !bytes.Equal(kept, body) {
					t.Fatal("small photo bytes changed")
				}
			}
			thumb, err := library.root.Open(m.thumbnailPath())
			if err != nil {
				t.Fatal(err)
			}
			probe, err := processor.Inspect(context.Background(), thumb)
			thumb.Close()
			if err != nil || min(probe.Width, probe.Height) != 480 {
				t.Fatalf("wrong photo thumbnail: %+v %v", probe, err)
			}
			before, err := library.root.ReadFile(m.owner().path(m.Filename))
			if err != nil {
				t.Fatal(err)
			}
			for _, force := range []bool{false, true} {
				if _, err := library.Scan(context.Background(), force); err != nil {
					t.Fatal(err)
				}
				after, err := library.root.ReadFile(m.owner().path(m.Filename))
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal("rescan recompressed the retained image")
				}
				stored, err := library.store.ownedMediaByID(context.Background(), m.owner(), m.ID)
				if err != nil || stored.Filename != m.Filename {
					t.Fatal("rescan changed media identity")
				}
			}
			if _, err := library.Upload(context.Background(), 42, "Reef.png", bytes.NewReader(body), nil); !errors.Is(err, errMediaConflict) {
				t.Fatalf("original filename alias bypassed conflict detection: %v", err)
			}
		})
	}
}

func TestPhotoScanPreservesMetadataAndRollback(t *testing.T) {
	processor := photoTestTools(t)
	library, _ := mediaTestLibrary(t)
	body := photoPNG(t, 2048, 2048, false)
	m, err := library.Upload(context.Background(), 42, "Shark.png", bytes.NewReader(body), nil)
	if err != nil {
		t.Fatal(err)
	}
	description, favorite := "Un requin", true
	m, err = library.Patch(context.Background(), 42, m.ID, mediaPatch{Description: &description, Favorite: &favorite})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := library.store.setDiveCover(context.Background(), 42, &m.ID); err != nil {
		t.Fatal(err)
	}
	library.processor = processor
	if _, err := library.store.db.Exec(`CREATE TRIGGER reject_photo_rename BEFORE UPDATE OF filename ON dive_media WHEN NEW.filename LIKE '%.jxl' BEGIN SELECT RAISE(ABORT,'test SQL failure'); END`); err != nil {
		t.Fatal(err)
	}
	result, err := library.Scan(context.Background(), false)
	if err != nil || result.Failed != 1 {
		t.Fatalf("expected recoverable SQL failure: %+v %v", result, err)
	}
	kept, err := library.root.ReadFile("42/Shark.png")
	if err != nil || !bytes.Equal(kept, body) {
		t.Fatal("SQL failure lost source bytes")
	}
	if _, err := library.root.Stat("42/Shark.png.jxl"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("uncommitted converted file was not rolled back")
	}
	if _, err := library.root.Stat(m.thumbnailPath()); err != nil {
		t.Fatal("rollback removed existing thumbnail")
	}
	if _, err := library.store.db.Exec(`DROP TRIGGER reject_photo_rename`); err != nil {
		t.Fatal(err)
	}
	if _, err := library.Scan(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	stored, err := library.store.mediaByID(context.Background(), 42, m.ID)
	if err != nil || stored.ID != m.ID || stored.Description != description || !stored.Favorite || stored.Filename != "Shark.png.jxl" || stored.ThumbnailStatus != "ready" {
		t.Fatalf("conversion lost metadata: %+v %v", stored, err)
	}
	dive, err := library.store.Dive(context.Background(), 42)
	if err != nil || dive.Dive.CoverMediaID == nil || *dive.Dive.CoverMediaID != m.ID {
		t.Fatal("conversion lost cover selection")
	}
	if _, err := library.root.Stat("42/Shark.png"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("source not removed after committed conversion")
	}
}

func TestPhotoHTTPNegotiationAndCachedFallback(t *testing.T) {
	processor := photoTestTools(t)
	library, _ := mediaTestLibrary(t)
	library.processor = processor
	m, err := library.Upload(context.Background(), 42, "Reef.png", bytes.NewReader(photoPNG(t, 2048, 2048, false)), nil)
	if err != nil || m.ThumbnailStatus != "ready" {
		t.Fatalf("photo setup: %+v %v", m, err)
	}
	handler := newAPIHandler(library.store, testLogger(t))
	get := func(url, accept, etag, method string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, url, nil)
		r.Header.Set("Accept", accept)
		if etag != "" {
			r.Header.Set("If-None-Match", etag)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	native := get(m.URL, "image/jxl", "", "GET")
	if native.Code != 200 || native.Header().Get("Content-Type") != "image/jxl" || native.Header().Get("Vary") != "Accept" {
		t.Fatalf("native image: %d %v", native.Code, native.Header())
	}
	fallback := get(m.URL, "image/jxl;q=0,image/jpeg", "", "GET")
	if fallback.Code != 200 || fallback.Header().Get("Content-Type") != "image/jpeg" || fallback.Header().Get("ETag") == native.Header().Get("ETag") {
		t.Fatalf("JPEG fallback: %d %v", fallback.Code, fallback.Header())
	}
	decoded, err := jpeg.Decode(bytes.NewReader(fallback.Body.Bytes()))
	if err != nil || decoded.Bounds().Dx() != m.Width || decoded.Bounds().Dy() != m.Height {
		t.Fatal("fallback dimensions/decoding failed")
	}
	cache := ".fallbacks/" + photoFallbackKey(m.SHA256) + ".jpg"
	before, err := library.root.Stat(cache)
	if err != nil {
		t.Fatal(err)
	}
	notModified := get(m.URL, "image/jpeg", fallback.Header().Get("ETag"), "GET")
	if notModified.Code != 304 || notModified.Body.Len() != 0 {
		t.Fatal("fallback conditional caching failed")
	}
	after, err := library.root.Stat(cache)
	if err != nil || !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("fallback was re-encoded")
	}
	if response := get(m.URL, "image/jpeg", native.Header().Get("ETag"), "GET"); response.Code != 200 {
		t.Fatal("native ETag incorrectly matched JPEG variant")
	}
	if response := get(m.URL, "image/jpeg", "", "HEAD"); response.Code != 200 || response.Body.Len() != 0 {
		t.Fatal("fallback HEAD failed")
	}
	if response := get(m.URL+"?format=original", "image/jpeg", "", "GET"); response.Header().Get("Content-Type") != "image/jxl" {
		t.Fatal("explicit original format ignored")
	}
	if response := get(m.URL+"?format=jpeg", "image/jxl", "", "GET"); response.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatal("explicit JPEG format ignored")
	}
	if response := get(m.ThumbnailURL, "image/jxl", "", "GET"); response.Header().Get("Content-Type") != "image/jxl" {
		t.Fatal("native photo thumbnail not JXL")
	}
	if response := get(m.ThumbnailURL, "image/jpeg", "", "GET"); response.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatal("thumbnail fallback failed")
	}
	if err := library.Delete(context.Background(), 42, m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := library.root.Stat(cache); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("delete left cached fallback")
	}
}

func TestPhotoMissingEncoderKeepsSource(t *testing.T) {
	processor := photoTestTools(t)
	library, _ := mediaTestLibrary(t)
	library.processor = processor
	body := photoPNG(t, 2048, 2048, false)
	t.Setenv("PATH", "")
	m, err := library.Upload(context.Background(), 42, "Keep.png", bytes.NewReader(body), nil)
	if err != nil || m.ThumbnailStatus != "error" || !strings.Contains(m.ThumbnailError, "cjxl") {
		t.Fatalf("missing tool: %+v %v", m, err)
	}
	kept, err := os.ReadFile(library.directory + "/42/Keep.png")
	if err != nil || !bytes.Equal(kept, body) {
		t.Fatal("missing encoder deleted the source")
	}
}

type failPhotoPreview struct {
	mediaproc.Processor
	mediaproc.PhotoProcessor
}

func (p failPhotoPreview) Photo(ctx context.Context, input, output *os.File, options mediaproc.PhotoOptions) (mediaproc.Probe, error) {
	if options.Preview {
		return mediaproc.Probe{}, errors.New("test thumbnail encoding failure")
	}
	return p.PhotoProcessor.Photo(ctx, input, output, options)
}

func TestPhotoThumbnailFailureKeepsSource(t *testing.T) {
	processor := photoTestTools(t)
	library, _ := mediaTestLibrary(t)
	library.processor = failPhotoPreview{processor, processor.(mediaproc.PhotoProcessor)}
	body := photoPNG(t, 2048, 2048, false)
	m, err := library.Upload(context.Background(), 42, "Keep.png", bytes.NewReader(body), nil)
	if err != nil || m.Filename != "Keep.png" || m.ThumbnailStatus != "error" || !strings.Contains(m.ThumbnailError, "thumbnail encoding failure") {
		t.Fatalf("thumbnail failure: %+v %v", m, err)
	}
	kept, err := library.root.ReadFile("42/Keep.png")
	if err != nil || !bytes.Equal(kept, body) {
		t.Fatal("thumbnail failure lost the source")
	}
	if _, err := library.root.Stat("42/Keep.png.jxl"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("failed conversion was published")
	}
}

func TestPhotoThumbnailRepairKeepsProcessingMarker(t *testing.T) {
	processor := photoTestTools(t)
	library, _ := mediaTestLibrary(t)
	library.processor = processor
	m, err := library.Upload(context.Background(), 42, "Reef.png", bytes.NewReader(photoPNG(t, 2048, 2048, false)), nil)
	if err != nil {
		t.Fatal(err)
	}
	library.processor = failPhotoPreview{processor, processor.(mediaproc.PhotoProcessor)}
	result, err := library.Scan(context.Background(), true)
	if err != nil || result.Failed != 1 {
		t.Fatalf("forced thumbnail failure: %+v %v", result, err)
	}
	failed, err := library.store.mediaByID(context.Background(), 42, m.ID)
	if err != nil || failed.photoState().Version != photoProfileVersion || failed.SHA256 != m.SHA256 {
		t.Fatal("thumbnail failure discarded processing identity")
	}
	library.processor = processor
	result, err = library.Scan(context.Background(), false)
	if err != nil || result.Failed != 0 {
		t.Fatalf("thumbnail repair: %+v %v", result, err)
	}
	repaired, err := library.store.mediaByID(context.Background(), 42, m.ID)
	if err != nil || repaired.ThumbnailStatus != "ready" || repaired.SHA256 != m.SHA256 {
		t.Fatal("thumbnail repair changed retained photo")
	}
}

func TestPhotoReplacingConvertedSourceWithSmallPhoto(t *testing.T) {
	library, _ := mediaTestLibrary(t)
	library.processor = photoTestTools(t)
	m, err := library.Upload(context.Background(), 42, "Reef.png", bytes.NewReader(photoPNG(t, 2048, 2048, false)), nil)
	if err != nil {
		t.Fatal(err)
	}
	body := photoPNG(t, 800, 600, false)
	writeTestMedia(t, library, "42/Reef.png", body)
	result, err := library.Scan(context.Background(), false)
	if err != nil || result.Failed != 0 || result.Added != 0 {
		t.Fatalf("replace scan: %+v %v", result, err)
	}
	stored, err := library.store.mediaByID(context.Background(), 42, m.ID)
	if err != nil || stored.Filename != "Reef.png" || stored.ThumbnailStatus != "ready" {
		t.Fatalf("replacement identity: %+v %v", stored, err)
	}
	kept, err := library.root.ReadFile("42/Reef.png")
	if err != nil || !bytes.Equal(kept, body) {
		t.Fatal("small replacement changed")
	}
	if _, err := library.root.Stat("42/Reef.png.jxl"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("obsolete converted photo remained")
	}
	result, err = library.Scan(context.Background(), false)
	if err != nil || result.Added != 0 || result.Removed != 0 || result.Failed != 0 {
		t.Fatalf("replacement rescan: %+v %v", result, err)
	}
}

func exercisePhotoSQLConversion(t *testing.T, store *sqlStore) {
	t.Helper()
	processor := photoTestTools(t)
	library, err := openMediaLibrary(store, t.TempDir(), 10<<20, testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	defer library.Close()
	previous := store.media
	store.media, library.processor = library, processor
	defer func() { store.media = previous }()
	handler := newAPIHandler(store, testLogger(t))
	trip := travelRequest[diveTrip](t, handler, "POST", "/api/v1/trips", map[string]any{"year": 2026, "country_code": "EG"}, 201)
	for _, owner := range []mediaOwner{{DiveID: 42}, {TripID: trip.ID}} {
		// Simulate an existing unprocessed attachment being upgraded at startup.
		library.processor = &fakeMediaProcessor{}
		m, err := library.uploadOwned(context.Background(), owner, "Boat.png", bytes.NewReader(photoPNG(t, 2048, 2048, false)), nil)
		if err != nil {
			t.Fatal(err)
		}
		description := "Le bateau"
		m, err = library.patchOwned(context.Background(), owner, m.ID, mediaPatch{Description: &description})
		if err != nil {
			t.Fatal(err)
		}
		if owner.TripID != "" {
			travelRequest[diveTrip](t, handler, "PATCH", owner.apiPath(), map[string]any{"cover_media_id": m.ID}, 200)
		} else if _, err := store.setDiveCover(context.Background(), 42, &m.ID); err != nil {
			t.Fatal(err)
		}
		library.processor = processor
		if result, err := library.Scan(context.Background(), false); err != nil || result.Failed != 0 {
			t.Fatalf("SQL conversion: %+v %v", result, err)
		}
		stored, err := store.ownedMediaByID(context.Background(), owner, m.ID)
		if err != nil || stored.Filename != "Boat.png.jxl" || stored.Description != description || stored.ThumbnailStatus != "ready" {
			t.Fatalf("SQL metadata: %+v %v", stored, err)
		}
		if owner.TripID != "" {
			updated, err := store.trip(context.Background(), owner.TripID)
			if err != nil || updated.CoverMediaID == nil || *updated.CoverMediaID != m.ID {
				t.Fatal("trip cover changed during conversion")
			}
		}
	}
}

func TestPhotoDiveAndTripSQLConversion(t *testing.T) {
	store, _ := openTestSQLStore(t)
	if err := store.SaveDives(context.Background(), []DiveRecord{testSQLDive(t, 42)}); err != nil {
		t.Fatal(err)
	}
	exercisePhotoSQLConversion(t, store)
}
