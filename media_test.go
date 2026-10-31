package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	charmLog "github.com/charmbracelet/log"
)

type fakeMediaProcessor struct {
	inspects   atomic.Int32
	thumbs     atomic.Int32
	fail       bool
	layout     string
	projection string
	probe      json.RawMessage
}

func (p *fakeMediaProcessor) Inspect(context.Context, *os.File) (mediaProbe, error) {
	p.inspects.Add(1)
	if p.fail {
		return mediaProbe{}, errors.New("unsupported test codec")
	}
	if p.probe != nil {
		return decodeMediaProbe(p.probe)
	}
	return mediaProbe{Width: 1280, Height: 720, Duration: 12.5, JSON: json.RawMessage(`{"streams":[{"codec_name":"hevc"}]}`)}, nil
}

func (p *fakeMediaProcessor) Thumbnail(_ context.Context, _, out *os.File, options mediaThumbnailOptions) error {
	p.thumbs.Add(1)
	p.layout, p.projection = options.StereoLayout, options.Projection
	return jpeg.Encode(out, image.NewRGBA(image.Rect(0, 0, 64, 36)), nil)
}

func mediaTestLibrary(t *testing.T) (*mediaLibrary, *fakeMediaProcessor) {
	t.Helper()
	store, _ := openTestSQLStore(t)
	if err := store.SaveDives(context.Background(), []DiveRecord{testSQLDive(t, 42), testSQLDive(t, 99)}); err != nil {
		t.Fatal(err)
	}
	library, err := openMediaLibrary(store, t.TempDir(), 1<<20, testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = library.Close() })
	fake := &fakeMediaProcessor{}
	library.processor = fake
	store.media = library
	return library, fake
}

