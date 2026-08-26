package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArchiveRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "dives.yaml")
	want := newArchive()
	want.Dives = append(want.Dives, DiveRecord{
		ID:              42,
		Activity:        map[string]any{"activityName": "Lagoon"},
		ActivityDetails: map[string]any{"samples": []any{1.0, 2.0}},
		DiveDetails:     map[string]any{"maxDepth": 20.5},
	})
	if err := saveArchive(path, want); err != nil {
		t.Fatal(err)
	}
	got, existed, err := loadArchive(path)
	if err != nil {
		t.Fatal(err)
	}
	if !existed || len(got.Dives) != 1 || got.Dives[0].ID != 42 {
		t.Fatalf("loadArchive() = %#v, existed=%v", got, existed)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if gotMode := info.Mode().Perm(); gotMode != 0o600 {
		t.Fatalf("archive mode = %o, want 600", gotMode)
	}
}

func TestLoadMissingArchive(t *testing.T) {
	archive, existed, err := loadArchive(filepath.Join(t.TempDir(), "dives.yaml"))
	if err != nil || existed || archive.Version != archiveVersion || archive.Dives == nil {
		t.Fatalf("loadArchive() = %#v, existed=%v, err=%v", archive, existed, err)
	}
}

func TestLoadArchiveValidation(t *testing.T) {
	tests := []struct {
		name, contents, contains string
	}{
		{"malformed", "version: [", "decode archive"},
		{"version", "version: 2\ndives: []\n", "unsupported archive version"},
		{"invalid ID", "version: 1\ndives:\n  - id: 0\n", "invalid dive ID"},
		{"duplicate", "version: 1\ndives:\n  - id: 1\n  - id: 1\n", "duplicate dive ID"},
		{"unknown field", "version: 1\ndives: []\nother: true\n", "field other not found"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "dives.yaml")
			if err := os.WriteFile(path, []byte(test.contents), 0o600); err != nil {
				t.Fatal(err)
			}
			_, _, err := loadArchive(path)
			if err == nil || !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("error = %v, want containing %q", err, test.contains)
			}
		})
	}
}

func TestSaveArchiveValidation(t *testing.T) {
	if err := saveArchive(filepath.Join(t.TempDir(), "dives.yaml"), Archive{}); err == nil {
		t.Fatal("expected unsupported version error")
	}
}
