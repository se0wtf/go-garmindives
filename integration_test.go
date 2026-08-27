package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	garmin "github.com/se0wtf/go-garminconnect"
)

func TestGarminDiveIntegration(t *testing.T) {
	email, password := os.Getenv("GARMIN_EMAIL"), os.Getenv("GARMIN_PASSWORD")
	if email == "" || password == "" {
		t.Skip("set GARMIN_EMAIL and GARMIN_PASSWORD to run the live integration test")
	}
	directory := t.TempDir()
	tokenPath := filepath.Join(directory, "tokens", "tokens.json")
	mfa := func(context.Context) (string, error) { return os.Getenv("GARMIN_MFA_CODE"), nil }
	client, err := garmin.Login(context.Background(), email, password, mfa, garmin.WithTokenFile(tokenPath))
	if err != nil {
		t.Fatal(err)
	}
	archive := newArchive()
	added, err := syncDives(context.Background(), client, &archive, nil)
	if err != nil {
		t.Fatal(err)
	}
	if added == 0 {
		t.Fatal("live account returned no dives")
	}
	archivePath := filepath.Join(directory, "dives.yaml")
	if err := saveArchive(archivePath, archive); err != nil {
		t.Fatal(err)
	}
	loaded, existed, err := loadArchive(archivePath)
	if err != nil || !existed || len(loaded.Dives) != added {
		t.Fatalf("loaded %d dives, existed=%v, err=%v", len(loaded.Dives), existed, err)
	}
	secondAdded, err := syncDives(context.Background(), client, &loaded, nil)
	if err != nil || secondAdded != 0 {
		t.Fatalf("second sync added %d dives: %v", secondAdded, err)
	}
}