func writeTestMedia(t *testing.T, library *mediaLibrary, path string, body []byte) {
	t.Helper()
	if err := library.root.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := library.root.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMediaScanReconcilesFilesystemAndRetainsMetadata(t *testing.T) {
	l, p := mediaTestLibrary(t)
	ctx := context.Background()
	writeTestMedia(t, l, "42/Coral original.JPG", []byte("original photo"))
	writeTestMedia(t, l, "42/Panorama.mov", []byte("360 video"))
	writeTestMedia(t, l, "42/notes.txt", []byte("ignored"))
	writeTestMedia(t, l, "123/unknown.jpg", []byte("later dive"))
	writeTestMedia(t, l, "42/nested/ignored.jpg", []byte("ignored"))
	r, err := l.Scan(ctx, false)
	if err != nil || r.Added != 2 || r.Failed != 0 || r.Skipped != 3 || p.inspects.Load() != 2 {
		t.Fatalf("first scan = %+v, err=%v, probes=%d", r, err, p.inspects.Load())
	}
	m, err := l.store.mediaByFilename(ctx, 42, "Panorama.mov")
	if err != nil {
		t.Fatal(err)
	}
	caption, layout, projection := "Shark dive", "mono", "360"
	m, err = l.Patch(ctx, 42, m.ID, mediaPatch{Caption: &caption, StereoLayout: &layout, Projection: &projection})
	if err != nil || p.layout != "mono" || p.projection != "360" || m.ThumbnailStatus != "ready" {
		t.Fatalf("patch = %+v, %v", m, err)
	}
	previous := p.inspects.Load()
	r, err = l.Scan(ctx, false)
	if err != nil || r.Unchanged != 2 || p.inspects.Load() != previous {
		t.Fatalf("unchanged scan = %+v, %v", r, err)
	}
	writeTestMedia(t, l, "42/Panorama.mov", []byte("changed 360 video"))
	r, err = l.Scan(ctx, false)
	updated, lookupErr := l.store.mediaByID(ctx, 42, m.ID)
	if err != nil || lookupErr != nil || r.Updated != 1 || updated.SHA256 == m.SHA256 || updated.Caption != caption || updated.StereoLayout != layout || updated.Projection != projection {
		t.Fatalf("modified scan = %+v, media=%+v, err=%v/%v", r, updated, err, lookupErr)
	}
	if _, err := l.root.Stat(".thumbnails/" + m.ThumbnailKey + ".jpg"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old thumbnail still exists: %v", err)
	}
	l.removeThumbnail(updated)
	r, err = l.Scan(ctx, false)
	if err != nil || r.Updated != 1 {
		t.Fatalf("missing thumbnail rebuild = %+v, %v", r, err)
	}
	if err := l.root.Remove("42/Coral original.JPG"); err != nil {
		t.Fatal(err)
	}
	if err := l.store.SaveDives(ctx, []DiveRecord{testSQLDive(t, 123)}); err != nil {
		t.Fatal(err)
	}
	r, err = l.Scan(ctx, false)
	if err != nil || r.Removed != 1 || r.Added != 1 {
		t.Fatalf("remove/link unknown dive = %+v, %v", r, err)
	}
	// The media FK uses the activity ID (dives.id), not garmin_dive_id.
	items, err := l.store.mediaForDive(ctx, 123)
	if err != nil || len(items) != 1 || items[0].Filename != "unknown.jpg" {
		t.Fatalf("deferred media = %+v, %v", items, err)
	}
	if err := l.root.RemoveAll("123"); err != nil {
		t.Fatal(err)
	}
	r, err = l.Scan(ctx, false)
	if err != nil || r.Removed != 1 {
		t.Fatalf("removed directory = %+v, %v", r, err)
	}
}

func TestMediaScanForceErrorsAndSymlinks(t *testing.T) {
	l, p := mediaTestLibrary(t)
	ctx := context.Background()
	writeTestMedia(t, l, "42/photo.jpg", []byte("one"))
	if _, err := l.Scan(ctx, false); err != nil {
		t.Fatal(err)
	}
	m, _ := l.store.mediaByFilename(ctx, 42, "photo.jpg")
	stamp := time.Unix(0, m.ModifiedNS)
	writeTestMedia(t, l, "42/photo.jpg", []byte("two"))
	if err := l.root.Chtimes("42/photo.jpg", stamp, stamp); err != nil {
		t.Fatal(err)
	}
	r, err := l.Scan(ctx, true)
	changed, _ := l.store.mediaByFilename(ctx, 42, "photo.jpg")
	if err != nil || r.Updated != 1 || changed.SHA256 == m.SHA256 {
		t.Fatalf("force scan = %+v, %v", r, err)
	}
	p.fail = true
	writeTestMedia(t, l, "42/unsupported.mov", []byte("future video codec"))
	r, err = l.Scan(ctx, false)
	broken, _ := l.store.mediaByFilename(ctx, 42, "unsupported.mov")
	if err != nil || r.Failed != 1 || broken.ThumbnailStatus != "error" || broken.ThumbnailURL != "" || broken.ThumbnailError == "" {
		t.Fatalf("failed thumbnail = %+v / %+v, %v", r, broken, err)
	}
	p.fail = false
	r, err = l.Scan(ctx, false)
	fixed, _ := l.store.mediaByID(ctx, 42, broken.ID)
	if err != nil || r.Updated != 1 || fixed.ThumbnailStatus != "ready" || fixed.ThumbnailError != "" {
		t.Fatalf("retry = %+v / %+v, %v", r, fixed, err)
	}
	outside := filepath.Join(t.TempDir(), "private.jpg")
	if err := os.WriteFile(outside, []byte("private bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := l.root.Symlink(outside, "42/escape.jpg"); err != nil {
		t.Fatal(err)
	}
	if err := l.root.Symlink(filepath.Dir(outside), "99"); err != nil {
		t.Fatal(err)
	}
	r, err = l.Scan(ctx, false)
	if err != nil || r.Skipped < 2 {
		t.Fatalf("symlink scan = %+v, %v", r, err)
	}
	if _, err := l.store.mediaByFilename(ctx, 42, "escape.jpg"); !errors.Is(err, errMediaNotFound) {
		t.Fatalf("symlink was indexed: %v", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := l.Scan(ctx, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled scan error = %v", err)
	}
}

func mediaRequest(t *testing.T, handler http.Handler, method, path string, body io.Reader, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, body)
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func uploadRequest(t *testing.T, handler http.Handler, name string, body []byte, more bool) *httptest.ResponseRecorder {
	t.Helper()
	var buffer bytes.Buffer
	w := multipart.NewWriter(&buffer)
	part, err := w.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(body); err != nil {
		t.Fatal(err)
	}
	if more {
		part, err = w.CreateFormFile("file", "second.jpg")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = part.Write(body)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return mediaRequest(t, handler, "POST", "/api/v1/dives/42/media", &buffer, w.FormDataContentType())
}

func decodeMediaResponse(t *testing.T, w *httptest.ResponseRecorder, status int) mediaItem {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status=%d, want=%d: %s", w.Code, status, w.Body)
	}
	var m mediaItem
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMediaAPIUploadServeRangesMetadataAndDelete(t *testing.T) {
	l, _ := mediaTestLibrary(t)
	handler := newAPIHandler(l.store, testLogger(t))
	body := []byte("exact original 360 video bytes")
	name := "Requin été #1.MOV"
	w := uploadRequest(t, handler, name, body, false)
	m := decodeMediaResponse(t, w, http.StatusCreated)
	checksum := sha256.Sum256(body)
	if m.Filename != name || m.Kind != "video" || m.SHA256 != hex.EncodeToString(checksum[:]) || m.ThumbnailURL == "" || w.Header().Get("Location") == "" {
		t.Fatalf("uploaded media = %+v", m)
	}
	saved, err := l.root.ReadFile("42/" + name)
	if err != nil || !bytes.Equal(saved, body) {
		t.Fatalf("original changed: %s, %v", saved, err)
	}
	base := "/api/v1/dives/42/media/" + m.ID
	w = mediaRequest(t, handler, "GET", m.URL, nil, "")
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), body) || w.Header().Get("Accept-Ranges") != "bytes" || w.Header().Get("Content-Type") != "video/quicktime" {
		t.Fatalf("original response = %d %v %q", w.Code, w.Header(), w.Body)
	}
	r := httptest.NewRequest("GET", m.URL, nil)
	r.Header.Set("Range", "bytes=6-13")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusPartialContent || w.Body.String() != string(body[6:14]) || w.Header().Get("Content-Range") == "" {
		t.Fatalf("range = %d %q", w.Code, w.Body)
	}
	r = httptest.NewRequest("GET", m.URL, nil)
	r.Header.Set("If-None-Match", `"`+m.SHA256+`"`)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusNotModified || w.Body.Len() != 0 {
		t.Fatalf("conditional GET = %d %q", w.Code, w.Body)
	}
	w = mediaRequest(t, handler, "HEAD", m.URL, nil, "")
	if w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Length") != strconv.Itoa(len(body)) {
		t.Fatalf("HEAD = %d %v %q", w.Code, w.Header(), w.Body)
	}
	w = mediaRequest(t, handler, "GET", m.ThumbnailURL, nil, "")
	if _, err := jpeg.Decode(w.Body); w.Code != 200 || err != nil {
		t.Fatalf("thumbnail response = %d, %v", w.Code, err)
	}
	w = mediaRequest(t, handler, "PATCH", base, strings.NewReader(`{"caption":"Bubbles","projection":"360"}`), "application/json")
	patched := decodeMediaResponse(t, w, 200)
	if patched.Caption != "Bubbles" || patched.Projection != "360" || patched.EffectiveProjection != "360" || patched.ProjectionFormat != "equirectangular" || patched.ThumbnailURL == m.ThumbnailURL {
		t.Fatalf("patched media = %+v", patched)
	}
	w = mediaRequest(t, handler, "GET", "/api/v1/dives/42", nil, "")
	var dive diveDetail
	if err := json.Unmarshal(w.Body.Bytes(), &dive); err != nil || len(dive.Media) != 1 || dive.Media[0].ID != m.ID {
		t.Fatalf("dive media = %+v, %v", dive.Media, err)
	}
	if w := uploadRequest(t, handler, name, []byte("replacement"), false); w.Code != 409 {
		t.Fatalf("duplicate upload = %d %s", w.Code, w.Body)
	}
	w = mediaRequest(t, handler, "GET", "/api/v1/dives/99/media/"+m.ID, nil, "")
	if w.Code != 404 {
		t.Fatalf("cross-dive access = %d", w.Code)
	}
	w = mediaRequest(t, handler, "DELETE", base, nil, "")
	if w.Code != 204 {
		t.Fatalf("delete = %d %s", w.Code, w.Body)
	}
	if _, err := l.root.Stat("42/" + name); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted original retained: %v", err)
	}
	files, err := os.ReadDir(filepath.Join(l.directory, ".thumbnails"))
	if err != nil || len(files) != 0 {
		t.Fatalf("deleted thumbnail retained: %v, %v", files, err)
	}
	w = mediaRequest(t, handler, "GET", m.URL, nil, "")
	if w.Code != 404 {
		t.Fatalf("deleted GET = %d", w.Code)
	}
}

func TestMediaSurvivesRestartAndDoesNotReprocessUnchangedFiles(t *testing.T) {
	l, _ := mediaTestLibrary(t)
	ctx := context.Background()
	m, err := l.Upload(ctx, 42, "Original.MOV", strings.NewReader("exact original bytes"), nil)
	if err != nil {
		t.Fatal(err)
	}
	caption := "Keep this caption"
	m, err = l.Patch(ctx, 42, m.ID, mediaPatch{Caption: &caption})
	if err != nil {
		t.Fatal(err)
	}
	path, directory := l.store.path, l.directory
	_ = l.Close()
	_ = l.store.Close()
	reopened, err := openSQLStore(ctx, outputSQLite, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	next, err := openMediaLibrary(reopened, directory, defaultMediaMaxUpload, testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	fake := &fakeMediaProcessor{}
	next.processor = fake
	r, err := next.Scan(ctx, false)
	saved, lookupErr := reopened.mediaByID(ctx, 42, m.ID)
	if err != nil || lookupErr != nil || r.Unchanged != 1 || fake.inspects.Load() != 0 || saved.Caption != caption || saved.SHA256 != m.SHA256 || saved.ThumbnailKey != m.ThumbnailKey {
		t.Fatalf("restart scan = %+v, saved=%+v, errors=%v/%v", r, saved, err, lookupErr)
	}
	// Removing a dive cascades the metadata. Originals stay on disk, allowing a
	// later reimport of that activity to discover them again.
	if _, err := reopened.db.Exec(`DELETE FROM dives WHERE id = 42`); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.mediaByID(ctx, 42, m.ID); !errors.Is(err, errMediaNotFound) {
		t.Fatalf("media foreign key did not cascade: %v", err)
	}
}

func TestMediaAPIErrorsRetainOriginalAndBlockSymlinkServing(t *testing.T) {
	l, processor := mediaTestLibrary(t)
	processor.fail = true
	h := newAPIHandler(l.store, testLogger(t))
	m := decodeMediaResponse(t, uploadRequest(t, h, "future.mov", []byte("future codec bytes"), false), 201)
	if m.ThumbnailStatus != "error" || m.ThumbnailURL != "" {
		t.Fatalf("failed thumbnail metadata = %+v", m)
	}
	w := mediaRequest(t, h, "GET", m.URL, nil, "")
	if w.Code != 200 || w.Body.String() != "future codec bytes" {
		t.Fatalf("original after failed thumbnail = %d %s", w.Code, w.Body)
	}
	w = mediaRequest(t, h, "GET", "/api/v1/dives/42/media/"+m.ID+"/thumbnail", nil, "")
	if w.Code != 404 || !strings.Contains(w.Body.String(), "thumbnail_unavailable") {
		t.Fatalf("failed thumbnail = %d %s", w.Code, w.Body)
	}
	outside := filepath.Join(t.TempDir(), "secret.mov")
	if err := os.WriteFile(outside, []byte("private data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := l.root.Remove("42/future.mov"); err != nil {
		t.Fatal(err)
	}
	if err := l.root.Symlink(outside, "42/future.mov"); err != nil {
		t.Fatal(err)
	}
	w = mediaRequest(t, h, "GET", m.URL, nil, "")
	if w.Code == 200 || strings.Contains(w.Body.String(), "private data") {
		t.Fatalf("served outside media root: %d %s", w.Code, w.Body)
	}
	w = mediaRequest(t, h, "POST", "/api/v1/media/scan?force=wrong", nil, "")
	if w.Code != 400 {
		t.Fatalf("invalid force accepted: %d", w.Code)
	}
	badBody := "--boundary\r\nContent-Disposition: form-data; name=\"file\"; filename=\"truncated.jpg\"\r\n\r\npartial"
	w = mediaRequest(t, h, "POST", "/api/v1/dives/42/media", strings.NewReader(badBody), "multipart/form-data; boundary=boundary")
	if w.Code != 400 {
		t.Fatalf("truncated multipart = %d %s", w.Code, w.Body)
	}
	if _, err := l.root.Stat("42/truncated.jpg"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial upload was published: %v", err)
	}
}

func TestMediaAPIValidationPaginationAndRescan(t *testing.T) {
	l, _ := mediaTestLibrary(t)
	l.maxUpload = 100
	h := newAPIHandler(l.store, testLogger(t))
	for _, test := range []struct {
		name string
		body []byte
		more bool
		code int
	}{
		{"../escape.jpg", []byte("image"), false, 400},
		{"a\\b.jpg", []byte("image"), false, 400},
		{".hidden.jpg", []byte("image"), false, 400},
		{"file.html", []byte("html"), false, 415},
		{"empty.jpg", nil, false, 400},
		{"big.mp4", bytes.Repeat([]byte("x"), 101), false, 413},
		{"multiple.jpg", []byte("image"), true, 400},
	} {
		w := uploadRequest(t, h, test.name, test.body, test.more)
		if w.Code != test.code {
			t.Errorf("upload %q = %d, want %d: %s", test.name, w.Code, test.code, w.Body)
		}
	}
	items, _ := l.store.mediaForDive(context.Background(), 42)
	if len(items) != 0 {
		t.Fatalf("invalid uploads published: %+v", items)
	}
	for _, name := range []string{"c.mov", "a.jpg", "b.jpeg"} {
		decodeMediaResponse(t, uploadRequest(t, h, name, []byte(name), false), 201)
	}
	w := mediaRequest(t, h, "GET", "/api/v1/dives/42/media?limit=2", nil, "")
	var first mediaListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &first); err != nil || len(first.Items) != 2 || first.Items[0].Filename != "a.jpg" || first.NextCursor == "" {
		t.Fatalf("first media page = %s, %v", w.Body, err)
	}
	w = mediaRequest(t, h, "GET", "/api/v1/dives/42/media?limit=2&cursor="+first.NextCursor, nil, "")
	var second mediaListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &second); err != nil || len(second.Items) != 1 || second.Items[0].Filename != "c.mov" || second.NextCursor != "" {
		t.Fatalf("second media page = %s, %v", w.Body, err)
	}
	writeTestMedia(t, l, "42/direct copy.mp4", []byte("filesystem video"))
	w = mediaRequest(t, h, "POST", "/api/v1/media/scan", nil, "")
	var scan mediaScanResult
	if err := json.Unmarshal(w.Body.Bytes(), &scan); err != nil || scan.Added != 1 || scan.Unchanged != 3 {
		t.Fatalf("API scan = %s, %v", w.Body, err)
	}
	w = mediaRequest(t, h, "GET", "/api/v1/media/status", nil, "")
	var status map[string]int64
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil || status["total"] != 4 || status["thumbnails_ready"] != 4 || status["max_upload_bytes"] != 100 {
		t.Fatalf("status = %s, %v", w.Body, err)
	}
	for path, code := range map[string]int{
		"/api/v1/dives/999/media":              404,
		"/api/v1/dives/42/media?limit=0":       400,
		"/api/v1/dives/42/media?cursor=broken": 400,
		"/assets/42/":                          404,
		"/assets/42/.thumbnails":               404,
		"/assets/42/nope.jpg":                  404,
	} {
		w := mediaRequest(t, h, "GET", path, nil, "")
		if w.Code != code {
			t.Errorf("GET %s = %d %s", path, w.Code, w.Body)
		}
	}
	base := "/api/v1/dives/42/media/" + first.Items[0].ID
	for _, body := range []string{`{}`, `null`, `{"filename":"rename.jpg"}`, `{"stereo_layout":"wrong"}`, `{"projection":"wrong"}`, `{} {}`} {
		w := mediaRequest(t, h, "PATCH", base, strings.NewReader(body), "application/json")
		if w.Code != 400 {
			t.Errorf("PATCH %s = %d %s", body, w.Code, w.Body)
		}
	}
	writeTestMedia(t, l, "42/a.jpg", []byte("changed file"))
	if w := mediaRequest(t, h, "GET", first.Items[0].URL, nil, ""); w.Code != 409 {
		t.Fatalf("changed original served with stale validator: %d", w.Code)
	}
}

func TestMediaConcurrentUploadNeverOverwrites(t *testing.T) {
	l, _ := mediaTestLibrary(t)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, body := range []string{"first", "second"} {
		wg.Add(1)
		go func(body string) {
			defer wg.Done()
			_, err := l.Upload(context.Background(), 42, "same.jpg", strings.NewReader(body), nil)
			results <- err
		}(body)
	}
	wg.Wait()
	close(results)
	var success, conflicts int
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, errMediaConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatalf("success=%d, conflicts=%d", success, conflicts)
	}
	entries, _ := os.ReadDir(filepath.Join(l.directory, ".uploads"))
	if len(entries) != 0 {
		t.Fatalf("staged uploads leaked: %+v", entries)
	}
}

func TestMediaFFmpegPhotoVideoAndStereoThumbnails(t *testing.T) {
	processor := newMediaProcessor()
	if processor.ffmpeg == "" || processor.ffprobe == "" {
		t.Skip("install FFmpeg for real photo/video thumbnail tests")
	}
	l, _ := mediaTestLibrary(t)
	l.processor = processor
	photo := image.NewRGBA(image.Rect(0, 0, 800, 400))
	for y := 0; y < 400; y++ {
		for x := 0; x < 800; x++ {
			c := color.RGBA{R: 240, A: 255}
			if x >= 400 {
				c = color.RGBA{B: 240, A: 255}
			}
			photo.SetRGBA(x, y, c)
		}
	}
	var jpegBytes bytes.Buffer
	if err := jpeg.Encode(&jpegBytes, photo, nil); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	m, err := l.Upload(ctx, 42, "Stereo photo.jpg", bytes.NewReader(jpegBytes.Bytes()), nil)
	if err != nil || m.ThumbnailStatus != "ready" || m.Width != 800 || m.Height != 400 {
		t.Fatalf("real photo = %+v, %v", m, err)
	}
	layout := "side_by_side"
	m, err = l.Patch(ctx, 42, m.ID, mediaPatch{StereoLayout: &layout})
	if err != nil || m.ThumbnailStatus != "ready" {
		t.Fatalf("stereo thumbnail = %+v, %v", m, err)
	}
	thumb, err := l.root.ReadFile(".thumbnails/" + m.ThumbnailKey + ".jpg")
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := jpeg.Decode(bytes.NewReader(thumb))
	if err != nil || decoded.Bounds().Dx() != 640 || decoded.Bounds().Dy() != 640 {
		t.Fatalf("single-eye dimensions = %v, %v", decoded, err)
	}
	r, _, b, _ := decoded.At(320, 320).RGBA()
	if r <= b {
		t.Fatal("stereo thumbnail did not select the left eye")
	}
	// The MP4 deliberately keeps moov at the end: the descriptor must be seekable.
	videoPath := filepath.Join(t.TempDir(), "360 sample.mp4")
	cmd := exec.Command(processor.ffmpeg, "-v", "error", "-f", "lavfi", "-i", "color=c=blue:s=128x64:r=10:d=0.2",
		"-c:v", "mpeg4", "-y", videoPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create test video: %v: %s", err, output)
	}
	video, err := os.Open(videoPath)
	if err != nil {
		t.Fatal(err)
	}
	defer video.Close()
	m, err = l.Upload(ctx, 42, "360 sample.mp4", video, nil)
	if err != nil || m.ThumbnailStatus != "ready" || m.Width != 128 || m.DurationSeconds <= 0 {
		t.Fatalf("real short video = %+v, %v", m, err)
	}
	projection := "360"
	m, err = l.Patch(ctx, 42, m.ID, mediaPatch{Projection: &projection})
	if err != nil || m.ThumbnailStatus != "ready" || m.EffectiveProjection != "360" || m.ProjectionFormat != "equirectangular" {
		t.Fatalf("360 video perspective thumbnail = %+v, %v", m, err)
	}
	layout = "top_bottom"
	m, err = l.Patch(ctx, 42, m.ID, mediaPatch{StereoLayout: &layout})
	if err != nil || m.ThumbnailStatus != "ready" {
		t.Fatalf("top/bottom video = %+v, %v", m, err)
	}
}

func TestRunServeScansAssetsBeforeListening(t *testing.T) {
	store, _ := openTestSQLStore(t)
	if err := store.SaveDives(context.Background(), []DiveRecord{testSQLDive(t, 42)}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "42"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "42", "copied.mov"), []byte("unknown future video codec"), 0o644); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger, _ := newAppLogger(&logs, "info")
	err := runServeWithDependencies(context.Background(), []string{"-assets-dir", dir, "-media-max-upload-bytes", "123456"}, io.Discard, logger,
		func(context.Context, string, string) (*sqlStore, error) { return store, nil },
		func(ctx context.Context, _ string, got *sqlStore, _ *charmLog.Logger) error {
			items, err := got.mediaForDive(ctx, 42)
			if err != nil || len(items) != 1 || items[0].Filename != "copied.mov" || got.media.maxUpload != 123456 {
				t.Fatalf("startup media = %+v, %v", items, err)
			}
			return nil
		})
	if err != nil || !strings.Contains(logs.String(), "scanning media assets") || !strings.Contains(logs.String(), "media scan complete") || !strings.Contains(logs.String(), "added=1") {
		t.Fatalf("startup = %v, logs=%s", err, logs.String())
	}
}
