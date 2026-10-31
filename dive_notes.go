package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

var (
	diveNoteHeader = regexp.MustCompile(`(?i)^(n|commentaires?|hl)[\s\p{Zs}]*:[\s\p{Zs}]*(.*)$`)
	diveNoteRating = regexp.MustCompile(`(?i)^n\s*[:=]?\s*([0-9]+(?:[.,][0-9]+)?)\s*(?:/\s*([0-9]+(?:[.,][0-9]+)?))?\s*$`)
)

type parsedDiveNotes struct {
	Rating                               *float64
	RatingMax                            *float64
	Comment                              string
	Highlights                           []string
	hasRating, hasComment, hasHighlights bool
}

// Legacy Dive notes take precedence per section. The official Personal API
// carries the activity's free-text note in description. Explicitly empty
// sections in notes remain empty instead of inheriting another source's text.
func parseDiveNotes(notes, description string) parsedDiveNotes {
	result := parseDiveNote(notes)
	fallback := parseDiveNote(description)
	if !result.hasRating {
		result.Rating, result.RatingMax = fallback.Rating, fallback.RatingMax
	}
	if !result.hasComment {
		result.Comment = fallback.Comment
	}
	if !result.hasHighlights {
		result.Highlights = fallback.Highlights
	}
	return result
}

func parseDiveNote(text string) parsedDiveNotes {
	result := parsedDiveNotes{Highlights: []string{}}
	text = strings.TrimPrefix(text, "\ufeff")
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	var comments []string
	section := ""
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		// Nonbreaking spaces often appear in text copied from a French keyboard.
		headerText := strings.ReplaceAll(strings.ReplaceAll(trimmed, "\u00a0", " "), "\u202f", " ")
		if rating := diveNoteRating.FindStringSubmatch(headerText); rating != nil {
			result.hasRating = true
			result.Rating, result.RatingMax = parseDiveRating(rating[1], rating[2])
			section = ""
			continue
		}
		if header := diveNoteHeader.FindStringSubmatch(trimmed); header != nil {
			switch strings.ToLower(header[1]) {
			case "n":
				// A malformed explicit rating is unavailable, never zero or a
				// rating from the other source. Keep its raw text in notes.
				result.hasRating, result.Rating, result.RatingMax = true, nil, nil
				section = ""
			case "commentaire", "commentaires":
				result.hasComment, section = true, "comment"
				if len(comments) > 0 {
					comments = append(comments, "")
				}
				if value := strings.TrimSpace(header[2]); value != "" {
					comments = append(comments, value)
				}
			case "hl":
				result.hasHighlights, section = true, "highlights"
				if value := diveHighlight(header[2]); value != "" {
					result.Highlights = append(result.Highlights, value)
				}
			}
			continue
		}
		switch section {
		case "comment":
			comments = append(comments, line)
		case "highlights":
			if value := diveHighlight(line); value != "" {
				result.Highlights = append(result.Highlights, value)
			}
		}
	}
	result.Comment = strings.TrimSpace(strings.Join(comments, "\n"))
	return result
}

func parseDiveRating(score, maximum string) (*float64, *float64) {
	value, err := strconv.ParseFloat(strings.ReplaceAll(score, ",", "."), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return nil, nil
	}
	if maximum == "" {
		return &value, nil
	}
	limit, err := strconv.ParseFloat(strings.ReplaceAll(maximum, ",", "."), 64)
	if err != nil || math.IsNaN(limit) || math.IsInf(limit, 0) || limit <= 0 || value > limit {
		return nil, nil
	}
	return &value, &limit
}

func diveHighlight(line string) string {
	line = strings.TrimSpace(line)
	switch line {
	case "-", "*", "•", "–":
		return ""
	}
	for _, bullet := range []string{"- ", "* ", "• ", "– "} {
		if strings.HasPrefix(line, bullet) {
			return strings.TrimSpace(strings.TrimPrefix(line, bullet))
		}
	}
	return line
}

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
			parsed := parseDiveNotes(note.notes, note.description)
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
