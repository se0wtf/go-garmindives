package app

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	mediaproc "github.com/se0wtf/go-garmindives/internal/media"
)

type testCaptureReader struct {
	capture *mediaproc.CaptureTime
	err     error
	calls   atomic.Int32
}

func (r *testCaptureReader) ReadCapture(context.Context, *os.File) (*mediaproc.CaptureTime, error) {
	r.calls.Add(1)
	return r.capture, r.err
}

func pendingTestService(t *testing.T) (*pendingService, *fakeMediaProcessor, *testCaptureReader, *atomic.Int64) {
	t.Helper()
	library, processor := mediaTestLibrary(t)
	if _, err := library.store.db.Exec(`UPDATE dives SET start_time='2026-08-15T10:00:00',timezone='Pacific/Noumea',duration_seconds=3600 WHERE id=42`); err != nil {
		t.Fatal(err)
	}
	if _, err := library.store.db.Exec(`UPDATE dives SET start_time='2024-01-01T10:00:00' WHERE id=99`); err != nil {
		t.Fatal(err)
	}
	p, err := newPendingService(library, filepath.Join(library.directory, "pending"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	library.store.pending = p
	reader := &testCaptureReader{capture: &mediaproc.CaptureTime{LocalTime: "2026-08-15T10:30:00", Source: "ExifIFD:DateTimeOriginal"}}
	p.reader = reader
	clock := &atomic.Int64{}
	clock.Store(time.Now().UnixMilli())
	p.now = func() time.Time { return time.UnixMilli(clock.Load()) }
	return p, processor, reader, clock
}
func putPending(t *testing.T, p *pendingService, name string, body []byte) {
	t.Helper()
	if err := p.files.root.WriteFile(name, body, 0o600); err != nil {
		t.Fatal(err)
	}
}
func settlePending(t *testing.T, p *pendingService, clock *atomic.Int64) pendingScanResult {
	t.Helper()
	if _, err := p.Scan(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	clock.Add(3000)
	result, err := p.Scan(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func onlyPending(t *testing.T, p *pendingService) pendingItem {
	t.Helper()
	items, err := p.store.pendingItems(context.Background())
	if err != nil || len(items) != 1 {
		t.Fatalf("pending items: %+v %v", items, err)
	}
	return items[0]
}

func TestPendingAutomaticAssignmentKeepsCaptureAndUsesDiveLocalTime(t *testing.T) {
	p, _, reader, clock := pendingTestService(t)
	body := []byte("complete synthetic photo")
	putPending(t, p, "Reef.jpg", body)
	if result, err := p.Scan(context.Background(), false); err != nil || result.Assigned != 0 || reader.calls.Load() != 0 {
		t.Fatal("photo imported before settling")
	}
	clock.Add(3000)
	result, err := p.Scan(context.Background(), false)
	if err != nil || result.Assigned != 1 {
		t.Fatalf("assignment: %+v %v", result, err)
	}
	items, err := p.store.mediaForDive(context.Background(), 42)
	if err != nil || len(items) != 1 {
		t.Fatalf("assigned media: %+v %v", items, err)
	}
	capture := items[0].Capture
	if capture == nil || capture.CapturedAt != "2026-08-14T23:30:00Z" || capture.LocalTime != "2026-08-15T10:30:00" || capture.Timezone != "Pacific/Noumea" || !capture.TimezoneAssumed {
		t.Fatalf("capture provenance: %+v", capture)
	}
	if _, err := p.files.root.Stat("Reef.jpg"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("pending original remained after complete assignment")
	}
	if actual, err := p.media.root.ReadFile("42/Reef.jpg"); err != nil || !bytes.Equal(actual, body) {
		t.Fatal("assigned photo bytes changed")
	}
	if _, err := p.media.Scan(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	stored, err := p.store.mediaByID(context.Background(), 42, items[0].ID)
	if err != nil || stored.Capture == nil || stored.Capture.CapturedAt != capture.CapturedAt {
		t.Fatal("ordinary rescan discarded capture metadata")
	}
}

func TestPendingAmbiguityMissingDatesAndLaterDiveSync(t *testing.T) {
	t.Run("overlap", func(t *testing.T) {
		p, _, _, clock := pendingTestService(t)
		if _, err := p.store.db.Exec(`UPDATE dives SET start_time='2026-08-15T10:30:00',timezone='Pacific/Noumea',duration_seconds=3600 WHERE id=99`); err != nil {
			t.Fatal(err)
		}
		putPending(t, p, "Overlap.jpg", []byte("photo"))
		settlePending(t, p, clock)
		item := onlyPending(t, p)
		if item.Reason != "multiple_matches" || len(item.Candidates) != 2 {
			t.Fatalf("ambiguous match: %+v", item)
		}
		m, err := p.Assign(context.Background(), item.ID, 99)
		if err != nil || m.DiveID != 99 {
			t.Fatalf("manual assignment: %+v %v", m, err)
		}
	})
	t.Run("missing date", func(t *testing.T) {
		p, _, reader, clock := pendingTestService(t)
		reader.capture = nil
		putPending(t, p, "NoDate.jpg", []byte("photo"))
		settlePending(t, p, clock)
		item := onlyPending(t, p)
		if item.Reason != "missing_capture_time" {
			t.Fatal(item.Reason)
		}
		if _, err := p.Scan(context.Background(), false); err != nil {
			t.Fatal(err)
		}
		if reader.calls.Load() != 1 {
			t.Fatal("unchanged missing metadata was re-read")
		}
		if m, err := p.Assign(context.Background(), item.ID, 42); err != nil || m.Capture != nil {
			t.Fatalf("manual date-free assignment: %+v %v", m, err)
		}
	})
	t.Run("later sync", func(t *testing.T) {
		p, _, reader, clock := pendingTestService(t)
		reader.capture.LocalTime = "2026-08-16T10:30:00"
		putPending(t, p, "Later.jpg", []byte("photo"))
		settlePending(t, p, clock)
		if item := onlyPending(t, p); item.Reason != "no_matching_dive" {
			t.Fatal(item.Reason)
		}
		if _, err := p.store.db.Exec(`UPDATE dives SET start_time='2026-08-16T10:00:00' WHERE id=42`); err != nil {
			t.Fatal(err)
		}
		result, err := p.Scan(context.Background(), false)
		if err != nil || result.Assigned != 1 || reader.calls.Load() != 1 {
			t.Fatalf("new dive match: %+v calls=%d %v", result, reader.calls.Load(), err)
		}
	})
}

func TestPendingConflictAndProcessingFailurePreserveOriginal(t *testing.T) {
	t.Run("filename", func(t *testing.T) {
		p, _, _, clock := pendingTestService(t)
		writeTestMedia(t, p.media, "42/Reef.jpg", []byte("existing photo"))
		putPending(t, p, "Reef.jpg", []byte("new photo"))
		settlePending(t, p, clock)
		item := onlyPending(t, p)
		if item.Reason != "filename_conflict" {
			t.Fatalf("conflict: %+v", item)
		}
		if actual, _ := p.media.root.ReadFile("42/Reef.jpg"); string(actual) != "existing photo" {
			t.Fatal("destination overwritten")
		}
		if err := p.Delete(context.Background(), item.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := p.files.root.Stat("Reef.jpg"); !errors.Is(err, fs.ErrNotExist) {
			t.Fatal("pending deletion failed")
		}
	})
	t.Run("retry", func(t *testing.T) {
		p, processor, _, clock := pendingTestService(t)
		processor.fail = true
		putPending(t, p, "Fail.jpg", []byte("retryable original"))
		settlePending(t, p, clock)
		item := onlyPending(t, p)
		if item.Reason != "processing_error" || item.State != "assigning" {
			t.Fatalf("failure: %+v", item)
		}
		if actual, _ := p.files.root.ReadFile("Fail.jpg"); string(actual) != "retryable original" {
			t.Fatal("failed conversion deleted pending original")
		}
		if items, _ := p.store.mediaForDive(context.Background(), 42); len(items) != 0 {
			t.Fatal("failed processing published incomplete media")
		}
		processor.fail = false
		count, err := p.Recover(context.Background())
		if err != nil || count != 1 {
			t.Fatalf("retry: %d %v", count, err)
		}
		items, _ := p.store.mediaForDive(context.Background(), 42)
		if len(items) != 1 || items[0].ID != item.MediaID {
			t.Fatal("retry changed reserved media identity")
		}
		if count, err := p.Recover(context.Background()); err != nil || count != 0 {
			t.Fatal("completed receipt duplicated media")
		}
	})
}

func TestPendingInterruptedCommitAndChangedSourceRecovery(t *testing.T) {
	for _, changed := range []bool{false, true} {
		name := "unchanged"
		if changed {
			name = "replaced source"
		}
		t.Run(name, func(t *testing.T) {
			p, _, reader, clock := pendingTestService(t)
			reader.capture = nil
			original := []byte("original photo")
			putPending(t, p, "Recovery.jpg", original)
			settlePending(t, p, clock)
			item := onlyPending(t, p)
			if err := p.media.reservePending(context.Background(), &item, 42, nil); err != nil {
				t.Fatal(err)
			}
			input, err := p.openSource(context.Background(), item, true)
			if err != nil {
				t.Fatal(err)
			}
			m, err := p.media.importPending(context.Background(), item, input)
			input.Close()
			if err != nil {
				t.Fatal(err)
			}
			if changed {
				putPending(t, p, "Recovery.jpg", []byte("new user bytes"))
			}
			count, err := p.Recover(context.Background())
			if err != nil || count != 1 {
				t.Fatalf("commit recovery: %d %v", count, err)
			}
			items, _ := p.store.mediaForDive(context.Background(), 42)
			if len(items) != 1 || items[0].ID != m.ID {
				t.Fatal("interrupted commit duplicated or replaced media")
			}
			if changed {
				actual, err := p.files.root.ReadFile("Recovery.jpg")
				if err != nil || string(actual) != "new user bytes" {
					t.Fatal("cleanup deleted a newer pending file")
				}
			} else if _, err := p.files.root.Stat("Recovery.jpg"); !errors.Is(err, fs.ErrNotExist) {
				t.Fatal("completed source not cleaned up")
			}
		})
	}
}

func TestPendingOrdinaryScanRecoversReservedID(t *testing.T) {
	p, _, reader, clock := pendingTestService(t)
	reader.capture = nil
	body := []byte("published before SQL commit")
	putPending(t, p, "BeforeSQL.jpg", body)
	settlePending(t, p, clock)
	item := onlyPending(t, p)
	capture := &photoCapture{LocalTime: "2026-08-15T10:30:00", Source: "test"}
	if err := p.media.reservePending(context.Background(), &item, 42, capture); err != nil {
		t.Fatal(err)
	}
	writeTestMedia(t, p.media, "42/BeforeSQL.jpg", body)
	if _, err := p.media.Scan(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	m, err := p.store.mediaByID(context.Background(), 42, item.MediaID)
	if err != nil || m.Capture == nil {
		t.Fatalf("ordinary scan lost receipt: %+v %v", m, err)
	}
	if count, err := p.Recover(context.Background()); err != nil || count != 1 {
		t.Fatalf("receipt recovery: %d %v", count, err)
	}
}

func TestPendingRealPhotoPipelineAndPrivatePreview(t *testing.T) {
	processor := photoTestTools(t)
	p, _, reader, clock := pendingTestService(t)
	p.media.processor, p.files.processor = processor, processor
	reader.capture = nil
	body := photoPNG(t, 2048, 2048, false)
	putPending(t, p, "Coral.png", body)
	settlePending(t, p, clock)
	item := onlyPending(t, p)
	handler := newAPIHandler(p.store, testLogger(t))
	w := mediaRequest(t, handler, "GET", item.ThumbnailURL, nil, "")
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/jpeg" || w.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("private preview: %d %v", w.Code, w.Header())
	}
	m, err := p.Assign(context.Background(), item.ID, 42)
	if err != nil || m.Filename != "Coral.png.jxl" || min(m.Width, m.Height) != 1080 || m.ThumbnailStatus != "ready" {
		t.Fatalf("JXL import: %+v %v", m, err)
	}
	if _, err := p.files.root.Stat("Coral.png"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("pending original not retired")
	}
}

func TestPendingWorkerFindsFilesAndStops(t *testing.T) {
	p, _, _, clock := pendingTestService(t)
	putPending(t, p, "Worker.jpg", []byte("photo"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		items, _ := p.store.pendingItems(context.Background())
		if len(items) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	clock.Add(3000)
	p.Signal(false)
	for time.Now().Before(deadline) {
		items, _ := p.store.mediaForDive(context.Background(), 42)
		if len(items) == 1 {
			p.Close()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("worker did not assign the pending file")
}

func TestPendingRealHEICCaptureAndConversion(t *testing.T) {
	processor := photoTestTools(t)
	reader := mediaproc.NewCaptureReader()
	if reader.Path == "" || mediaproc.NewProcessor().HEIF == "" {
		t.Skip("install exiftool and libheif")
	}
	p, _, _, clock := pendingTestService(t)
	p.reader = reader
	p.media.processor, p.files.processor = processor, processor
	body, err := os.ReadFile("../media/testdata/grid-rotated.heic")
	if err != nil {
		t.Fatal(err)
	}
	putPending(t, p, "Camera.HEIC", body)
	cmd := exec.Command(reader.Path, "-config", "", "-overwrite_original", "-DateTimeOriginal=2026:08:15 10:30:00", filepath.Join(p.directory, "Camera.HEIC"))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("write synthetic capture time: %s %v", output, err)
	}
	if result := settlePending(t, p, clock); result.Assigned != 1 {
		t.Fatalf("real HEIC import: %+v", result)
	}
	items, err := p.store.mediaForDive(context.Background(), 42)
	if err != nil || len(items) != 1 || items[0].Capture == nil || items[0].Capture.CapturedAt != "2026-08-14T23:30:00Z" || items[0].Width != 1080 || items[0].Height != 1620 || items[0].Filename != "Camera.HEIC.jxl" {
		t.Fatalf("real metadata/JXL pipeline: %+v %v", items, err)
	}
}

func TestPendingRejectsUnsafeDirectoriesAndSourceReplacement(t *testing.T) {
	p, _, reader, clock := pendingTestService(t)
	reader.capture = nil
	for _, directory := range []string{p.media.directory, filepath.Dir(p.media.directory), filepath.Join(p.media.directory, "42"), filepath.Join(p.media.directory, "trips"), filepath.Join(p.media.directory, ".thumbnails")} {
		if extra, err := newPendingService(p.media, directory, time.Minute); err == nil {
			extra.Close()
			t.Fatalf("unsafe pending directory accepted: %s", directory)
		}
	}
	outside := filepath.Join(t.TempDir(), "keep.jpg")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.files.root.Symlink(outside, "Escape.jpg"); err != nil {
		t.Fatal(err)
	}
	putPending(t, p, "Change.jpg", []byte("old bytes"))
	settlePending(t, p, clock)
	item := onlyPending(t, p)
	putPending(t, p, "Change.jpg", []byte("newer source bytes"))
	if err := p.Delete(context.Background(), item.ID); !errors.Is(err, errPendingChanged) {
		t.Fatalf("replaced source delete: %v", err)
	}
	if actual, _ := p.files.root.ReadFile("Change.jpg"); string(actual) != "newer source bytes" {
		t.Fatal("replacement was deleted")
	}
	if actual, _ := os.ReadFile(outside); string(actual) != "outside" {
		t.Fatal("symlink import changed external file")
	}
}

func exercisePendingPostgres(t *testing.T, store *sqlStore) {
	t.Helper()
	ctx := context.Background()
	if err := store.SaveDives(ctx, []DiveRecord{testSQLDive(t, 9012)}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE dives SET start_time='2035-06-20T10:00:00',timezone='Pacific/Noumea',duration_seconds=3600 WHERE id=9012`); err != nil {
		t.Fatal(err)
	}
	library, err := openMediaLibrary(store, t.TempDir(), 1<<20, testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	defer library.Close()
	library.processor = &fakeMediaProcessor{}
	p, err := newPendingService(library, filepath.Join(library.directory, "pending"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	previousMedia, previousPending := store.media, store.pending
	store.media, store.pending = library, p
	defer func() { store.media, store.pending = previousMedia, previousPending }()
	p.reader = &testCaptureReader{capture: &mediaproc.CaptureTime{LocalTime: "2035-06-20T10:30:00", Source: "test"}}
	clock := &atomic.Int64{}
	clock.Store(time.Now().UnixMilli())
	p.now = func() time.Time { return time.UnixMilli(clock.Load()) }
	putPending(t, p, "Postgres.jpg", []byte("postgres inbox photo"))
	if result := settlePending(t, p, clock); result.Assigned != 1 {
		t.Fatalf("PostgreSQL assignment: %+v", result)
	}
	items, err := store.mediaForDive(ctx, 9012)
	if err != nil || len(items) != 1 || items[0].Capture == nil || items[0].Capture.CapturedAt != "2035-06-19T23:30:00Z" {
		t.Fatalf("PostgreSQL capture: %+v %v", items, err)
	}
	p.reader = &testCaptureReader{}
	putPending(t, p, "Review.jpg", []byte("manual review"))
	settlePending(t, p, clock)
	response := travelRequest[pendingListResponse](t, newAPIHandler(store, testLogger(t)), "GET", "/api/v1/admin/media/pending?reason=missing_capture_time", nil, 200)
	if len(response.Items) != 1 {
		t.Fatal("PostgreSQL pending listing failed")
	}
	if err := p.Delete(ctx, response.Items[0].ID); err != nil {
		t.Fatal(err)
	}
	p.reader = &testCaptureReader{capture: &mediaproc.CaptureTime{LocalTime: "2035-06-20T10:30:00", Source: "QuickTime:CreateDate"}}
	putPending(t, p, "Postgres.mp4", []byte("postgres video"))
	if result := settlePending(t, p, clock); result.Assigned != 1 {
		t.Fatalf("PostgreSQL video assignment: %+v", result)
	}
	items, err = store.mediaForDive(ctx, 9012)
	if err != nil || len(items) != 2 {
		t.Fatalf("PostgreSQL video items: %+v %v", items, err)
	}
	found := false
	for _, m := range items {
		if m.Kind == "video" && m.Capture != nil {
			found = true
		}
	}
	if !found {
		t.Fatal("PostgreSQL video capture missing")
	}
}
