package app

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"net/http/httptest"
	"testing"

	mediaproc "github.com/se0wtf/go-garmindives/internal/media"
)

func TestPendingVideoMatchingAndConversion(t *testing.T) {
	p, _, _, clock := pendingTestService(t)
	processor := &countedVideoProcessor{Processor: mediaproc.NewProcessor()}
	p.media.processor, p.files.processor = processor, processor
	body := appVideoFixture(t, "3840x2160", "libx264", "mp4")
	putPending(t, p, "GoPro.MP4", body)
	if result := settlePending(t, p, clock); result.Assigned != 1 || result.Failed != 0 {
		t.Fatalf("pending import: %+v", result)
	}
	items, err := p.store.mediaForDive(context.Background(), 42)
	if err != nil || len(items) != 1 || items[0].Width != 1920 || items[0].Height != 1080 || items[0].Kind != "video" || items[0].Capture == nil || items[0].Capture.CapturedAt != "2026-08-14T23:30:00Z" || !items[0].Capture.TimezoneAssumed {
		t.Fatalf("assigned video: %+v %v", items, err)
	}
	if _, err := p.files.root.Stat("GoPro.MP4"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("pending original remained")
	}
	if _, err := p.media.Scan(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	m, err := p.store.mediaByID(context.Background(), 42, items[0].ID)
	if err != nil || m.Capture == nil || processor.conversions != 1 {
		t.Fatal("rescan lost capture or recompressed video")
	}
}

func TestPendingVideoPrivatePreviewAndManualAssignment(t *testing.T) {
	p, _, reader, clock := pendingTestService(t)
	reader.capture = nil
	p.media.processor, p.files.processor = mediaproc.NewProcessor(), mediaproc.NewProcessor()
	body := appVideoFixture(t, "320x180", "libx264", "mp4")
	putPending(t, p, "Resolve export.mp4", body)
	settlePending(t, p, clock)
	item := onlyPending(t, p)
	if item.Kind != "video" || item.Reason != "missing_capture_time" {
		t.Fatalf("edited video: %+v", item)
	}
	h := newAPIHandler(p.store, testLogger(t))
	for _, path := range []string{item.URL, item.ThumbnailURL} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || w.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatalf("private video: %d %s", w.Code, w.Body)
		}
		if path == item.ThumbnailURL && w.Header().Get("Content-Type") != "image/jpeg" {
			t.Fatal("pending video preview is not JPEG")
		}
		if path == item.URL && !bytes.Equal(w.Body.Bytes(), body) {
			t.Fatal("pending original was transcoded during review")
		}
	}
	m := travelRequest[mediaItem](t, h, "POST", "/api/v1/admin/media/pending/"+item.ID+"/assign", map[string]int64{"dive_id": 42}, 200)
	if m.Kind != "video" || m.Capture != nil || m.ThumbnailStatus != "ready" {
		t.Fatalf("manual video: %+v", m)
	}
}

func TestPendingVideoEncoderFailureAndCommittedReceiptRecovery(t *testing.T) {
	p, _, _, clock := pendingTestService(t)
	processor := &countedVideoProcessor{Processor: mediaproc.NewProcessor(), fail: true}
	p.media.processor, p.files.processor = processor, processor
	body := appVideoFixture(t, "320x180", "libx264", "mov")
	putPending(t, p, "Recovery.mov", body)
	settlePending(t, p, clock)
	item := onlyPending(t, p)
	if item.Reason != "processing_error" || item.State != "assigning" {
		t.Fatalf("failed import: %+v", item)
	}
	if rows, _ := p.store.mediaForDive(context.Background(), 42); len(rows) != 0 {
		t.Fatal("failed import saved incomplete media")
	}
	if original, _ := p.files.root.ReadFile(item.Filename); !bytes.Equal(original, body) {
		t.Fatal("encoder failure deleted original")
	}
	processor.fail = false
	input, err := p.openSource(context.Background(), item, true)
	if err != nil {
		t.Fatal(err)
	}
	m, err := p.media.importPending(context.Background(), item, input)
	input.Close()
	if err != nil || m.ID != item.MediaID || m.Filename != "Recovery.mov.mp4" {
		t.Fatalf("retry: %+v %v", m, err)
	}
	// The SQL commit completed, but the inbox cleanup did not. A new file with
	// the same name must survive recovery of the older import.
	putPending(t, p, item.Filename, []byte("new camera bytes"))
	if count, err := p.Recover(context.Background()); err != nil || count != 1 {
		t.Fatalf("recovery: %d %v", count, err)
	}
	if original, _ := p.files.root.ReadFile(item.Filename); string(original) != "new camera bytes" {
		t.Fatal("recovery deleted replacement")
	}
	rows, err := p.store.mediaForDive(context.Background(), 42)
	if err != nil || len(rows) != 1 || rows[0].ID != m.ID || rows[0].Capture == nil {
		t.Fatal("recovery duplicated media or lost capture")
	}
}
