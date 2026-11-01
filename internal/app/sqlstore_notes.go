package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	notedata "github.com/se0wtf/go-garmindives/internal/notes"
)

const diveNoteMigrationBatchSize = 100

// Backfill stored notes without re-downloading dives. Close each batch's cursor
// before writing on the transaction connection, for SQLite and PostgreSQL.
func migrateDiveNotes(ctx context.Context, store *sqlStore, tx *sql.Tx) error {
	var lastID int64
	for {
		rows, err := tx.QueryContext(ctx, store.bind(`SELECT id, notes, description FROM dives
			WHERE id > ? ORDER BY id LIMIT ?`), lastID, diveNoteMigrationBatchSize)
		if err != nil {
			return err
		}
		type storedNote struct {
			id                 int64
			notes, description string
		}
		batch := make([]storedNote, 0, diveNoteMigrationBatchSize)
		for rows.Next() {
			var note storedNote
			if err := rows.Scan(&note.id, &note.notes, &note.description); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, note)
			lastID = note.id
		}
		readErr, closeErr := rows.Err(), rows.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		for _, note := range batch {
			parsed := notedata.Parse(note.notes, note.description)
			highlights, err := json.Marshal(parsed.Highlights)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, store.bind(`UPDATE dives SET rating = ?, rating_max = ?,
				note_comment = ?, highlights_json = ? WHERE id = ?`),
				parsed.Rating, parsed.RatingMax, parsed.Comment, string(highlights), note.id); err != nil {
				return fmt.Errorf("parse stored notes for dive %d: %w", note.id, err)
			}
		}
		if len(batch) < diveNoteMigrationBatchSize {
			return nil
		}
	}
}
