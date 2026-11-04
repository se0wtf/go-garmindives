package app

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
)

func TestPhotoRecoveryAcrossSQLCommit(t *testing.T) {
	for _, inPlace := range []bool{false, true} {
		for _, committed := range []bool{false, true} {
			name := "separate-target"
			if inPlace {
				name = "in-place"
			}
			if committed {
				name += "/committed"
			} else {
				name += "/uncommitted"
			}
			t.Run(name, func(t *testing.T) {
				library, _ := mediaTestLibrary(t)
				ctx := context.Background()
				source := "Reef.png"
				if inPlace {
					source = "Reef.jxl"
				}
				original := []byte("source photo bytes")
				old, err := library.Upload(ctx, 42, source, bytes.NewReader(original), nil)
				if err != nil {
					t.Fatal(err)
				}
				m := old
				m.Filename, m.ContentType = convertedPhotoFilename(source), "image/jxl"
				m.ThumbnailKey, m.ThumbnailStatus = strings.Repeat("a", 64), "ready"
				m.setPhotoState(photoState{Version: photoProfileVersion, SourceFilename: source, SourceSHA256: old.SHA256})
				encoded := []byte("verified encoded photo")
				writeTestMedia(t, library, ".uploads/encoded", encoded)
				m.SHA256, err = library.hashPhotoPath(ctx, ".uploads/encoded")
				if err != nil {
					t.Fatal(err)
				}
				writeTestMedia(t, library, m.thumbnailPath(), []byte("verified thumbnail"))
				job, err := library.publishPhoto(ctx, old.owner(), source, old.SHA256, ".uploads/encoded", m, old)
				if err != nil {
					t.Fatal(err)
				}
				if committed {
					if err := library.store.saveMedia(ctx, m); err != nil {
						t.Fatal(err)
					}
				}
				// Reopen the filesystem root to simulate startup following interruption.
				reopened, err := openMediaLibrary(library.store, library.directory, library.maxUpload, testLogger(t))
				if err != nil {
					t.Fatal(err)
				}
				defer reopened.Close()
				if err := reopened.recoverPhotoJobs(ctx); err != nil {
					t.Fatal(err)
				}
				path, expected := old.owner().path(source), original
				if committed {
					path, expected = m.owner().path(m.Filename), encoded
				}
				actual, err := reopened.root.ReadFile(path)
				if err != nil || !bytes.Equal(actual, expected) {
					t.Fatalf("recovery lost retained bytes: %q %v", actual, err)
				}
				if !inPlace {
					absent := m.owner().path(m.Filename)
					if committed {
						absent = old.owner().path(source)
					}
					if _, err := reopened.root.Stat(absent); !errors.Is(err, fs.ErrNotExist) {
						t.Fatalf("unexpected recovery leftover: %s", absent)
					}
				}
				if _, err := reopened.root.Stat(job.path()); !errors.Is(err, fs.ErrNotExist) {
					t.Fatal("recovery journal not retired")
				}
				if job.Backup != "" {
					if _, err := reopened.root.Stat(job.Backup); !errors.Is(err, fs.ErrNotExist) {
						t.Fatal("recovery backup not retired")
					}
				}
			})
		}
	}
}

func TestPhotoRetirementRetainsChangedSource(t *testing.T) {
	library, _ := mediaTestLibrary(t)
	writeTestMedia(t, library, "42/Reef.png", []byte("new copy supplied by user"))
	err := library.retirePhoto(context.Background(), "42/Reef.png", strings.Repeat("0", 64))
	if !errors.Is(err, errPhotoSourceChanged) {
		t.Fatalf("changed source: %v", err)
	}
	actual, err := library.root.ReadFile("42/Reef.png")
	if err != nil || string(actual) != "new copy supplied by user" {
		t.Fatal("changed source was deleted")
	}
}

func TestPhotoRecoveryRejectsEscapingJournal(t *testing.T) {
	library, _ := mediaTestLibrary(t)
	id := strings.Repeat("a", 32)
	journal := `{"id":"` + id + `","owner":{"DiveID":42},"source":"42/../99/Keep.png","target":"42/Reef.png.jxl","source_sha":"` + strings.Repeat("a", 64) + `","target_sha":"` + strings.Repeat("b", 64) + `","thumbnail":"` + strings.Repeat("c", 64) + `"}`
	writeTestMedia(t, library, "99/Keep.png", []byte("keep"))
	writeTestMedia(t, library, ".photo-jobs/"+id+".json", []byte(journal))
	if err := library.recoverPhotoJobs(context.Background()); err == nil {
		t.Fatal("invalid recovery path accepted")
	}
	if actual, err := library.root.ReadFile("99/Keep.png"); err != nil || string(actual) != "keep" {
		t.Fatal("invalid journal changed unrelated file")
	}
}
