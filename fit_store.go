package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

const fitBlobMigrationBatchSize = 16

var errNoStoredFIT = errors.New("dive has no stored FIT files")

type storedFITFile struct {
	Index     int
	Name      string
	SizeBytes int64
	SHA256    string
	Data      []byte
}

func prepareFITFiles(files []fitFile) ([]storedFITFile, error) {
	if len(files) == 0 || len(files) > maxFITFiles {
		return nil, errors.New("invalid number of original FIT files")
	}
	result := make([]storedFITFile, 0, len(files))
	remaining := maxFITBytes
	for index, file := range files {
		if file.Name == "" || file.OriginalBase64 == "" {
			return nil, fmt.Errorf("original FIT file %d is missing its name or bytes", index)
		}
		// Reject oversized data before allocating its decoded representation.
		if len(file.OriginalBase64) > base64.StdEncoding.EncodedLen(remaining) {
			return nil, errors.New("original FIT files exceed 128 MiB")
		}
		data, err := base64.StdEncoding.DecodeString(file.OriginalBase64)
		if err != nil {
			// Do not echo source bytes or base64 content in an error.
			return nil, fmt.Errorf("original FIT file %d contains invalid base64", index)
		}
		if len(data) == 0 || len(data) > remaining {
			return nil, fmt.Errorf("original FIT file %d is empty or exceeds the size limit", index)
		}
		remaining -= len(data)
		sum := sha256.Sum256(data)
		result = append(result, storedFITFile{
			Index: index, Name: file.Name, SizeBytes: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), Data: data,
		})
	}
	return result, nil
}

func (s *sqlStore) insertFITFiles(ctx context.Context, tx *sql.Tx, diveID int64, files []storedFITFile) error {
	query := s.bind(`INSERT INTO dive_fit_files (dive_id, file_index, filename, size_bytes, sha256, data)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (dive_id, file_index) DO NOTHING`)
	for _, file := range files {
		if _, err := tx.ExecContext(ctx, query, diveID, file.Index, file.Name, file.SizeBytes, file.SHA256, file.Data); err != nil {
			return fmt.Errorf("store original FIT file %d for dive %d: %w", file.Index, diveID, err)
		}
	}
	return nil
}

// migrateFITFiles copies already retained bytes from fit_json. It does not
// decode FIT messages or contact Garmin, and shares the migration transaction.
func migrateFITFiles(ctx context.Context, s *sqlStore, tx *sql.Tx) error {
	binaryType := "BLOB"
	if s.dialect == outputPostgres {
		binaryType = "BYTEA"
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE dive_fit_files (
		dive_id BIGINT NOT NULL REFERENCES dives(id) ON DELETE CASCADE,
		file_index INTEGER NOT NULL CHECK (file_index >= 0),
		filename TEXT NOT NULL,
		size_bytes BIGINT NOT NULL CHECK (size_bytes > 0 AND size_bytes = length(data)),
		sha256 TEXT NOT NULL CHECK (length(sha256) = 64),
		data `+binaryType+` NOT NULL,
		stored_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (dive_id, file_index)
	)`); err != nil {
		return fmt.Errorf("create original FIT file table: %w", err)
	}
	var lastID int64
	for {
		rows, err := tx.QueryContext(ctx, s.bind(`SELECT id, fit_json FROM dives
			WHERE id > ? AND fit_json <> 'null' ORDER BY id LIMIT ?`), lastID, fitBlobMigrationBatchSize)
		if err != nil {
			return fmt.Errorf("read existing FIT documents: %w", err)
		}
		type originalDocument struct {
			ID    int64
			Files []fitFile `json:"files"`
		}
		batch := make([]originalDocument, 0, fitBlobMigrationBatchSize)
		for rows.Next() {
			var document originalDocument
			var raw string
			if err := rows.Scan(&document.ID, &raw); err != nil {
				rows.Close()
				return fmt.Errorf("read existing FIT document: %w", err)
			}
			// Only retain file envelopes, not thousands of decoded record messages.
			var envelope struct {
				Files []struct {
					Name           string `json:"name"`
					OriginalBase64 string `json:"original_base64"`
				} `json:"files"`
			}
			if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
				rows.Close()
				return fmt.Errorf("read original FIT bytes for dive %d: invalid FIT JSON", document.ID)
			}
			for _, file := range envelope.Files {
				document.Files = append(document.Files, fitFile{Name: file.Name, OriginalBase64: file.OriginalBase64})
			}
			batch = append(batch, document)
			lastID = document.ID
		}
		readErr := rows.Err()
		closeErr := rows.Close()
		if readErr != nil {
			return fmt.Errorf("read existing FIT documents: %w", readErr)
		}
		if closeErr != nil {
			return closeErr
		}
		// Close the cursor before writing on the transaction's connection, which
		// also supports PostgreSQL without buffering the entire database.
		for _, document := range batch {
			files, err := prepareFITFiles(document.Files)
			if err != nil {
				return fmt.Errorf("migrate original FIT bytes for dive %d: %w", document.ID, err)
			}
			if err := s.insertFITFiles(ctx, tx, document.ID, files); err != nil {
				return err
			}
		}
		if len(batch) < fitBlobMigrationBatchSize {
			return nil
		}
	}
}

// FITFiles loads the original bytes directly from the database. Future features
// can use these independently of the decoded JSON document or a Garmin session.
func (s *sqlStore) FITFiles(ctx context.Context, diveID int64) ([]storedFITFile, error) {
	exists, err := s.diveExists(ctx, diveID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, errDiveNotFound
	}
	rows, err := s.db.QueryContext(ctx, s.bind(`SELECT file_index, filename, size_bytes, sha256, data
		FROM dive_fit_files WHERE dive_id = ? ORDER BY file_index`), diveID)
	if err != nil {
		return nil, fmt.Errorf("load original FIT files for dive %d: %w", diveID, err)
	}
	defer rows.Close()
	result := make([]storedFITFile, 0)
	for rows.Next() {
		var file storedFITFile
		if err := rows.Scan(&file.Index, &file.Name, &file.SizeBytes, &file.SHA256, &file.Data); err != nil {
			return nil, fmt.Errorf("read original FIT file for dive %d: %w", diveID, err)
		}
		sum := sha256.Sum256(file.Data)
		if int64(len(file.Data)) != file.SizeBytes || hex.EncodeToString(sum[:]) != file.SHA256 {
			return nil, fmt.Errorf("original FIT file %d for dive %d failed its size or checksum check", file.Index, diveID)
		}
		result = append(result, file)
	}
	return result, rows.Err()
}

func (s *sqlStore) DecodeStoredFIT(ctx context.Context, diveID int64) (*fitDocument, error) {
	files, err := s.FITFiles(ctx, diveID)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("%w: %d", errNoStoredFIT, diveID)
	}
	document := &fitDocument{Version: fitDocumentVersion, Files: make([]fitFile, 0, len(files))}
	for _, file := range files {
		decoded, err := decodeFITFile(ctx, file.Name, file.Data)
		if err != nil {
			return nil, fmt.Errorf("decode stored FIT file %d for dive %d: %w", file.Index, diveID, err)
		}
		document.Files = append(document.Files, decoded)
	}
	return document, nil
}
