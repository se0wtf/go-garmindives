package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"
)

const archiveVersion = 1

type Archive struct {
	Version int          `yaml:"version"`
	Dives   []DiveRecord `yaml:"dives"`
}

type DiveRecord struct {
	ID              int64 `yaml:"id"`
	Activity        any   `yaml:"activity"`
	ActivityDetails any   `yaml:"activity_details"`
	DiveDetails     any   `yaml:"dive_details"`
}

func newArchive() Archive {
	return Archive{Version: archiveVersion, Dives: []DiveRecord{}}
}

func loadArchive(path string) (Archive, bool, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return newArchive(), false, nil
	}
	if err != nil {
		return Archive{}, false, fmt.Errorf("open archive: %w", err)
	}
	defer file.Close()

	var archive Archive
	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	if err := decoder.Decode(&archive); err != nil {
		return Archive{}, true, fmt.Errorf("decode archive: %w", err)
	}
	if err := validateArchive(archive); err != nil {
		return Archive{}, true, err
	}
	if archive.Dives == nil {
		archive.Dives = []DiveRecord{}
	}
	return archive, true, nil
}

func saveArchive(path string, archive Archive) error {
	if err := validateArchive(archive); err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if _, err := os.Stat(directory); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return fmt.Errorf("create archive directory: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("inspect archive directory: %w", err)
	}

	temporary, err := os.CreateTemp(directory, ".go-garmindives-*.yaml")
	if err != nil {
		return fmt.Errorf("create temporary archive: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("secure temporary archive: %w", err)
	}
	encoder := yaml.NewEncoder(temporary)
	encoder.SetIndent(2)
	if err := encoder.Encode(archive); err != nil {
		encoder.Close()
		temporary.Close()
		return fmt.Errorf("encode archive: %w", err)
	}
	if err := encoder.Close(); err != nil {
		temporary.Close()
		return fmt.Errorf("close archive encoder: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync archive: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close archive: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace archive: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secure archive: %w", err)
	}
	return nil
}

func validateArchive(archive Archive) error {
	if archive.Version != archiveVersion {
		return fmt.Errorf("unsupported archive version %d", archive.Version)
	}
	seen := make(map[int64]struct{}, len(archive.Dives))
	for _, dive := range archive.Dives {
		if dive.ID <= 0 {
			return fmt.Errorf("invalid dive ID %d", dive.ID)
		}
		if _, exists := seen[dive.ID]; exists {
			return fmt.Errorf("duplicate dive ID %d", dive.ID)
		}
		seen[dive.ID] = struct{}{}
	}
	return nil
}
